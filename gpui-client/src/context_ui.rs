use super::*;
pub(super) fn edit_id(command: &str) -> Option<i64> {
    let mut p = command.split_whitespace();
    if p.next()? != "/edit" {
        return None;
    }
    p.next()?.parse().ok().filter(|id| *id > 0)
}
pub(super) fn edit_response_admitted(r: &Response, p: Option<&PendingSend>) -> bool {
    let Some(p) = p.filter(|p| p.request_id == r.request_id && p.edit_message.is_some()) else {
        return true;
    };
    if r.kind == "error" {
        return miochat_gpui::information::is_plain_error_response(r);
    }
    // Edits return an acknowledgement only. MessageChanged arrives independently.
    let mut copy = r.clone();
    copy.kind = "error".into();
    copy.room.clear();
    copy.reply_to = 0;
    r.kind == "action"
        && r.room == p.room
        && miochat_gpui::information::is_plain_error_response(&copy)
}
pub(super) fn consume_edit_submission(
    composer: &mut String,
    submitted: &str,
    original_room: &str,
    current_room: &str,
) -> bool {
    original_room == current_room
        && miochat_gpui::direct_message::clear_unchanged_submission(composer, submitted)
}
impl MioChatApp {
    pub(super) fn activate_message_edit(
        &mut self,
        command: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        let Some(id) = edit_id(command) else {
            return false;
        };
        // Only the contextual two-token action opens the preloaded editor. Typed
        // /edit ID body still follows the original manual command route.
        if command.split_whitespace().count() != 2 {
            return false;
        }
        if self.screen != Screen::Chat
            || self.direct_form.is_some()
            || self.composition.is_some()
            || self.word_start.is_some()
            || self.pending_send.is_some()
            || self.profile_editor.is_some()
        {
            return true;
        }
        let Some(m) = self.messages.iter().find(|m| {
            m.id == id && m.room == self.room && m.sender == self.user && m.recalled == 0
        }) else {
            self.error = Some("该消息不在当前可见窗口；请先定位消息。".into());
            cx.notify();
            return true;
        };
        let body = m.body.clone();
        self.open_direct_form(cx);
        if let Some(f) = self.direct_form.as_mut() {
            f.purpose = miochat_gpui::direct_message::FormPurpose::EditMessage { id };
            f.body = body;
            self.field = Field::DirectBody;
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
    fn contextual_edit_keeps_fixed_id_and_original_chat_draft() {
        use miochat_gpui::direct_message::{DirectMessageForm, FormPurpose};
        let mut f = DirectMessageForm::new("alice".into(), "lobby".into(), 1);
        f.purpose = FormPurpose::EditMessage { id: 42 };
        f.body = "原文🙂".into();
        assert!(!f.has_target());
        assert_eq!(f.command("alice", "lobby", 1).unwrap(), "/edit 42 原文🙂");
        assert!(f.command("alice", "elsewhere", 1).is_err());
        f.begin("edit1".into());
        f.body.push('新');
        assert!(!f.finish("edit1", "alice", 1, true));
        assert_eq!(f.body, "原文🙂新");
    }
    #[test]
    fn contextual_edit_ack_rejects_siblings_and_old_scope() {
        let p = PendingSend::command("id".into(), "lobby".into(), "/edit 42 text", String::new());
        let mut r = Response {
            kind: "action".into(),
            request_id: "id".into(),
            room: "lobby".into(),
            ..Response::default()
        };
        assert!(edit_response_admitted(&r, Some(&p)));
        r.profile = Some(Profile::default());
        assert!(!edit_response_admitted(&r, Some(&p)));
        r.profile = None;
        r.room = "other".into();
        assert!(!edit_response_admitted(&r, Some(&p)));
    }
    #[test]
    fn contextual_edit_manual_ack_consumes_only_unchanged_same_scope_command() {
        let mut draft = "/edit 42 new".to_string();
        assert!(!consume_edit_submission(
            &mut draft,
            "/edit 42 new",
            "lobby",
            "other"
        ));
        assert!(!consume_edit_submission(&mut draft, "", "lobby", "lobby"));
        assert!(!consume_edit_submission(
            &mut draft,
            "/edit 42 old",
            "lobby",
            "lobby"
        ));
        assert!(consume_edit_submission(
            &mut draft,
            "/edit 42 new",
            "lobby",
            "lobby"
        ));
        assert!(draft.is_empty());
    }
}
