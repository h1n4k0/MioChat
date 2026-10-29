use super::*;
use miochat_gpui::direct_message::FormPurpose;
impl MioChatApp {
    pub(super) fn activate_guess_action(
        &mut self,
        command: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        if miochat_gpui::guess::is_view(command) {
            if self.screen == Screen::Chat && self.direct_form.is_none() {
                self.run_command("/guess".into(), true, cx);
            }
            return true;
        }
        let start = command == "local:guess-start";
        let input = command.strip_prefix("local:guess-input ");
        if !start && input.is_none() {
            return false;
        }
        if self.screen != Screen::Chat || self.direct_form.is_some() || self.pending_send.is_some()
        {
            return true;
        }
        let Some(intent) = self
            .feature_panel
            .as_ref()
            .and_then(|p| miochat_gpui::guess::native_intent(command, p, &self.room))
        else {
            return true;
        };
        self.open_direct_form(cx);
        if let Some(f) = self.direct_form.as_mut() {
            f.purpose = FormPurpose::Guess(intent);
            if start {
                f.body = "100".into();
            }
            self.field = Field::DirectBody;
            self.reset_selection();
        }
        self.focus.focus(window, cx);
        cx.notify();
        true
    }
}
