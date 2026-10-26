package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func navigationFixture(t *testing.T, direct, historical bool, total int) (*Store, *Hub, model) {
	t.Helper()
	s, hub := featureFixture(t)
	room, recipient, peer := "lobby", "", ""
	if direct {
		room, recipient, peer = "", "alice", "bob"
	}
	if _, err := s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO messages(room,sender,recipient,body,kind,at) SELECT ?,'bob',?,printf('中文消息 %04d',x),'chat',1700000000+x FROM n`, total, room, recipient); err != nil {
		t.Fatal(err)
	}
	if historical {
		p := Position{Draft: "尚未发送的中文草稿", Offset: 12, Anchor: 101, LastRead: 300}
		if err := s.SavePosition("alice", scopeFor(room, peer), p); err != nil {
			t.Fatal(err)
		}
	}
	m := testModel(t, s, hub, "alice")
	if direct {
		if err := m.move("", peer); err != nil {
			t.Fatal(err)
		}
	}
	return s, hub, m
}

func assertContinuousTimeline(t *testing.T, m *model) {
	t.Helper()
	if len(m.feed) > maxFeedMessages || len(m.messageCache) > maxCachedMessages {
		t.Fatal("timeline memory exceeds its independent limits", len(m.feed), len(m.messageCache))
	}
	for i := 1; i < len(m.feed); i++ {
		if m.feed[i].ID != m.feed[i-1].ID+1 {
			t.Fatal("a timeline page contains a gap or duplicate", m.feed[i-1].ID, m.feed[i].ID)
		}
	}
}

func TestHistoryWindowScrollsForwardAfterFiveHundredMessageTruncation(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(fmt.Sprintf("direct=%t", direct), func(t *testing.T) {
			s, _, m := navigationFixture(t, direct, false, 900)
			for i := 0; i < 8; i++ {
				m.view.GotoTop()
				m.scrollTimeline(-3)
				assertContinuousTimeline(t, &m)
			}
			if len(m.feed) != 500 || m.feed[0].ID != 1 || lastMessageID(m.feed) != 500 || m.feedAtLatest {
				t.Fatal("older paging did not expose the truncated tail", len(m.feed), m.feed[0].ID, lastMessageID(m.feed), m.feedAtLatest)
			}
			room, recipient := "lobby", ""
			if direct {
				room, recipient = "", "alice"
			}
			incoming := mustSend(t, s, "bob", room, recipient, "真正的最新消息")
			m.view.GotoBottom()
			m.consumeEvents(Event{Message: &incoming})
			if lastMessageID(m.feed) != 500 || m.newMessages != 1 {
				t.Fatal("historical window appended across its missing pages", lastMessageID(m.feed), m.newMessages)
			}
			for steps := 0; steps < 10 && !(m.feedAtLatest && m.view.AtBottom()); steps++ {
				m.scrollTimeline(100000)
				assertContinuousTimeline(t, &m)
			}
			if !m.feedAtLatest || !m.view.AtBottom() || lastMessageID(m.feed) != incoming.ID || m.newMessages != 0 {
				t.Fatal("scrolling down remained trapped in an old page", m.feedAtLatest, m.view.AtBottom(), lastMessageID(m.feed), m.newMessages)
			}
			var count int
			if err := s.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil || count != 901 {
				t.Fatal("paging changed durable chat history", count, err)
			}
		})
	}
}

func TestHistoryOldCursorRecoversAndLiveReconnectUsesLatestPage(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(fmt.Sprintf("direct=%t", direct), func(t *testing.T) {
			s, hub, m := navigationFixture(t, direct, true, 900)
			if m.feed[0].ID != 101 || lastMessageID(m.feed) != 300 || m.feedAtLatest {
				t.Fatal("legacy reading cursor was not restored as history")
			}
			m.view.GotoBottom()
			if m.currentPosition().Offset == 0 {
				t.Fatal("old page bottom was persisted as the database tail")
			}
			m.keyInteractions(tea.KeyMsg{Type: tea.KeyCtrlEnd})
			if lastMessageID(m.feed) != 900 || !m.feedAtLatest || !m.view.AtBottom() || m.view.YOffset == 0 {
				t.Fatal("Ctrl+End did not reach a real nonzero latest viewport")
			}
			stored := s.Position("alice", scopeFor(m.room, m.peer))
			if stored.Offset != 0 || stored.Draft != "尚未发送的中文草稿" {
				t.Fatal("live position or unsent draft was saved incorrectly", stored)
			}
			reconnected := testModel(t, s, hub, "alice")
			if direct {
				if err := reconnected.move("", "bob"); err != nil {
					t.Fatal(err)
				}
			}
			if reconnected.feed[0].ID != 801 || lastMessageID(reconnected.feed) != 900 || !reconnected.feedAtLatest || !reconnected.view.AtBottom() {
				t.Fatal("a live reconnect returned to an old anchor")
			}
		})
	}
}

func TestHistoryCurrentSessionSendReturnsToLatestWithoutDuplicateEvent(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(fmt.Sprintf("direct=%t", direct), func(t *testing.T) {
			s, _, m := navigationFixture(t, direct, true, 900)
			if err := m.send("当前会话的新发言", "chat", m.peer); err != nil {
				t.Fatal(err)
			}
			if lastMessageID(m.feed) != 901 || !m.feedAtLatest || !m.view.AtBottom() {
				t.Fatal("sending in this conversation did not return to latest")
			}
			latest, err := s.History(m.room, "alice", m.peer, 1)
			if err != nil || len(latest) != 1 {
				t.Fatal("missing durable sent message", err)
			}
			// The history reload already contains the still-queued Broadcast.
			m.consumeEvents(Event{Message: &latest[0]})
			var copies int
			for _, message := range m.feed {
				if message.ID == latest[0].ID {
					copies++
				}
			}
			if copies != 1 {
				t.Fatal("the queued event duplicated a history row", copies)
			}
			assertContinuousTimeline(t, &m)
		})
	}
}

func TestHistorySavedCursorAnchorsTheVisibleMessageWithinLargeWindow(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(fmt.Sprintf("direct=%t", direct), func(t *testing.T) {
			s, hub, m := navigationFixture(t, direct, false, 900)
			for i := 0; i < 4; i++ {
				m.view.GotoTop()
				m.scrollTimeline(-3)
			}
			m.view.SetYOffset(m.segments[300].First + 1)
			visible := m.timelineAnchor()
			if err := m.saveView(); err != nil {
				t.Fatal(err)
			}
			p := s.Position("alice", scopeFor(m.room, m.peer))
			if p.Anchor != visible.id || p.Offset != 1 {
				t.Fatal("cursor retained an absolute line from the 500-row window", p.Anchor, p.Offset, visible)
			}
			reconnected := testModel(t, s, hub, "alice")
			if direct {
				if err := reconnected.move("", "bob"); err != nil {
					t.Fatal(err)
				}
			}
			// The existing Offset=0 live sentinel reserves the top historical
			// line: restoring a relative zero offset may advance by one line.
			if reconnected.feed[0].ID != visible.id || reconnected.view.YOffset != max(1, visible.offset) {
				t.Fatal("the saved visible message moved beyond the one-line compatibility bound", visible, reconnected.timelineAnchor())
			}
		})
	}
}

func TestHistoryLoadingFailureKeepsExistingWindowAndDraft(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(fmt.Sprintf("direct=%t", direct), func(t *testing.T) {
			s, _, m := navigationFixture(t, direct, true, 900)
			before := append([]Message(nil), m.feed...)
			m.view.GotoBottom()
			offset, draft := m.view.YOffset, m.input.Value()
			if err := s.db.reader.Close(); err != nil {
				t.Fatal(err)
			}
			m.keyInteractions(tea.KeyMsg{Type: tea.KeyCtrlEnd})
			m.scrollTimeline(3)
			m.command("/history")
			if !reflect.DeepEqual(m.feed, before) || m.view.YOffset != offset || m.input.Value() != draft || m.feedAtLatest || !strings.Contains(m.flash, "载入消息失败") {
				t.Fatal("a failed history query cleared or moved the previous view", m.view.YOffset, offset, m.flash)
			}
		})
	}
}

func TestLatestTimelineOrdersAndDeduplicatesDelayedEvents(t *testing.T) {
	s, hub := featureFixture(t)
	m := testModel(t, s, hub, "alice")
	first := mustSend(t, s, "bob", "lobby", "", "先提交后广播")
	second := mustSend(t, s, "bob", "lobby", "", "后提交先广播")
	m.consumeEvents(Event{Message: &second})
	m.consumeEvents(Event{Message: &first})
	m.consumeEvents(Event{Message: &second})
	if len(m.feed) != 2 || m.feed[0].ID != first.ID || m.feed[1].ID != second.ID {
		t.Fatal("delayed events were lost, duplicated, or ordered incorrectly", m.feed)
	}
}

func TestLiveEventDecorationFailureRetainsPlaceholderInsteadOfDroppingFeed(t *testing.T) {
	s, hub := featureFixture(t)
	m := testModel(t, s, hub, "alice")
	message := mustSend(t, s, "bob", "lobby", "", "不得在元数据失败时直接显示原文")
	if err := s.db.reader.Close(); err != nil {
		t.Fatal(err)
	}
	m.consumeEvents(Event{Message: &message})
	if len(m.feed) != 1 || m.feed[0].ID != message.ID || !strings.Contains(m.feed[0].Body, "暂时无法加载消息状态") {
		t.Fatal("decoration failure dropped the feed or exposed unchecked raw content", m.feed)
	}
}
