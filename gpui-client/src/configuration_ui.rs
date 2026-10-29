use super::*;
use miochat_gpui::{
    configuration::{ConfigurationIntent, split_argument},
    direct_message::{DirectMessageForm, FormPurpose},
};
fn configuration_action_form(
    command: &str,
    owner: &str,
    room: &str,
    generation: u64,
) -> Option<DirectMessageForm> {
    let (head, args) = split_argument(command);
    let intent = ConfigurationIntent::form(head)?;
    let (target, body) = split_argument(args);
    let mut form = DirectMessageForm::new(owner.into(), room.into(), generation);
    form.purpose = FormPurpose::Configuration(intent);
    form.target = if target.is_empty() {
        if head == "/maintenance" {
            "off"
        } else {
            "registration"
        }
    } else {
        target
    }
    .into();
    form.body = body.into();
    Some(form)
}
impl MioChatApp {
    pub(super) fn activate_configuration_action(
        &mut self,
        command: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        let Some(prepared) =
            configuration_action_form(command, &self.user, &self.room, self.generation)
        else {
            return false;
        };
        if self.screen != Screen::Chat
            || self.direct_form.is_some()
            || self.word_start.is_some()
            || self.pending_send.is_some()
        {
            return true;
        }
        self.open_direct_form(cx);
        if let Some(f) = self.direct_form.as_mut() {
            *f = prepared;
            self.field = Field::DirectTarget;
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
    #[test]
    fn actual_configuration_menu_prefill_and_credential_routes_keep_input_separate() {
        let form = configuration_action_form("/maintenance on 中文 多词公告", "owner", "lobby", 7)
            .unwrap();
        assert_eq!(form.target, "on");
        assert_eq!(form.body, "中文 多词公告");
        assert!(form.extra.is_empty());
        assert_eq!(
            form.command("owner", "lobby", 7).unwrap(),
            "/maintenance on 中文 多词公告"
        );
        assert!(routes_direct_input(
            Screen::Chat,
            Field::DirectBody,
            Some(&form),
            "owner",
            7
        ));
        for screen in [
            Screen::Password,
            Screen::Auth,
            Screen::KeyBinding,
            Screen::Connect,
            Screen::Trust,
        ] {
            assert!(!routes_direct_input(
                screen,
                Field::DirectBody,
                Some(&form),
                "owner",
                7
            ));
        }
        assert!(!routes_direct_input(
            Screen::Chat,
            Field::Password,
            Some(&form),
            "owner",
            7
        ));
        let mut held = Some(form);
        miochat_gpui::direct_message::discard_outside_chat(&mut held, false);
        assert!(held.is_none());
        let clear =
            configuration_action_form("/config maintenance_notice", "owner", "lobby", 7).unwrap();
        assert!(clear.body.is_empty());
        assert_eq!(
            clear.command("owner", "lobby", 7).unwrap(),
            "/config maintenance_notice"
        );
        assert!(configuration_action_form("/configx", "owner", "lobby", 7).is_none());
    }
}
