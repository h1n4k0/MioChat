package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

type Event struct {
	Preferences bool
	Message     *Message
	Presence    bool
	Refresh     bool
	Notice      string
	NoticeKind  string
	MoveRoom    string
	ChangedID   int64
}
type Client struct {
	Name, Room, Peer string
	Events           chan Event
	Close            func()
	closeOnce        sync.Once
	ID, IP, Version  string
	AuthEpoch        int64
	Ctx              context.Context
}
type Hub struct {
	mu                 sync.Mutex
	clients            map[*Client]bool
	joining            map[*Client]bool
	authLimits         authLimiter
	registrationLimits authLimiter
	AuthSlots          chan struct{}
	Started            time.Time
	store              *Store
	attachOnce         sync.Once
	lifeMu             sync.Mutex
	stopping           bool
	uiTasks            sync.WaitGroup
}

func (h *Hub) beginUI() bool {
	h.lifeMu.Lock()
	defer h.lifeMu.Unlock()
	if h.stopping {
		return false
	}
	h.uiTasks.Add(1)
	return true
}
func (h *Hub) stopUI() { h.lifeMu.Lock(); h.stopping = true; h.lifeMu.Unlock() }

func newHub() *Hub {
	return &Hub{clients: make(map[*Client]bool), joining: make(map[*Client]bool), AuthSlots: make(chan struct{}, 2), Started: time.Now()}
}
func (h *Hub) authAllowed(ip string, register bool) bool {
	key := ip
	window := time.Minute
	limit := 15
	if register {
		key = "register/" + ip
		window = time.Hour
		limit = 10
		if h.store != nil {
			if n, e := strconv.Atoi(h.store.Config("registration_quota")); e == nil && n > 0 {
				limit = n
			}
		}
	}
	if register {
		return h.registrationLimits.allow(key, time.Now(), window, limit)
	}
	return h.authLimits.allow(key, time.Now(), window, limit)
}
func (h *Hub) Join(c *Client) error {
	h.mu.Lock()
	if len(h.clients)+len(h.joining) >= 64 {
		h.mu.Unlock()
		return errors.New("大厅已满，请稍后再来")
	}
	if h.clients[c] || h.joining[c] {
		h.mu.Unlock()
		return errors.New("此会话已经加入")
	}
	count := 0
	for x := range h.clients {
		if x.Name == c.Name {
			count++
		}
	}
	for x := range h.joining {
		if x.Name == c.Name {
			count++
		}
	}
	if count >= 3 {
		h.mu.Unlock()
		return errors.New("同一账户最多同时登录 3 个会话")
	}
	h.joining[c] = true
	h.mu.Unlock()
	c.Room = "lobby"
	var err error
	if h.store != nil {
		err = h.store.RecordSession(c)
	}
	h.mu.Lock()
	delete(h.joining, c)
	h.lifeMu.Lock()
	stopping := h.stopping
	h.lifeMu.Unlock()
	if err != nil || stopping || (c.Ctx != nil && c.Ctx.Err() != nil) {
		h.mu.Unlock()
		if err == nil && h.store != nil {
			h.store.EndSession(c.ID)
		}
		if err != nil {
			return err
		}
		return errors.New("连接已结束，请重新登录")
	}
	h.clients[c] = true
	h.presenceLocked()
	h.mu.Unlock()
	return nil
}
func (h *Hub) Leave(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.presenceLocked()
	h.mu.Unlock()
	if h.store != nil {
		h.store.EndSession(c.ID)
	}
}
func (h *Hub) emitLocked(c *Client, e Event) {
	select {
	case c.Events <- e:
	default:
		c.closeOnce.Do(func() { go c.Close() })
	}
}
func (h *Hub) presenceLocked() {
	for c := range h.clients {
		if len(c.Events) == 0 {
			h.emitLocked(c, Event{Presence: true})
		}
	}
}

func (h *Hub) Attach(s *Store) { h.attachOnce.Do(func() { h.store = s }) }
func (h *Hub) Refresh() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		h.emitLocked(c, Event{Refresh: true})
	}
}
func (h *Hub) PreferencesChanged(user string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.Name == user {
			h.emitLocked(c, Event{Preferences: true})
		}
	}
}
func (h *Hub) RemoveRoomUser(name, room string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.Name == name && c.Room == room && c.Peer == "" {
			c.Room = "lobby"
			h.emitLocked(c, Event{MoveRoom: "lobby"})
		}
	}
}
func (h *Hub) NotifyUser(user, kind, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.Name == user {
			h.emitLocked(c, Event{Notice: text, NoticeKind: kind})
		}
	}
}
func (h *Hub) KickSession(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.ID == id {
			c.closeOnce.Do(func() { go c.Close() })
		}
	}
}
func (h *Hub) Enforce() {
	h.mu.Lock()
	clients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		h.EnforceOne(c)
	}
}
func (h *Hub) EnforceOne(c *Client) {
	_, _ = h.RefreshClient(c)
}
func (h *Hub) RefreshClient(c *Client) (User, bool) {
	if h.store == nil {
		return User{}, true
	}
	u, valid := h.store.SessionHealth(c)
	if !valid {
		c.closeOnce.Do(func() { go c.Close() })
	}
	return u, valid
}
func (h *Hub) MessageChanged(id int64) {
	if h.store == nil {
		return
	}
	m, e := messageQ(h.store.db, id)
	if e != nil {
		return
	}
	public := true
	var allowed map[string]bool
	if m.Recipient == "" {
		public, allowed = h.store.Readers(m.Room)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if m.Recipient != "" {
			if c.Name == m.Sender || c.Name == m.Recipient {
				h.emitLocked(c, Event{ChangedID: id})
			}
		} else if c.Room == m.Room && c.Peer == "" && (public || allowed[c.Name]) {
			h.emitLocked(c, Event{ChangedID: id})
		}
	}
}
func (h *Hub) Move(c *Client, room, peer string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.Room = room
	c.Peer = peer
	h.presenceLocked()
}
func (h *Hub) Broadcast(m Message) {
	public := true
	var allowed map[string]bool
	if h.store != nil && m.Recipient == "" {
		public, allowed = h.store.Readers(m.Room)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if m.Recipient == "" && !public && !allowed[c.Name] {
			continue
		}
		if m.Recipient != "" {
			if c.Name == m.Sender || c.Name == m.Recipient {
				h.emitLocked(c, Event{Message: &m})
			}
		} else if c.Room == m.Room && c.Peer == "" {
			h.emitLocked(c, Event{Message: &m})
		}
	}
}
func (h *Hub) Online() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	names := map[string]bool{}
	for c := range h.clients {
		names[c.Name] = true
	}
	result := make([]string, 0, len(names))
	for n := range names {
		result = append(result, n)
	}
	sort.Strings(result)
	return result
}
func (h *Hub) RoomCounts() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	counts := map[string]int{}
	for c := range h.clients {
		if c.Peer == "" {
			counts[c.Room]++
		}
	}
	return counts
}
func (h *Hub) Kick(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.Name == name {
			c.closeOnce.Do(func() { go c.Close() })
		}
	}
}
func (h *Hub) Stats() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return fmt.Sprintf("运行 %s · %d 个在线会话", time.Since(h.Started).Truncate(time.Second), len(h.clients))
}

type bucket struct {
	tokens float64
	at     time.Time
}

func (b *bucket) Allow() bool {
	now := time.Now()
	if b.at.IsZero() {
		b.tokens = 6
	} else {
		b.tokens = min(6, b.tokens+now.Sub(b.at).Seconds()*1.5)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
