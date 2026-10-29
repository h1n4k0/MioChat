//! Chat-only affordances. Dedicated editors own their keys before this route.
use super::*;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Shortcut {
    Help,
    Notifications,
    Contacts,
    Menu,
    Admin,
    Sidebar,
}
impl Shortcut {
    fn command(self) -> Option<&'static str> {
        match self {
            Self::Help => Some("/help"),
            Self::Notifications => Some("/notifications"),
            Self::Contacts => Some("/contacts"),
            Self::Menu => Some("/menu"),
            Self::Admin => Some("/admin"),
            Self::Sidebar => None,
        }
    }
}
fn context_allowed(screen: Screen, bound: bool, occupied: &[bool; 34]) -> bool {
    screen == Screen::Chat && bound && !occupied.iter().any(|v| *v)
}
fn key_route(event: &KeyDownEvent, eligible: bool) -> Option<Shortcut> {
    if !eligible || event.is_held || event.prefer_character_input {
        return None;
    }
    let m = &event.keystroke.modifiers;
    if m.control && !m.platform && !m.alt && !m.shift && !m.function {
        return (event.keystroke.key == "b").then_some(Shortcut::Sidebar);
    }
    if m.modified() {
        return None;
    }
    match event.keystroke.key.as_str() {
        "f1" => Some(Shortcut::Help),
        "f2" => Some(Shortcut::Notifications),
        "f3" => Some(Shortcut::Contacts),
        "f4" => Some(Shortcut::Menu),
        "f8" => Some(Shortcut::Admin),
        _ => None,
    }
}
pub(super) fn sidebar_shown(screen: Screen, preference: bool) -> bool {
    screen != Screen::Chat || preference
}
// No layout transition may restore the persisted pre-clear anchor into a
// deliberately cleared feed. Require a real present anchor for non-tail views.
fn reflow_position(
    position: Position,
    ids: &[i64],
    cleared: bool,
    ready: bool,
) -> Result<Option<Position>, &'static str> {
    if !ready {
        return Err("阅读布局正在恢复；请稍后再切换侧栏。");
    }
    if cleared || ids.is_empty() {
        return Ok(None);
    }
    if position.offset > 0 && !ids.contains(&position.anchor) {
        return Err("当前阅读锚点不可用；请先恢复阅读位置，再切换侧栏。");
    }
    Ok(Some(position))
}
fn apply_sidebar_change(
    visible: &mut bool,
    revision: &mut u64,
    restoration: &mut Option<(Position, Option<u64>)>,
    position: Option<Position>,
) {
    *visible = !*visible;
    *revision = revision.wrapping_add(1);
    *restoration = position.map(|p| (p, None));
}
impl MioChatApp {
    fn affordances_allowed(&self) -> bool {
        self.ordinary_chat_context_allowed() && (self.native_actions || self.mock_mode)
    }
    pub(super) fn ordinary_chat_context_allowed(&self) -> bool {
        !self.attachment_context_occupied()
            && !self.custom_emoji_context_occupied()
            && self.ordinary_chat_context_without_attachment()
    }
    pub(super) fn ordinary_chat_context_without_attachment(&self) -> bool {
        !self.dot_visible()
            && !self.emoji_open()
            && context_allowed(
                self.screen,
                self.session
                    .as_ref()
                    .is_some_and(|s| s.generation() == self.generation)
                    && self.session_origin.is_some()
                    && !self.user.is_empty()
                    && !self.room.is_empty()
                    && !self.must_reset,
                &[
                    self.direct_form.is_some(),
                    self.composition.is_some(),
                    self.word_start.is_some(),
                    self.secret_admin.is_some(),
                    self.secret_form_origin.is_some(),
                    self.maintenance_visible(),
                    self.maintenance
                        .as_ref()
                        .is_some_and(|j| j.pending.is_some() || j.confirming),
                    self.profile.is_some(),
                    self.profile_editor.is_some(),
                    self.room_form.is_some(),
                    self.center_open,
                    self.preferences_open,
                    self.feature_panel.is_some(),
                    self.appearance_menu,
                    self.mock_menu,
                    self.key_reminder_active(),
                    self.marked_range.is_some(),
                    self.message_selection.is_some(),
                    self.pending_send.is_some(),
                    self.pending_room.is_some(),
                    self.pending_room_form.is_some(),
                    self.pending_preferences.is_some(),
                    self.pending_center.is_some(),
                    self.pending_profile.is_some(),
                    self.pending_password.is_some(),
                    self.pending_key_bind.is_some(),
                    self.pending_key_login.is_some(),
                    self.pending_draft.is_some(),
                    self.pending_view.is_some(),
                    self.persistence.pending(),
                    self.queued_room.is_some(),
                    self.close_started.is_some(),
                    self.close_ready,
                    self.quarantine_confirmation.is_some(),
                ],
            )
    }
    pub(super) fn toggle_sidebar(&mut self, cx: &mut Context<Self>) {
        if !self.affordances_allowed() {
            return;
        }
        let ids: Vec<_> = self.messages.iter().map(|m| m.id).collect();
        let position = match reflow_position(
            self.reading_position(),
            &ids,
            self.timeline_cleared,
            self.restoring_view.is_none()
                && !self.view_unavailable
                && self.layout_revision == self.message_revision,
        ) {
            Ok(position) => position,
            Err(error) => {
                self.error = Some(error.into());
                cx.notify();
                return;
            }
        };
        // Invalidate old-width geometry. InputCapture marks only the next
        // rendered layout as this revision, before scroll restoration runs.
        apply_sidebar_change(
            &mut self.sidebar_visible,
            &mut self.message_revision,
            &mut self.restoring_view,
            position,
        );
        cx.notify();
    }
    pub(super) fn affordance_key(&mut self, event: &KeyDownEvent, cx: &mut Context<Self>) -> bool {
        let Some(action) = key_route(event, self.affordances_allowed()) else {
            return false;
        };
        if let Some(command) = action.command() {
            self.begin_feature_navigation();
            self.run_command(command.into(), true, cx);
        } else {
            self.toggle_sidebar(cx);
        }
        cx.stop_propagation();
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn key(name: &str) -> KeyDownEvent {
        KeyDownEvent {
            keystroke: gpui::Keystroke::parse(name).unwrap(),
            is_held: false,
            prefer_character_input: false,
        }
    }
    #[test]
    fn actual_chat_shortcut_route_requires_exact_keys_and_empty_input_owners() {
        for (name, expected) in [
            ("f1", Shortcut::Help),
            ("f2", Shortcut::Notifications),
            ("f3", Shortcut::Contacts),
            ("f4", Shortcut::Menu),
            ("f8", Shortcut::Admin),
            ("ctrl-b", Shortcut::Sidebar),
        ] {
            let event = key(name);
            assert_eq!(
                key_route(&event, context_allowed(Screen::Chat, true, &[false; 34])),
                Some(expected)
            );
            for index in 0..34 {
                let mut occupied = [false; 34];
                occupied[index] = true;
                assert_eq!(
                    key_route(&event, context_allowed(Screen::Chat, true, &occupied)),
                    None
                );
            }
            assert_eq!(
                key_route(
                    &event,
                    context_allowed(Screen::Password, true, &[false; 34])
                ),
                None
            );
            assert_eq!(
                key_route(&event, context_allowed(Screen::Chat, false, &[false; 34])),
                None
            );
            let mut held = event.clone();
            held.is_held = true;
            assert_eq!(key_route(&held, true), None);
            let mut ime = event;
            ime.prefer_character_input = true;
            assert_eq!(key_route(&ime, true), None);
        }
        for name in [
            "shift-f4",
            "ctrl-f4",
            "alt-f2",
            "ctrl-shift-b",
            "super-b",
            "enter",
            "space",
            "pageup",
            "pagedown",
            "escape",
            "ctrl-end",
        ] {
            assert_eq!(key_route(&key(name), true), None);
        }
    }
    #[test]
    fn actual_sidebar_reflow_preserves_anchor_and_never_restores_cleared_history() {
        let p = Position {
            anchor: 42,
            offset: 3,
            last_read: 45,
        };
        assert_eq!(
            reflow_position(p.clone(), &[40, 42, 45], false, true),
            Ok(Some(p.clone()))
        );
        assert_eq!(reflow_position(p.clone(), &[90], true, true), Ok(None));
        assert_eq!(reflow_position(p.clone(), &[], false, true), Ok(None));
        assert!(reflow_position(p.clone(), &[90], false, true).is_err());
        assert!(reflow_position(p.clone(), &[42], false, false).is_err());
        let tail = Position { offset: 0, ..p };
        assert_eq!(
            reflow_position(tail.clone(), &[42], false, true),
            Ok(Some(tail))
        );
        let mut shown = true;
        let mut revision = 7;
        let mut restoration = None;
        let kept = Position {
            anchor: 42,
            offset: 3,
            last_read: 45,
        };
        apply_sidebar_change(
            &mut shown,
            &mut revision,
            &mut restoration,
            Some(kept.clone()),
        );
        assert!(!shown);
        assert_eq!(revision, 8);
        assert_eq!(restoration, Some((kept, None)));
        // Cleared-feed transition cannot revive a hidden persistent anchor.
        apply_sidebar_change(&mut shown, &mut revision, &mut restoration, None);
        assert!(shown);
        assert_eq!(revision, 9);
        assert!(restoration.is_none());
        assert!(sidebar_shown(Screen::Chat, true));
        assert!(!sidebar_shown(Screen::Chat, false));
        assert!(sidebar_shown(Screen::Connect, false));
    }
    #[test]
    fn actual_chat_shortcut_command_table_has_no_send_or_credential_action() {
        for (name, command) in [
            ("f1", "/help"),
            ("f2", "/notifications"),
            ("f3", "/contacts"),
            ("f4", "/menu"),
            ("f8", "/admin"),
        ] {
            assert_eq!(
                key_route(&key(name), true).unwrap().command(),
                Some(command)
            );
        }
        assert_eq!(key_route(&key("ctrl-b"), true).unwrap().command(), None);
    }
    #[test]
    fn actual_local_poll_restores_two_sidebar_toggles_with_and_without_persistence() {
        use crate::draft_ui::{
            RestoreStep, local_restore_step as step, local_view_poll_plan as plan,
        };
        for persistence in [false, true] {
            assert_eq!(plan(Screen::Chat, persistence), (true, persistence));
            assert_eq!(plan(Screen::Auth, persistence), (false, false));
            let mut shown = true;
            let mut revision = 5;
            let mut layout = 5;
            let mut epoch = 10;
            let mut restoration = None;
            for _ in 0..2 {
                let p = Position {
                    anchor: 42,
                    offset: 2,
                    last_read: 45,
                };
                let capture = reflow_position(
                    p.clone(),
                    &[42, 45],
                    false,
                    restoration.is_none() && layout == revision,
                )
                .unwrap();
                apply_sidebar_change(&mut shown, &mut revision, &mut restoration, capture);
                let pending = |r: &Option<(Position, Option<u64>)>| r.as_ref().map(|(_, e)| *e);
                assert_eq!(
                    step(Screen::Chat, layout, revision, epoch, pending(&restoration)),
                    RestoreStep::Wait
                );
                layout = revision;
                epoch += 1;
                assert_eq!(
                    step(Screen::Chat, layout, revision, epoch, pending(&restoration)),
                    RestoreStep::Apply
                );
                restoration = Some((p.clone(), Some(epoch)));
                assert_eq!(
                    step(Screen::Chat, layout, revision, epoch, pending(&restoration)),
                    RestoreStep::Wait
                );
                epoch += 1;
                assert_eq!(
                    step(Screen::Chat, layout, revision, epoch, pending(&restoration)),
                    RestoreStep::Finish
                );
                restoration = None;
            }
            assert!(shown);
            let hidden = Position {
                anchor: 999,
                offset: 7,
                last_read: 1000,
            };
            let clear = reflow_position(hidden, &[42], true, true).unwrap();
            apply_sidebar_change(&mut shown, &mut revision, &mut restoration, clear);
            layout = revision;
            epoch += 1;
            assert!(restoration.is_none());
            assert_eq!(
                step(Screen::Chat, layout, revision, epoch, None),
                RestoreStep::Wait
            );
        }
    }
}
