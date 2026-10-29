//! Bounded session-local submission history. Text is never Debug/serialized or
//! independently persisted; an explicit recall becomes the ordinary chat draft.
use super::*;
use miochat_gpui::composition::Origin;
use std::collections::VecDeque;

const CAPACITY: usize = 40;
const TEXT_LIMIT: usize = 1600;

// Inspect normalized command heads as iterators, not a cloned unsent body.
// Non-ASCII slash-command heads are conservatively excluded from history: Go
// simple Unicode lowercase and Rust expanding lowercase need not be identical.
fn credential_head(line: &str) -> bool {
    let head = || {
        line.chars()
            .filter(|c| !miochat_gpui::word::is_draft_format_control(*c))
            .skip_while(|c| c.is_whitespace())
            .take_while(|c| !c.is_whitespace())
    };
    if head().next() != Some('/') {
        return false;
    }
    if head().any(|c| !c.is_ascii()) {
        return true;
    }
    [
        "/passwd",
        "/password",
        "/auth",
        "/login",
        "/register",
        "/key-add",
        "/key-bind",
        "/resetpass",
        "/invitecode",
    ]
    .iter()
    .any(|name| head().map(|c| c.to_ascii_lowercase()).eq(name.chars()))
}
pub(super) fn safe_text(text: &str) -> bool {
    text.chars().count() <= TEXT_LIMIT
        && !text
            .chars()
            .any(|c| c.is_control() && c != '\n' && c != '\t')
        && !miochat_gpui::word::reserved_start(text)
        && !text.lines().any(credential_head)
}

