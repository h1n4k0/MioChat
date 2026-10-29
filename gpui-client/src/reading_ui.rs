//! Keyboard reading uses the same server windows as the visible timeline buttons.
use super::*;
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum ReadingKey {
    Older,
    Newer,
    Latest,
    CancelReply,
}
fn route(e: &KeyDownEvent, eligible: bool) -> Option<ReadingKey> {
    if !eligible || e.is_held || e.prefer_character_input {
        return None;
    }
    let m = &e.keystroke.modifiers;
    if m.control && !m.platform && !m.alt && !m.shift && !m.function {
        return (e.keystroke.key == "end").then_some(ReadingKey::Latest);
    }
    if m.modified() {
        return None;
    }
    match e.keystroke.key.as_str() {
        "pageup" => Some(ReadingKey::Older),
        "pagedown" => Some(ReadingKey::Newer),
        "escape" => Some(ReadingKey::CancelReply),
        _ => None,
    }
}
fn page_offset(offset: f32, maximum: f32, height: f32, older: bool) -> f32 {
    let step = (height / 2.).max(1.);
    (offset + if older { step } else { -step }).clamp(-maximum.max(0.), 0.)
}
pub(super) struct ReadingMove {
    request: String,
    room: String,
    owner: String,
    generation: u64,
    origin: miochat_gpui::composition::Origin,
    older: bool,
    ready: bool,
}
fn reading_ack(expected: (&str, &str), actual: (&str, &str), success: bool) -> Option<bool> {
    (expected.0 == actual.0).then_some(success && expected.1 == actual.1)
}
impl ReadingMove {
    pub(super) fn ready_for(&self, id: &str) -> bool {
        self.ready && self.request == id
    }
}
pub(super) fn restore_after_history(
    persistent: bool,
    keyboard_page: bool,
    has_draft: bool,
    has_messages: bool,
) -> bool {
    (persistent || keyboard_page) && !has_draft && has_messages
}
impl MioChatApp {
    fn half_page(&mut self, older: bool) {
        let y = page_offset(
            self.timeline_scroll.offset().y.into(),
            self.timeline_scroll.max_offset().y.into(),
            self.timeline_scroll.bounds().size.height.into(),
            older,
        );
        self.timeline_scroll.set_offset(gpui::point(px(0.), px(y)));
    }
    pub(super) fn reading_response(&mut self, id: &str, room: &str, success: bool) {
        let decision = self
            .reading_move
            .as_ref()
            .and_then(|p| reading_ack((&p.request, &p.room), (id, room), success));
        match decision {
            Some(true) if room == self.room => self.reading_move.as_mut().unwrap().ready = true,
            Some(_) => self.reading_move = None,
            None => {}
        }
    }
    pub(super) fn poll_reading_move(&mut self, cx: &mut Context<Self>) {
        let Some(p) = self.reading_move.as_ref() else {
            return;
        };
        if self.screen != Screen::Chat
            || p.room != self.room
            || p.owner != self.user
            || p.generation != self.generation
            || self.session_origin.as_ref() != Some(&p.origin)
            || self.timeline_cleared && p.ready
            || self.view_unavailable
        {
            self.reading_move = None;
            return;
        }
        if !p.ready {
            if self
                .pending_send
                .as_ref()
                .is_none_or(|v| v.request_id != p.request)
                && self
                    .pending_view
                    .as_ref()
                    .is_none_or(|v| v.request_id != p.request)
            {
                self.reading_move = None;
            }
            return;
        }
        if self.restoring_view.is_some() || self.layout_revision != self.message_revision {
            return;
        }
        let older = self.reading_move.take().unwrap().older;
        self.half_page(older);
        cx.notify();
    }
    pub(super) fn reading_key(&mut self, event: &KeyDownEvent, cx: &mut Context<Self>) -> bool {
        let Some(key) = route(
            event,
            self.ordinary_chat_context_allowed() && self.reading_move.is_none(),
        ) else {
            return false;
        };
        match key {
            ReadingKey::CancelReply => {
                if self.reply_to == 0 {
                    return false;
                }
                self.run_command("/cancelreply".into(), true, cx);
            }
            ReadingKey::Latest => {
                if self.persistence_enabled {
                    if let Err(e) = self.request_view(Position::default(), false) {
                        self.error = Some(e.to_string());
                    }
                } else if self.mock_mode {
                    self.timeline_scroll.scroll_to_bottom();
                } else {
                    self.error = Some("服务器不支持阅读窗口；请使用 /history。".into());
                }
            }
            ReadingKey::Older | ReadingKey::Newer => {
                let older = key == ReadingKey::Older;
                // Explicit PageUp is allowed to request history after clear; PageDown is not.
                if self.timeline_cleared && !older {
                    cx.stop_propagation();
                    return true;
                }
                let edge = if older {
                    self.timeline_scroll.offset().y >= px(-2.)
                } else {
                    self.timeline_scroll.offset().y + self.timeline_scroll.max_offset().y <= px(2.)
                };
                if self.restoring_view.is_some() || self.layout_revision != self.message_revision {
                    return true;
                }
                if edge && (older || !self.feed_latest) && !self.mock_mode {
                    let origin = self.session_origin.clone().unwrap();
                    let request = if older {
                        self.run_command(
                            if self.timeline_cleared {
                                "/history"
                            } else {
                                "/older"
                            }
                            .into(),
                            true,
                            cx,
                        );
                        self.pending_send.as_ref().map(|p| p.request_id.clone())
                    } else if self.persistence_enabled {
                        match self.request_view(self.reading_position(), true) {
                            Ok(()) => self.pending_view.as_ref().map(|p| p.request_id.clone()),
                            Err(e) => {
                                self.error = Some(e.to_string());
                                None
                            }
                        }
                    } else {
                        self.error = Some("服务器不支持较新阅读窗口。".into());
                        None
                    };
                    if let Some(request) = request {
                        self.reading_move = Some(ReadingMove {
                            request,
                            room: self.room.clone(),
                            owner: self.user.clone(),
                            generation: self.generation,
                            origin,
                            older,
                            ready: false,
                        });
                    }
                } else {
                    self.half_page(older);
                }
            }
        }
        cx.stop_propagation();
        cx.notify();
        true
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    fn key(s: &str) -> KeyDownEvent {
        KeyDownEvent {
            keystroke: gpui::Keystroke::parse(s).unwrap(),
            is_held: false,
            prefer_character_input: false,
        }
    }
    #[test]
    fn actual_reading_key_route_and_half_viewport_bounds() {
        for (s, k) in [
            ("pageup", ReadingKey::Older),
            ("pagedown", ReadingKey::Newer),
            ("ctrl-end", ReadingKey::Latest),
            ("escape", ReadingKey::CancelReply),
        ] {
            let e = key(s);
            assert_eq!(route(&e, true), Some(k));
            assert_eq!(route(&e, false), None);
            let mut held = e.clone();
            held.is_held = true;
            assert_eq!(route(&held, true), None);
            let mut ime = e;
            ime.prefer_character_input = true;
            assert_eq!(route(&ime, true), None);
        }
        for s in ["ctrl-pageup", "shift-pagedown", "alt-end", "super-end"] {
            assert_eq!(route(&key(s), true), None);
        }
        assert_eq!(page_offset(-400., 1000., 300., true), -250.);
        assert_eq!(page_offset(-400., 1000., 300., false), -550.);
        assert_eq!(page_offset(-10., 1000., 500., true), 0.);
        assert_eq!(page_offset(-900., 1000., 500., false), -1000.);
        assert_eq!(page_offset(0., 0., 500., false), 0.);
    }
    #[test]
    fn actual_reading_old_ack_error_and_wrong_scope_do_not_schedule_scroll() {
        assert_eq!(
            reading_ack(("request", "lobby"), ("old", "lobby"), true),
            None
        );
        assert_eq!(
            reading_ack(("request", "lobby"), ("request", "other"), true),
            Some(false)
        );
        assert_eq!(
            reading_ack(("request", "lobby"), ("request", "lobby"), false),
            Some(false)
        );
        assert_eq!(
            reading_ack(("request", "lobby"), ("request", "lobby"), true),
            Some(true)
        );
    }
    #[test]
    fn actual_keyboard_history_restores_anchor_with_or_without_persistence() {
        for persistent in [true, false] {
            assert!(restore_after_history(persistent, true, false, true));
            assert!(!restore_after_history(persistent, true, true, true));
            assert!(!restore_after_history(persistent, true, false, false));
        }
        assert!(!restore_after_history(false, false, false, true));
    }
}
