/// A presence snapshot must not cancel a room switch. A forced ACL fallback may.
pub fn accepts_room_state(
    pending: Option<(&str, &str)>,
    request_id: &str,
    room: &str,
    visible: &[String],
    forced: bool,
) -> bool {
    match pending {
        Some((id, target)) if !request_id.is_empty() => {
            id == request_id && (target == room || forced)
        }
        Some((_, target)) => target == room || forced || !visible.iter().any(|r| r == target),
        None => request_id.is_empty() || forced,
    }
}

/// A clear acknowledgement is local to the initiating request and conversation.
/// An ACL fallback or later navigation must never clear a different view.
pub fn accepts_clear_view(
    pending: (&str, &str),
    response: (&str, &str),
    current_room: &str,
) -> bool {
    !pending.0.is_empty()
        && !pending.1.is_empty()
        && pending == response
        && pending.1 == current_room
}

/// Keeping seen IDs across a local clear suppresses replayed/updated old rows,
/// but still admits new messages. Explicit history resets both collections.
pub fn merge_timeline_message(
    messages: &mut Vec<crate::session::Message>,
    seen: &mut std::collections::HashSet<i64>,
    message: crate::session::Message,
) -> bool {
    if seen.insert(message.id) {
        messages.push(message);
        messages.sort_by_key(|message| message.id);
        true
    } else if let Some(existing) = messages.iter_mut().find(|item| item.id == message.id) {
        *existing = message;
        true
    } else {
        false
    }
}

/// Clearing a view deliberately detaches it from its saved historical anchor.
/// Live/replayed events must not schedule restoration of that hidden anchor.
pub fn should_restore_live_view(
    persistence_enabled: bool,
    cleared: bool,
    pending_view: bool,
    restoring_view: bool,
) -> bool {
    persistence_enabled && !cleared && !pending_view && !restoring_view
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn historical_clear_then_live_preserves_anchor_and_notification_dedup() {
        use crate::{notifications::Notifications, persistence::Position, session::Message};
        let old = Message {
            id: 10,
            room: "lobby".into(),
            sender: "peer".into(),
            kind: "chat".into(),
            ..Message::default()
        };
        let new = Message {
            id: 11,
            ..old.clone()
        };
        let saved = Position {
            anchor: 10,
            offset: 3,
            last_read: 10,
        };
        let mut messages = vec![old.clone()];
        let mut seen = std::collections::HashSet::from([old.id]);
        let mut notifications = Notifications::default();
        notifications.history("lobby", &messages, true);
        messages.clear();
        let cleared = true;
        let mut restoring: Option<Position> = None;
        for message in [old.clone(), new.clone(), new.clone()] {
            let _ = notifications.live(&message, "me", "lobby", false);
            merge_timeline_message(&mut messages, &mut seen, message);
            // This is the same scheduling predicate used by the live UI path.
            if should_restore_live_view(true, cleared, false, restoring.is_some()) {
                restoring = Some(saved.clone());
            }
        }
        assert_eq!(messages, vec![new]);
        assert!(restoring.is_none()); // poll has no missing historical anchor to restore
        assert_eq!(saved.anchor, 10);
        assert_eq!(saved.offset, 3);
        assert_eq!(notifications.unread["lobby"], 1);
        assert!(!notifications.live(&old, "me", "lobby", false));
        assert!(should_restore_live_view(true, false, false, false));
        assert!(!should_restore_live_view(false, false, false, false));
        assert!(!should_restore_live_view(true, false, true, false));
        assert!(!should_restore_live_view(true, false, false, true));
    }
    #[test]
    fn cleared_timeline_suppresses_old_events_but_accepts_new_and_explicit_history() {
        use crate::session::Message;
        let old = Message {
            id: 1,
            body: "old".into(),
            ..Message::default()
        };
        let new = Message {
            id: 2,
            body: "new".into(),
            ..Message::default()
        };
        let mut messages = Vec::new();
        let mut seen = std::collections::HashSet::new();
        assert!(merge_timeline_message(
            &mut messages,
            &mut seen,
            old.clone()
        ));
        messages.clear();
        assert!(!merge_timeline_message(
            &mut messages,
            &mut seen,
            old.clone()
        ));
        assert!(messages.is_empty());
        assert!(merge_timeline_message(
            &mut messages,
            &mut seen,
            new.clone()
        ));
        assert_eq!(messages, vec![new.clone()]);
        // /history starts a fresh window, rather than treating historical rows
        // as newly delivered notification events.
        messages.clear();
        seen.clear();
        assert!(merge_timeline_message(
            &mut messages,
            &mut seen,
            new.clone()
        ));
        assert!(merge_timeline_message(
            &mut messages,
            &mut seen,
            old.clone()
        ));
        assert_eq!(messages, vec![old, new]);
    }
    #[test]
    fn clear_view_requires_current_request_and_scope() {
        assert!(accepts_clear_view(
            ("c1", "lobby"),
            ("c1", "lobby"),
            "lobby"
        ));
        assert!(!accepts_clear_view(
            ("c1", "lobby"),
            ("c2", "lobby"),
            "lobby"
        ));
        assert!(!accepts_clear_view(
            ("c1", "private"),
            ("c1", "private"),
            "lobby"
        ));
        assert!(!accepts_clear_view(
            ("c1", "private"),
            ("c1", "lobby"),
            "lobby"
        ));
        assert!(!accepts_clear_view(("", "lobby"), ("", "lobby"), "lobby"));
        assert!(!accepts_clear_view(("c1", ""), ("c1", ""), ""));
    }

    #[test]
    fn clear_wire_field_is_explicit_and_backwards_compatible() {
        let clear: crate::session::Response = serde_json::from_str(
            r#"{"type":"action","request_id":"c1","room":"lobby","clear_view":true,"at_latest":true,"reply_to":7}"#,
        ).unwrap();
        assert!(clear.clear_view);
        assert!(clear.at_latest);
        assert_eq!(clear.reply_to, 7);
        assert!(clear.messages.is_empty());
        let old: crate::session::Response = serde_json::from_str(r#"{"type":"action"}"#).unwrap();
        assert!(!old.clear_view);
    }
    #[test]
    fn interleaved_presence_stale_ack_and_acl_fallback() {
        let rooms = vec!["lobby".into(), "dev".into()];
        assert!(!accepts_room_state(
            Some(("r2", "dev")),
            "",
            "lobby",
            &rooms,
            false
        ));
        assert!(!accepts_room_state(
            Some(("r2", "dev")),
            "r1",
            "dev",
            &rooms,
            false
        ));
        assert!(accepts_room_state(
            Some(("r2", "dev")),
            "r2",
            "dev",
            &rooms,
            false
        ));
        assert!(accepts_room_state(
            Some(("r2", "dev")),
            "",
            "lobby",
            &["lobby".into()],
            false
        ));
        assert!(accepts_room_state(
            Some(("r2", "dev")),
            "r2",
            "lobby",
            &["lobby".into()],
            true
        ));
        assert!(!accepts_room_state(None, "r1", "lobby", &rooms, false));
    }
}