struct Binding {
    owner: String,
    origin: Origin,
    generation: u64,
}
struct Entry {
    room: String,
    body: String,
}
struct Unsent {
    body: String,
    selection: Range<usize>,
}
#[derive(Default)]
pub(super) struct History {
    binding: Option<Binding>,
    room: String,
    entries: VecDeque<Entry>,
    cursor: Option<usize>,
    unsent: Option<Unsent>,
}
#[derive(Clone, Copy, PartialEq, Eq)]
enum Direction {
    Older,
    Newer,
}
impl History {
    pub fn clear(&mut self) {
        *self = Self::default();
    }
    fn sync(&mut self, context: Option<(&str, &Origin, u64, &str)>) {
        let Some((owner, origin, generation, room)) =
            context.filter(|(owner, _, _, room)| !owner.is_empty() && !room.is_empty())
        else {
            self.clear();
            return;
        };
        if !self
            .binding
            .as_ref()
            .is_some_and(|b| b.owner == owner && &b.origin == origin && b.generation == generation)
        {
            self.clear();
            self.binding = Some(Binding {
                owner: owner.into(),
                origin: origin.clone(),
                generation,
            });
        }
        if self.room != room {
            self.room = room.into();
            // Normal draft navigation owns the active composer. Never flush or
            // restore the history sentinel into either conversation on switch.
            self.cursor = None;
            self.unsent = None;
        }
    }
    fn record_attempt(&mut self, eligible: bool, text: &str) {
        if !eligible || self.binding.is_none() {
            return;
        }
        self.cursor = None;
        self.unsent = None;
        // Check before copying, including dedicated-form opening commands.
        if text.trim().is_empty() || !safe_text(text) {
            return;
        }
        self.entries.push_back(Entry {
            room: self.room.clone(),
            body: text.into(),
        });
        while self.entries.len() > CAPACITY {
            self.entries.pop_front();
        }
    }
    fn navigate(
        &mut self,
        direction: Direction,
        body: &mut String,
        selection: &mut Range<usize>,
        marked: &mut Option<Range<usize>>,
    ) -> bool {
        if self.binding.is_none() || marked.is_some() || !safe_text(body) {
            // In particular, do not clone a sensitive UNSUBMITTED input into
            // the first-Up sentinel. Refusal leaves text/caret/history intact.
            return false;
        }
        let visible: Vec<_> = self
            .entries
            .iter()
            .filter(|e| e.room == self.room)
            .collect();
        if visible.is_empty() {
            return false;
        }
        let next = match (direction, self.cursor) {
            (Direction::Older, None) => Some(visible.len() - 1),
            (Direction::Older, Some(i)) => Some(i.saturating_sub(1)),
            (Direction::Newer, None) => return false,
            (Direction::Newer, Some(i)) => (i + 1 < visible.len()).then_some(i + 1),
        };
        let (value, restored_selection) = if let Some(i) = next {
            let value = &visible[i].body;
            (value.as_str(), value.len()..value.len())
        } else {
            let Some(unsent) = &self.unsent else {
                return false;
            };
            (unsent.body.as_str(), unsent.selection.clone())
        };
        if !safe_text(value) {
            return false;
        }
        let first_unsent = self.cursor.is_none().then(|| Unsent {
            body: body.clone(),
            selection: editing::valid_selection(body, selection.clone()),
        });
        // Reuse the actual protected-word and editor-size boundary; there is
        // no direct string assignment that could bypass the composer guard.
        let previous_len = body.len();
        if !miochat_gpui::word::replace_composer(
            body,
            selection,
            marked,
            Some(0..previous_len),
            value,
        ) {
            return false;
        }
        *selection = editing::valid_selection(body, restored_selection);
        if let Some(unsent) = first_unsent {
            self.unsent = Some(unsent);
        }
        self.cursor = next;
        if next.is_none() {
            self.unsent = None;
        }
        true
    }
}
fn key_route(event: &KeyDownEvent, eligible: bool) -> Option<Direction> {
    if !eligible
        || event.is_held
        || event.prefer_character_input
        || event.keystroke.modifiers.modified()
    {
        return None;
    }
    match event.keystroke.key.as_str() {
        "up" => Some(Direction::Older),
        "down" => Some(Direction::Newer),
        _ => None,
    }
}
impl MioChatApp {
    pub(super) fn sync_input_history(&mut self) {
        let context = (self.screen == Screen::Chat
            && !self.must_reset
            && self
                .session
                .as_ref()
                .is_some_and(|s| s.generation() == self.generation))
        .then(|| {
            self.session_origin
                .as_ref()
                .map(|o| (self.user.as_str(), o, self.generation, self.room.as_str()))
        })
        .flatten();
        self.input_history.sync(context);
    }
    fn history_allowed(&self) -> bool {
        self.ordinary_chat_context_allowed()
            && !self.persistence.quarantined()
            && (!self.persistence_enabled || self.persistence.ready())
    }
    pub(super) fn record_input_attempt(&mut self) {
        self.sync_input_history();
        let eligible = self.history_allowed();
        self.input_history.record_attempt(eligible, &self.composer);
    }
    pub(super) fn history_key(
        &mut self,
        event: &KeyDownEvent,
        window: &Window,
        cx: &mut Context<Self>,
    ) -> bool {
        self.sync_input_history();
        let Some(direction) = key_route(
            event,
            self.history_allowed() && self.focus.is_focused(window),
        ) else {
            return false;
        };
        if self.input_history.navigate(
            direction,
            &mut self.composer,
            &mut self.selection,
            &mut self.marked_range,
        ) {
            // Explicit recall is ordinary draft editing; existing polling alone
            // owns autosave/reply/reading-position, not the transient history.
            cx.notify();
        }
        cx.stop_propagation();
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn history(origin: &Origin) -> History {
        let mut h = History::default();
        h.sync(Some(("alice", origin, 7, "lobby")));
        h
    }
    fn step(
        h: &mut History,
        direction: Direction,
        body: &mut String,
        selection: &mut Range<usize>,
    ) -> bool {
        h.navigate(direction, body, selection, &mut None)
    }
    #[test]
    fn actual_history_bounded_attempts_and_unicode_unsent_restore() {
        let o = Origin::synthetic("fixture", 1);
        let mut h = history(&o);
        for i in 0..41 {
            h.record_attempt(true, &format!("line{i}"));
        }
        assert_eq!(h.entries.len(), 40);
        assert_eq!(h.entries[0].body, "line1");
        let mut body = "未发🙂尾".to_string();
        let mut selection = 3..10;
        assert!(step(&mut h, Direction::Older, &mut body, &mut selection));
        assert_eq!(body, "line40");
        body = "编辑旧输入".into();
        assert!(step(&mut h, Direction::Older, &mut body, &mut selection));
        assert_eq!(body, "line39");
        assert!(step(&mut h, Direction::Newer, &mut body, &mut selection));
        assert!(step(&mut h, Direction::Newer, &mut body, &mut selection));
        assert_eq!(body, "未发🙂尾");
        assert_eq!(selection, 3..10);
        assert!(h.cursor.is_none() && h.unsent.is_none());
        assert!(!step(&mut h, Direction::Newer, &mut body, &mut selection));
        for _ in 0..50 {
            assert!(step(&mut h, Direction::Older, &mut body, &mut selection));
        }
        assert_eq!(body, "line1");
    }
    #[test]
    fn actual_history_sensitive_submissions_and_unsent_are_checked_before_copy() {
        let o = Origin::synthetic("fixture", 1);
        for secret in [
            "/passwd synthetic",
            "  /KEY-BIND x",
            "/resetpass alice",
            "/invitecode 1",
            "/word \nstart synthetic",
            "\n/word \r\nstart x",
            "hello\n/login token",
            "/pass\u{200b}wd synthetic",
            "\u{feff}/key-\u{200c}bind synthetic",
            "safe\n\u{200b}/invi\u{2028}tecode 1",
            "/pass\u{2029}wd synthetic",
            "/invİtecode synthetic",
            "/Key-add synthetic",
        ] {
            let mut h = history(&o);
            h.record_attempt(true, "safe");
            h.record_attempt(true, secret);
            assert_eq!(h.entries.len(), 1);
            let mut body = secret.to_string();
            let mut selection = 0..secret.len();
            let old = selection.clone();
            assert!(!step(&mut h, Direction::Older, &mut body, &mut selection));
            assert_eq!(body, secret);
            assert_eq!(selection, old);
            assert!(h.cursor.is_none() && h.unsent.is_none());
        }
        assert!(safe_text("/passwordless not-a-credential-command"));
        assert!(!safe_text("text\0control"));
        assert!(!safe_text(&"文".repeat(1601)));
    }
    #[test]
    fn actual_history_scope_navigation_never_flushes_or_retargets_sentinel() {
        let o = Origin::synthetic("fixture", 1);
        let mut h = history(&o);
        h.record_attempt(true, "lobby sent");
        let mut body = "unsent lobby".into();
        let mut selection = 0..0;
        assert!(step(&mut h, Direction::Older, &mut body, &mut selection));
        body = "currently edited".into();
        h.sync(Some(("alice", &o, 7, "dm:bob")));
        assert_eq!(body, "currently edited");
        assert!(h.unsent.is_none() && h.cursor.is_none());
        assert!(!step(&mut h, Direction::Older, &mut body, &mut selection));
        h.record_attempt(true, "dm only");
        h.sync(Some(("alice", &o, 7, "lobby")));
        assert!(step(&mut h, Direction::Older, &mut body, &mut selection));
        assert_eq!(body, "lobby sent");
    }
    #[test]
    fn actual_history_owner_origin_generation_and_credentials_clear_all() {
        let o = Origin::synthetic("fixtureA", 1);
        let other = Origin::synthetic("fixtureB", 1);
        for context in [
            Some(("bob", &o, 7, "lobby")),
            Some(("alice", &other, 7, "lobby")),
            Some(("alice", &o, 8, "lobby")),
            None,
        ] {
            let mut h = history(&o);
            h.record_attempt(true, "private old");
            let mut body = "unsent".into();
            assert!(step(&mut h, Direction::Older, &mut body, &mut (0..0)));
            h.sync(context);
            assert!(h.entries.is_empty() && h.unsent.is_none() && h.cursor.is_none());
            assert_eq!(body, "private old"); // lifecycle does not perform a second draft write
        }
        let spec = miochat_gpui::connection::ConnectionSpec::default();
        let origin = Origin::ssh(&spec, "synthetic-key".into(), "identity-a").unwrap();
        let mut changed_host = spec.clone();
        changed_host.host = "other.invalid".into();
        let mut changed_port = spec.clone();
        changed_port.port += 1;
        for next in [
            Origin::ssh(&changed_host, "synthetic-key".into(), "identity-a").unwrap(),
            Origin::ssh(&changed_port, "synthetic-key".into(), "identity-a").unwrap(),
            Origin::ssh(&spec, "other-key".into(), "identity-a").unwrap(),
            Origin::ssh(&spec, "synthetic-key".into(), "identity-b").unwrap(),
        ] {
            let mut h = history(&origin);
            h.record_attempt(true, "server-A-only");
            h.sync(Some(("alice", &next, 7, "lobby")));
            assert!(h.entries.is_empty());
        }
    }
    #[test]
    fn actual_history_attempt_route_does_not_record_ineligible_or_blank_inputs() {
        let o = Origin::synthetic("fixture", 1);
        let mut h = history(&o);
        h.record_attempt(false, "pending/menu/form/programmatic");
        h.record_attempt(true, " \n ");
        assert!(h.entries.is_empty());
        let mut untouched = "no history".into();
        assert!(!step(&mut h, Direction::Older, &mut untouched, &mut (0..0)));
        assert_eq!(untouched, "no history");
        assert!(h.unsent.is_none());
        h.record_attempt(true, "/unknown rejected command");
        assert_eq!(h.entries.len(), 1);
        // Server ACK/error is not an insertion hook; a pending repeat is ineligible.
        h.record_attempt(false, "/unknown rejected command");
        assert_eq!(h.entries.len(), 1);
        let mut body = "draft".into();
        let mut selection = 0..0;
        assert!(step(&mut h, Direction::Older, &mut body, &mut selection));
        h.record_attempt(true, "edited retry");
        assert!(h.unsent.is_none() && h.cursor.is_none());
        assert_eq!(h.entries.back().unwrap().body, "edited retry");
    }
    #[test]
    fn actual_history_key_gate_excludes_modifiers_composition_repeat_and_modal() {
        let mut e = KeyDownEvent {
            keystroke: gpui::Keystroke::parse("up").unwrap(),
            is_held: false,
            prefer_character_input: false,
        };
        assert!(key_route(&e, true) == Some(Direction::Older));
        assert!(key_route(&e, false).is_none());
        e.is_held = true;
        assert!(key_route(&e, true).is_none());
        e.is_held = false;
        e.prefer_character_input = true;
        assert!(key_route(&e, true).is_none());
        e.prefer_character_input = false;
        for key in ["ctrl-up", "shift-up", "alt-up", "cmd-up", "enter", "tab"] {
            e.keystroke = gpui::Keystroke::parse(key).unwrap();
            assert!(key_route(&e, true).is_none());
        }
        e.keystroke = gpui::Keystroke::parse("down").unwrap();
        assert!(key_route(&e, true) == Some(Direction::Newer));
        let o = Origin::synthetic("fixture", 1);
        let mut h = history(&o);
        h.record_attempt(true, "sent");
        let mut body = "输入中".into();
        let mut selection = 0..0;
        let mut marked = Some(0..3);
        assert!(!h.navigate(Direction::Older, &mut body, &mut selection, &mut marked));
        assert_eq!(body, "输入中");
        assert!(h.unsent.is_none());
    }
    #[test]
    fn actual_history_restore_runs_protected_composer_guard_and_utf8_selection() {
        let o = Origin::synthetic("fixture", 1);
        let mut h = history(&o);
        h.record_attempt(true, "中文🙂");
        let mut body = "草稿🙂".into();
        let mut selection = 1..999;
        assert!(step(&mut h, Direction::Older, &mut body, &mut selection));
        assert_eq!(selection, 10..10);
        assert!(step(&mut h, Direction::Newer, &mut body, &mut selection));
        assert_eq!(body, "草稿🙂");
        assert!(body.is_char_boundary(selection.start) && body.is_char_boundary(selection.end));
        // Even a corrupt/internally injected entry cannot bypass the same guard.
        h.entries.push_back(Entry {
            room: "lobby".into(),
            body: "/word start synthetic".into(),
        });
        let before = body.clone();
        assert!(!step(&mut h, Direction::Older, &mut body, &mut selection));
        assert_eq!(body, before);
    }
}
