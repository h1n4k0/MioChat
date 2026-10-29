use super::*;
use miochat_gpui::{direct_message::FormPurpose, sanctions::SanctionIntent};
// Shared by the actual pointer and keyboard routes; no text or submit side effects.
fn force_value<'a>(
    form: Option<&'a mut miochat_gpui::direct_message::DirectMessageForm>,
    active_input: bool,
    composing: bool,
    binding: (&str, &str, u64),
) -> Option<&'a mut bool> {
    let f = form?;
    if !active_input || composing || f.pending() || !f.bound_to(binding.0, binding.1, binding.2) {
        return None;
    }
    let FormPurpose::Sanction(i) = &mut f.purpose else {
        return None;
    };
    if i.command != "/banip" {
        return None;
    }
    i.self_ip_force.as_mut()
}
fn toggle_force_form(
    form: Option<&mut miochat_gpui::direct_message::DirectMessageForm>,
    active_input: bool,
    composing: bool,
    binding: (&str, &str, u64),
) -> bool {
    let Some(force) = force_value(form, active_input, composing, binding) else {
        return false;
    };
    *force = !*force;
    true
}
pub(super) fn handle_force_key(
    event: &KeyDownEvent,
    active_input: bool,
    composing: bool,
    form: Option<&mut miochat_gpui::direct_message::DirectMessageForm>,
    binding: (&str, &str, u64),
) -> bool {
    let m = event.keystroke.modifiers;
    if event.prefer_character_input
        || !event.keystroke.key.eq_ignore_ascii_case("f")
        || !m.control
        || !m.shift
        || m.alt
        || m.platform
        || m.function
    {
        return false;
    }
    let Some(force) = force_value(form, active_input, composing, binding) else {
        return false;
    };
    // Consume key repeat without oscillating a safety confirmation.
    if !event.is_held {
        *force = !*force;
    }
    true
}
impl MioChatApp {
    pub(super) fn toggle_sanction_force(&mut self, cx: &mut Context<Self>) {
        let active = self.direct_input_active();
        if toggle_force_form(
            self.direct_form.as_mut(),
            active,
            self.marked_range.is_some(),
            (&self.user, &self.room, self.generation),
        ) {
            cx.notify();
        }
    }
    pub(super) fn activate_sanction_action(
        &mut self,
        command: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        let head = command.split_whitespace().next().unwrap_or_default();
        if !matches!(
            head,
            "/mute" | "/unmute" | "/gmute" | "/ungmute" | "/ban" | "/unban" | "/banip" | "/unbanip"
        ) {
            return false;
        }
        if self.screen != Screen::Chat
            || self.direct_form.is_some()
            || self.word_start.is_some()
            || self.pending_send.is_some()
        {
            return true;
        }
        let Some(intent) = SanctionIntent::form(head, &self.room) else {
            self.error = Some("频道禁言请在频道内执行。".into());
            cx.notify();
            return true;
        };
        self.open_direct_form(cx);
        if let Some(f) = self.direct_form.as_mut() {
            f.purpose = FormPurpose::Sanction(intent);
            let parts: Vec<_> = command.split_whitespace().collect();
            f.target = parts.get(1).copied().unwrap_or_default().into();
            f.extra = parts.get(2).copied().unwrap_or("permanent").into();
            f.body = "管理员操作".into();
            self.field = direct_field(f.initial_slot());
            self.reset_selection();
        }
        self.focus.focus(window, cx);
        cx.notify();
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use miochat_gpui::direct_message::DirectMessageForm;
    fn form() -> DirectMessageForm {
        let mut f = DirectMessageForm::new("owner".into(), "lobby".into(), 3);
        f.purpose = FormPurpose::Sanction(SanctionIntent::form("/banip", "lobby").unwrap());
        f.target = "192.0.2.0/24".into();
        f.extra = "1h".into();
        f.body = "synthetic".into();
        f
    }
    fn key() -> KeyDownEvent {
        KeyDownEvent {
            keystroke: gpui::Keystroke {
                key: "f".into(),
                modifiers: gpui::Modifiers {
                    control: true,
                    shift: true,
                    ..Default::default()
                },
                ..Default::default()
            },
            is_held: false,
            prefer_character_input: false,
        }
    }
    #[test]
    fn actual_force_key_route_is_bound_composition_safe_and_never_submits() {
        let mut f = form();
        let original = (f.target.clone(), f.extra.clone(), f.body.clone());
        assert!(handle_force_key(
            &key(),
            true,
            false,
            Some(&mut f),
            ("owner", "lobby", 3)
        ));
        assert_eq!(f.sanction_intent().unwrap().self_ip_force, Some(true));
        let mut held = key();
        held.is_held = true;
        assert!(handle_force_key(
            &held,
            true,
            false,
            Some(&mut f),
            ("owner", "lobby", 3)
        ));
        assert_eq!(f.sanction_intent().unwrap().self_ip_force, Some(true));
        assert!(!f.pending());
        assert_eq!(
            (&f.target, &f.extra, &f.body),
            (&original.0, &original.1, &original.2)
        );
        assert!(handle_force_key(
            &key(),
            true,
            false,
            Some(&mut f),
            ("owner", "lobby", 3)
        ));
        assert_eq!(f.sanction_intent().unwrap().self_ip_force, Some(false));
        for (active, composing, owner, room, generation) in [
            (false, false, "owner", "lobby", 3),
            (true, true, "owner", "lobby", 3),
            (true, false, "other", "lobby", 3),
            (true, false, "owner", "elsewhere", 3),
            (true, false, "owner", "lobby", 4),
        ] {
            let before = f.clone();
            assert!(!handle_force_key(
                &key(),
                active,
                composing,
                Some(&mut f),
                (owner, room, generation)
            ));
            assert_eq!(f, before);
        }
        for (screen, field) in [
            (Screen::Password, Field::Password),
            (Screen::Connect, Field::Host),
            (Screen::Chat, Field::RoomTarget),
        ] {
            let active = routes_direct_input(screen, field, Some(&f), "owner", 3);
            let before = f.clone();
            assert!(!handle_force_key(
                &key(),
                active,
                false,
                Some(&mut f),
                ("owner", "lobby", 3)
            ));
            assert_eq!(f, before);
        }
        for word in ["space", "enter", "g"] {
            let mut k = key();
            k.keystroke.key = word.into();
            let before = f.clone();
            assert!(!handle_force_key(
                &k,
                true,
                false,
                Some(&mut f),
                ("owner", "lobby", 3)
            ));
            assert_eq!(f, before);
        }
        for n in 0..5 {
            let mut k = key();
            match n {
                0 => k.keystroke.modifiers.shift = false,
                1 => k.keystroke.modifiers.alt = true,
                2 => k.keystroke.modifiers.platform = true,
                3 => k.keystroke.modifiers.function = true,
                _ => k.prefer_character_input = true,
            }
            let before = f.clone();
            assert!(!handle_force_key(
                &k,
                true,
                false,
                Some(&mut f),
                ("owner", "lobby", 3)
            ));
            assert_eq!(f, before);
        }
        assert!(f.begin("pending".into()));
        let before = f.clone();
        assert!(!handle_force_key(
            &key(),
            true,
            false,
            Some(&mut f),
            ("owner", "lobby", 3)
        ));
        assert_eq!(f, before);
        let mut other = form();
        other.purpose = FormPurpose::Direct;
        let before = other.clone();
        assert!(!handle_force_key(
            &key(),
            true,
            false,
            Some(&mut other),
            ("owner", "lobby", 3)
        ));
        assert_eq!(other, before);
        for command in ["/ban", "/unbanip"] {
            let mut other = form();
            other.purpose = FormPurpose::Sanction(SanctionIntent::form(command, "lobby").unwrap());
            let before = other.clone();
            assert!(!handle_force_key(
                &key(),
                true,
                false,
                Some(&mut other),
                ("owner", "lobby", 3)
            ));
            assert_eq!(other, before);
        }
    }
}
