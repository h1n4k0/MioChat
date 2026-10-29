package main

import "time"

// Consume bounded batches so a legitimate room-wide burst doesn't require one
// full history render and database refresh per recipient for every message.
func (m *model) consumeEvents(first Event) {
	events := []Event{first}
	draining := true
	for len(events) < 32 && draining {
		select {
		case e := <-m.client.Events:
			events = append(events, e)
		default:
			draining = false
		}
	}
	bottom := m.feedAtLatest && m.view.AtBottom()
	changed, attention := false, false
	channelMuted := false
	if m.peer == "" {
		channelMuted = m.store.Position(m.user.Name, "room:"+m.room).Muted
	}
	added := map[int64]bool{}
	for _, event := range events {
		if event.Preferences {
			m.settings = m.store.Settings(m.user.Name)
			if u, err := m.store.User(m.user.Name); err == nil {
				m.user.Theme = u.Theme
			}
			m.rebuild(false)
		}
		room, peer := m.room, m.peer
		if event.MoveRoom != "" {
			_ = m.forceMove(event.MoveRoom)
		}
		if event.Refresh {
			m.reloadVisible()
		}
		if room != m.room || peer != m.peer {
			// A move rebuilt a different conversation. Pending decorations and
			// scroll state from the preceding scope must not leak into it.
			changed = false
			added = map[int64]bool{}
			bottom = m.feedAtLatest && m.view.AtBottom()
			channelMuted = false
			if m.peer == "" {
				channelMuted = m.store.Position(m.user.Name, "room:"+m.room).Muted
			}
		}
		if event.ChangedID > 0 {
			m.refreshMessage(event.ChangedID)
		}
		if event.Notice != "" && !m.ignored[noticeActor(event.Notice)] && m.settings["dnd"] != "on" {
			m.flash = event.Notice
			m.flashKind = event.NoticeKind
			m.flashUntil = time.Now().Add(4 * time.Second)
			attention = true
		}
		if event.Message == nil {
			continue
		}
		message := *event.Message
		if m.ignored[message.Sender] {
			continue
		}
		current := message.Recipient == "" && m.peer == "" && message.Room == m.room
		if message.Recipient != "" {
			peer := message.Sender
			if peer == m.user.Name {
				peer = message.Recipient
			}
			if m.peer == peer {
				current = true
			} else if message.Sender != m.user.Name {
				m.unread[peer]++
				if m.settings["dnd"] != "on" {
					m.flash = "收到 @" + peer + " 的私信，/dm " + peer + " 查看"
					m.flashUntil = time.Now().Add(5 * time.Second)
				}
			}
			attention = true
		}
		if current {
			present := false
			for _, item := range m.feed {
				if item.ID == message.ID && message.ID > 0 {
					present = true
					break
				}
			}
			if present {
				continue // A history reload can already contain this queued event.
			}
			follow := m.feedAtLatest && m.view.AtBottom()
			if follow {
				if m.insertTimelineEvent(message) {
					added[message.ID] = true
					changed = true
					bottom = true
				}
			} else {
				// An event may be far beyond this historical page. Keep the page
				// contiguous and retrieve its intervening rows by cursor instead.
				if message.ID > lastMessageID(m.feed) {
					m.feedAtLatest = false
				}
				if message.ID > m.latestMessageID {
					m.newMessages++
				}
			}
			m.latestMessageID = max(m.latestMessageID, message.ID)
		}
		if message.Recipient != "" || !channelMuted {
			for _, name := range mentionTargets(message.Body) {
				attention = attention || name == m.user.Name
			}
			attention = attention || message.ReplyName == m.user.Name
		}
	}
	if bottom && len(m.feed) > 300 {
		m.eventFloorID = max(m.eventFloorID, lastMessageID(m.feed[:len(m.feed)-300]))
		m.feed = m.feed[len(m.feed)-300:]
	}
	if changed {
		var fresh []Message
		var indices []int
		for i, message := range m.feed {
			if added[message.ID] {
				fresh = append(fresh, message)
				indices = append(indices, i)
			}
		}
		m.store.decorate(fresh, m.user.Name)
		for i, index := range indices {
			m.feed[index] = fresh[i]
		}
		m.rebuild(bottom)
	}
	m.online = m.hub.Online()
	if attention {
		m.noticeCount = m.store.UnreadNotifications(m.user.Name)
		if m.settings["bell"] == "on" && m.settings["dnd"] != "on" {
			m.bellPending.Store(true)
		}
	}
}
