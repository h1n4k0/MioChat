use super::*;
use miochat_gpui::{direct_message::FormPurpose, report::ReportIntent};
impl MioChatApp {
    pub(super) fn activate_report_action(
        &mut self,
        command: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        let head = command.split_whitespace().next().unwrap_or_default();
        if !matches!(head, "/report" | "/resolve") {
            return false;
        }
        if self.screen != Screen::Chat
            || self.direct_form.is_some()
            || self.word_start.is_some()
            || self.pending_send.is_some()
        {
            return true;
        }
        let intent = if head == "/resolve" {
            let Some(i) = ReportIntent::parse(command) else {
                self.run_command("/reports".into(), true, cx);
                return true;
            };
            i
        } else {
            ReportIntent::report_form(
                command
                    .split_whitespace()
                    .nth(1)
                    .and_then(|s| s.parse().ok())
                    .filter(|id| *id > 0)
                    .unwrap_or(0),
            )
        };
        self.open_direct_form(cx);
        if let Some(f) = self.direct_form.as_mut() {
            if intent.id > 0 {
                f.target = intent.id.to_string();
            }
            f.purpose = FormPurpose::Report(intent);
            self.field = direct_field(f.initial_slot());
            self.reset_selection();
        }
        self.focus.focus(window, cx);
        cx.notify();
        true
    }
}
