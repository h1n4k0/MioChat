use super::*;
use miochat_gpui::{direct_message::FormPurpose, management::ManagementIntent};
impl MioChatApp {
    pub(super) fn activate_management_action(
        &mut self,
        command: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        let fields = command.split_whitespace().collect::<Vec<_>>();
        let head = fields.first().copied().unwrap_or_default();
        if !matches!(head, "/slowmode" | "/kick" | "/role") {
            return false;
        }
        if self.screen != Screen::Chat
            || self.direct_form.is_some()
            || self.word_start.is_some()
            || self.pending_send.is_some()
        {
            return true;
        }
        let Some(intent) = ManagementIntent::form(head, &self.room) else {
            self.error = Some("慢速模式请在实际频道内设置。".into());
            cx.notify();
            return true;
        };
        self.open_direct_form(cx);
        if let Some(f) = self.direct_form.as_mut() {
            f.purpose = FormPurpose::Management(intent);
            f.target = fields
                .get(1)
                .copied()
                .unwrap_or(if head == "/slowmode" { "0" } else { "" })
                .into();
            f.extra = fields.get(2).copied().unwrap_or("member").into();
            f.body.clear();
            self.field = Field::DirectTarget;
            self.reset_selection();
        }
        self.focus.focus(window, cx);
        cx.notify();
        true
    }
}
