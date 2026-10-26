package main

import "time"

// A viewport bottom is only the end of the in-memory page. feedAtLatest also
// records whether that page reaches the database tail for this conversation.
func lastMessageID(messages []Message) int64 {
	var id int64
	for _, message := range messages {
		id = max(id, message.ID)
	}
	return id
}

func (m *model) insertTimelineEvent(message Message) bool {
	index := len(m.feed)
	for i, item := range m.feed {
		if item.ID <= 0 {
			continue
		}
		if item.ID == message.ID {
			return false
		}
		if item.ID > message.ID && index == len(m.feed) {
			index = i
		}
	}
	if message.ID <= m.eventFloorID {
		return false // An old event outside the retained latest window.
	}
	m.feed = append(m.feed, Message{})
	copy(m.feed[index+1:], m.feed[index:])
	m.feed[index] = message
	return true
}

func (m *model) loadLatest(limit int) error {
	feed, err := m.store.History(m.room, m.user.Name, m.peer, limit)
	if err != nil {
		return err
	}
	m.feed = feed
	m.feedAtLatest = true
	m.latestMessageID = max(m.latestMessageID, lastMessageID(feed))
	m.eventFloorID = lastMessageID(feed)
	m.newMessages = 0
	m.rebuild(true)
	return nil
}

type readingAnchor struct {
	id     int64
	offset int
}

func (m model) timelineAnchor() readingAnchor {
	for _, segment := range m.segments {
		if segment.Last >= m.view.YOffset {
			return readingAnchor{id: segment.ID, offset: m.view.YOffset - segment.First}
		}
	}
	return readingAnchor{}
}

func (m *model) restoreTimelineAnchor(anchor readingAnchor) {
	for _, segment := range m.segments {
		if segment.ID == anchor.id {
			m.view.SetYOffset(max(0, segment.First+anchor.offset))
			return
		}
	}
}

func (m *model) loadNewer() error {
	newer, err := m.store.PageAfter(m.room, m.user.Name, m.peer, lastMessageID(m.feed), 100)
	if err != nil {
		return err
	}
	anchor := m.timelineAnchor()
	m.feed = append(m.feed, newer...)
	if len(m.feed) > maxFeedMessages {
		m.eventFloorID = max(m.eventFloorID, lastMessageID(m.feed[:len(m.feed)-maxFeedMessages]))
		m.feed = m.feed[len(m.feed)-maxFeedMessages:]
	}
	m.feedAtLatest = len(newer) < 100
	m.latestMessageID = max(m.latestMessageID, lastMessageID(newer))
	m.rebuild(false)
	m.restoreTimelineAnchor(anchor)
	return nil
}

func (m *model) scrollTimeline(lines int) {
	if lines < 0 {
		if m.view.AtTop() {
			m.loadOlder()
		}
		m.view.ScrollUp(-lines)
		return
	}
	wasBottom, offset := m.view.AtBottom(), m.view.YOffset
	m.view.ScrollDown(lines)
	if m.view.AtBottom() && !m.feedAtLatest {
		if err := m.loadNewer(); err != nil {
			m.view.SetYOffset(offset)
			m.timelineError(err)
			return
		}
		if wasBottom {
			m.view.ScrollDown(lines)
		}
	}
	if m.feedAtLatest && m.view.AtBottom() {
		m.newMessages = 0
	}
}

func (m *model) timelineError(err error) {
	m.flash = "载入消息失败，请重试：" + err.Error()
	m.flashKind = ""
	m.flashUntil = time.Now().Add(8 * time.Second)
}
