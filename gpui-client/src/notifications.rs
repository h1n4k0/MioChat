use crate::session::Message;
use std::collections::{HashMap, HashSet, VecDeque};

/// In-memory only. History establishes a replay floor; live IDs are deduplicated
/// independently so interleaved room events and out-of-order delivery remain safe.
#[derive(Default)]
pub struct Notifications {
    floors: HashMap<String, i64>,
    seen: HashSet<(String, i64)>,
    order: VecDeque<(String, i64)>,
    pub unread: HashMap<String, usize>,
}
impl Notifications {
    pub fn live_with_preferences(
        &mut self,
        message: &Message,
        user: &str,
        current_room: &str,
        foreground: bool,
        preferences: &crate::preferences::Preferences,
    ) -> bool {
        let live = self.live_filtered(
            message,
            user,
            current_room,
            foreground,
            preferences.blocks(&message.sender),
        );
        let room = preferences.rooms.get(&message.room);
        if !message.room.starts_with("dm:") && !room.is_some_and(|p| p.subscribed) {
            self.unread.remove(&message.room);
        }
        live && preferences.alerts() && !room.is_some_and(|p| p.muted)
    }
    pub fn history(&mut self, room: &str, messages: &[Message], entering_room: bool) {
        let floor = messages.iter().map(|m| m.id).max().unwrap_or(0);
        if entering_room {
            self.floors.insert(room.into(), floor);
        } else {
            self.floors.entry(room.into()).or_insert(floor);
        }
    }
    pub fn live(
        &mut self,
        message: &Message,
        user: &str,
        current_room: &str,
        foreground: bool,
    ) -> bool {
        self.live_filtered(message, user, current_room, foreground, false)
    }
    fn live_filtered(
        &mut self,
        message: &Message,
        user: &str,
        current_room: &str,
        foreground: bool,
        blocked: bool,
    ) -> bool {
        if message.id <= *self.floors.get(&message.room).unwrap_or(&0) {
            return false;
        }
        let key = (message.room.clone(), message.id);
        if !self.seen.insert(key.clone()) {
            return false;
        }
        self.order.push_back(key);
        while self.order.len() > 8192 {
            if let Some(old) = self.order.pop_front() {
                self.seen.remove(&old);
            }
        }
        if blocked
            || message.sender == user
            || message.edited != 0
            || message.recalled != 0
            || message.kind != "chat"
        {
            return false;
        }
        if foreground && current_room == message.room {
            return false;
        }
        *self.unread.entry(message.room.clone()).or_default() += 1;
        true
    }
    pub fn read(&mut self, room: &str) {
        self.unread.remove(room);
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn muted_room_keeps_subscribed_unread_but_does_not_alert() {
        let mut p = crate::preferences::Preferences::default();
        p.rooms.insert(
            "dev".into(),
            crate::preferences::RoomPreferences {
                subscribed: true,
                muted: true,
                ..Default::default()
            },
        );
        let mut n = Notifications::default();
        assert!(!n.live_with_preferences(&message(1, "dev"), "me", "lobby", false, &p));
        assert_eq!(n.unread["dev"], 1);
        p.rooms.get_mut("dev").unwrap().subscribed = false;
        assert!(!n.live_with_preferences(&message(2, "dev"), "me", "lobby", false, &p));
        assert!(!n.unread.contains_key("dev"));
    }
    fn message(id: i64, room: &str) -> Message {
        Message {
            id,
            room: room.into(),
            sender: "peer".into(),
            kind: "chat".into(),
            ..Default::default()
        }
    }
    #[test]
    fn blocked_messages_have_no_unread_or_replay_after_unblock() {
        let mut n = Notifications::default();
        let mut p = crate::preferences::Preferences::default();
        p.blocked.push("peer".into());
        assert!(!n.live_with_preferences(&message(1, "lobby"), "me", "other", false, &p));
        assert!(n.unread.is_empty());
        p.blocked.clear();
        assert!(!n.live_with_preferences(&message(1, "lobby"), "me", "other", false, &p));
        assert!(n.live_with_preferences(&message(2, "lobby"), "me", "other", false, &p));
    }
    #[test]
    fn dnd_keeps_unread_and_does_not_replay_suppressed_notifications() {
        let mut n = Notifications::default();
        let mut p = crate::preferences::Preferences::default();
        p.settings.insert("dnd".into(), "on".into());
        assert!(!n.live_with_preferences(&message(1, "dm:peer"), "me", "lobby", false, &p));
        assert_eq!(n.unread["dm:peer"], 1);
        p.settings.insert("dnd".into(), "off".into());
        assert!(!n.live_with_preferences(&message(1, "dm:peer"), "me", "lobby", false, &p));
        assert!(n.live_with_preferences(&message(2, "dm:peer"), "me", "lobby", false, &p));
        assert_eq!(n.unread["dm:peer"], 2);
    }
    #[test]
    fn history_own_updates_and_reconnect_replays_never_notify() {
        let mut n = Notifications::default();
        n.history("lobby", &[message(10, "lobby")], true);
        assert!(!n.live(&message(10, "lobby"), "me", "lobby", false));
        let mut own = message(11, "lobby");
        own.sender = "me".into();
        assert!(!n.live(&own, "me", "lobby", false));
        let mut edited = message(12, "lobby");
        edited.edited = 1;
        assert!(!n.live(&edited, "me", "lobby", false));
        let mut recalled = message(13, "lobby");
        recalled.recalled = 1;
        assert!(!n.live(&recalled, "me", "lobby", false));
        assert!(n.live(&message(14, "lobby"), "me", "lobby", false));
        assert!(!n.live(&message(14, "lobby"), "me", "lobby", false));
        n.history("lobby", &[message(14, "lobby")], true);
        assert!(!n.live(&message(14, "lobby"), "me", "lobby", false));
    }
    #[test]
    fn presence_snapshot_does_not_swallow_live_notification() {
        let mut n = Notifications::default();
        n.history("lobby", &[message(10, "lobby")], true);
        n.history("lobby", &[message(11, "lobby")], false);
        assert!(n.live(&message(11, "lobby"), "me", "lobby", false));
    }
    #[test]
    fn foreground_room_and_out_of_order_rooms_are_distinguished() {
        let mut n = Notifications::default();
        assert!(!n.live(&message(1, "lobby"), "me", "lobby", true));
        assert!(n.live(&message(3, "dev"), "me", "lobby", true));
        assert!(n.live(&message(2, "dev"), "me", "lobby", true));
        assert_eq!(n.unread["dev"], 2);
        n.read("dev");
        assert!(!n.unread.contains_key("dev"));
    }

    #[test]
    fn dm_scopes_keep_notifications_and_history_separate() {
        let mut n = Notifications::default();
        n.history("dm:alice", &[message(10, "dm:alice")], true);
        assert!(!n.live(&message(10, "dm:alice"), "me", "lobby", false));
        assert!(n.live(&message(11, "dm:bob"), "me", "dm:alice", true));
        assert!(!n.live(&message(12, "dm:alice"), "me", "dm:alice", true));
        assert_eq!(n.unread["dm:bob"], 1);
        assert!(!n.unread.contains_key("dm:alice"));
    }
}
