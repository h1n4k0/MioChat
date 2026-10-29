//! Connection-bound message-action editor. Never contains the chat draft.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum FormPurpose {
    Direct,
    EditMessage { id: i64 },
    SecretAdmin(crate::secret_admin::Kind),
    Configuration(crate::configuration::ConfigurationIntent),
    Management(crate::management::ManagementIntent),
    Sanction(crate::sanctions::SanctionIntent),
    Report(crate::report::ReportIntent),
    Moderation(crate::moderation::ReadIntent),
    Word(crate::word::WordIntent),
    Guess(crate::guess::GuessIntent),
    Tic(crate::tic::TicIntent),
    Simple(crate::simple_actions::SimpleAction),
    Community(crate::community_mutation::CommunityIntent),
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FormSlot {
    Target,
    Extra,
    Body,
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DirectMessageForm {
    pub purpose: FormPurpose,
    pub extra: String,
    pub owner: String,
    pub room: String,
    pub generation: u64,
    pub target: String,
    pub body: String,
    pending: Option<(String, String, String, String, FormPurpose)>,
}

impl DirectMessageForm {
    pub fn new(owner: String, room: String, generation: u64) -> Self {
        Self {
            purpose: FormPurpose::Direct,
            extra: String::new(),
            owner,
            room,
            generation,
            target: String::new(),
            body: String::new(),
            pending: None,
        }
    }
    pub fn has_body(&self) -> bool {
        match &self.purpose {
            FormPurpose::SecretAdmin(k) => *k == crate::secret_admin::Kind::Invitecode,
            FormPurpose::Direct
            | FormPurpose::EditMessage { .. }
            | FormPurpose::Configuration(_) => true,
            FormPurpose::Management(_) => false,
            FormPurpose::Sanction(i) => !i.remove,
            FormPurpose::Report(i) => i.has_body(),
            FormPurpose::Guess(_) | FormPurpose::Word(_) => true,
            FormPurpose::Tic(_) | FormPurpose::Moderation(_) => false,
            FormPurpose::Simple(a) => a.has_body(),
            FormPurpose::Community(i) => i.action.has_body(),
        }
    }
    pub fn secret_admin_kind(&self) -> Option<crate::secret_admin::Kind> {
        if let FormPurpose::SecretAdmin(kind) = self.purpose {
            Some(kind)
        } else {
            None
        }
    }
    pub fn configuration_intent(&self) -> Option<&crate::configuration::ConfigurationIntent> {
        if let FormPurpose::Configuration(i) = &self.purpose {
            Some(i)
        } else {
            None
        }
    }
    pub fn management_intent(&self) -> Option<&crate::management::ManagementIntent> {
        if let FormPurpose::Management(i) = &self.purpose {
            Some(i)
        } else {
            None
        }
    }
    pub fn report_intent(&self) -> Option<&crate::report::ReportIntent> {
        if let FormPurpose::Report(i) = &self.purpose {
            Some(i)
        } else {
            None
        }
    }
    pub fn sanction_submission(
        &self,
        chat_screen: bool,
        owner: &str,
        room: &str,
        generation: u64,
        command: &str,
    ) -> Option<crate::sanctions::SanctionIntent> {
        let intent = self.sanction_intent()?;
        if !chat_screen
            || intent.command != "/banip" && intent.self_ip_force.is_some()
            || self.command(owner, room, generation).ok().as_deref() != Some(command)
        {
            return None;
        }
        let mut submitted = crate::sanctions::SanctionIntent::parse(command, room)?;
        if submitted.command != intent.command || submitted.room != intent.room {
            return None;
        }
        submitted.self_ip_force = intent.self_ip_force;
        Some(submitted)
    }
    pub fn sanction_intent(&self) -> Option<&crate::sanctions::SanctionIntent> {
        if let FormPurpose::Sanction(i) = &self.purpose {
            Some(i)
        } else {
            None
        }
    }
    pub fn extra_label(&self) -> &'static str {
        if self.management_intent().is_some() {
            "角色：admin / moderator / member"
        } else if self.sanction_intent().is_some() {
            "期限：10m / 7d / permanent"
        } else {
            "北京时间 YYYY-MM-DD HH:MM"
        }
    }
    pub fn is_direct(&self) -> bool {
        matches!(self.purpose, FormPurpose::Direct)
    }
    pub fn simple_action(&self) -> Option<crate::simple_actions::SimpleAction> {
        if let FormPurpose::Simple(a) = self.purpose {
            Some(a)
        } else {
            None
        }
    }
    pub fn community_intent(&self) -> Option<&crate::community_mutation::CommunityIntent> {
        if let FormPurpose::Community(i) = &self.purpose {
            Some(i)
        } else {
            None
        }
    }
    pub fn tic_intent(&self) -> Option<&crate::tic::TicIntent> {
        if let FormPurpose::Tic(i) = &self.purpose {
            Some(i)
        } else {
            None
        }
    }
    pub fn public_word_submission(
        &self,
        owner: &str,
        room: &str,
        generation: u64,
        command: &str,
    ) -> Option<crate::word::WordIntent> {
        let intent = self.word_intent()?;
        if intent.start
            || intent.object_id <= 0
            || intent.room.is_empty()
            || self.command(owner, room, generation).ok().as_deref() != Some(command)
        {
            return None;
        }
        intent.bind_submission(command)
    }
    pub fn word_intent(&self) -> Option<&crate::word::WordIntent> {
        if let FormPurpose::Word(i) = &self.purpose {
            Some(i)
        } else {
            None
        }
    }
    pub fn guess_intent(&self) -> Option<&crate::guess::GuessIntent> {
        if let FormPurpose::Guess(i) = &self.purpose {
            Some(i)
        } else {
            None
        }
    }
    pub fn has_target(&self) -> bool {
        matches!(&self.purpose,FormPurpose::Report(i) if i.has_body())
            || matches!(
                self.purpose,
                FormPurpose::SecretAdmin(_)
                    | FormPurpose::Moderation(_)
                    | FormPurpose::Sanction(_)
                    | FormPurpose::Management(_)
                    | FormPurpose::Configuration(_)
            )
            || self.is_direct()
            || self
                .tic_intent()
                .is_some_and(|i| i.action == crate::tic::TicAction::Challenge)
            || self.community_intent().is_some_and(|i| i.action.creates())
    }
    pub fn has_extra(&self) -> bool {
        self.management_intent()
            .is_some_and(|i| i.command == "/role")
            || self.sanction_intent().is_some_and(|i| !i.remove)
            || self
                .community_intent()
                .is_some_and(|i| i.action == crate::community_mutation::CommunityAction::Event)
    }
    pub fn title(&self) -> &'static str {
        match &self.purpose {
            FormPurpose::Direct => "发送一次私信",
            FormPurpose::EditMessage { .. } => "保存消息编辑",
            FormPurpose::SecretAdmin(k) => k.title(),
            FormPurpose::Configuration(i) => i.title(),
            FormPurpose::Management(i) => i.title(),
            FormPurpose::Sanction(i) => i.title(),
            FormPurpose::Report(i) => i.title(),
            FormPurpose::Moderation(i) => i.title(),
            FormPurpose::Word(_) => "猜字/词（频道可见）",
            FormPurpose::Guess(i) => i.title(),
            FormPurpose::Tic(i) => i.title(),
            FormPurpose::Simple(a) => a.title(),
            FormPurpose::Community(i) => i.action.title(),
        }
    }
    pub fn target_label(&self) -> &'static str {
        if let Some(kind) = self.secret_admin_kind() {
            if kind == crate::secret_admin::Kind::Resetpass {
                "目标账户或 #UID（将撤销其旧凭据）"
            } else {
                "期限：1d / 7d / permanent"
            }
        } else if let Some(intent) = self.configuration_intent() {
            intent.target_label()
        } else if let Some(intent) = self.management_intent() {
            intent.target_label()
        } else if let Some(intent) = self.sanction_intent() {
            intent.target_label()
        } else if matches!(self.purpose, FormPurpose::Report(_)) {
            "消息编号（管理员仅可读此条）"
        } else if let FormPurpose::Moderation(i) = &self.purpose {
            i.label()
        } else if self.tic_intent().is_some() {
            "挑战对手用户名或 #UID"
        } else if self.is_direct() {
            "收件人用户名"
        } else {
            "标题（最多100字符）"
        }
    }
    pub fn body_label(&self) -> &'static str {
        match &self.purpose {
            FormPurpose::Direct => "私信内容",
            FormPurpose::EditMessage { .. } => "消息正文（编号固定，聊天草稿不变）",
            FormPurpose::SecretAdmin(_) => "使用次数：1–100",
            FormPurpose::Configuration(i) => i.body_label(),
            FormPurpose::Management(_) => "",
            FormPurpose::Sanction(_) => "管理原因（最多240字符）",
            FormPurpose::Report(_) => "举报原因（最多240字符）",
            FormPurpose::Word(_) => "猜字/词（频道可见）",
            FormPurpose::Guess(i) => i.body_label(),
            FormPurpose::Tic(_) | FormPurpose::Moderation(_) => "",
            FormPurpose::Simple(a) => a.input_label(),
            FormPurpose::Community(i) => i.action.body_label(),
        }
    }
    pub fn slot_enabled(&self, slot: FormSlot) -> bool {
        match slot {
            FormSlot::Target => self.has_target(),
            FormSlot::Extra => self.has_extra(),
            FormSlot::Body => self.has_body() || !self.has_target() && !self.has_extra(),
        }
    }
    pub fn initial_slot(&self) -> FormSlot {
        if self.has_target() {
            FormSlot::Target
        } else {
            FormSlot::Body
        }
    }
    pub fn next_slot(&self, slot: FormSlot, reverse: bool) -> FormSlot {
        let mut fields = Vec::new();
        if self.has_target() {
            fields.push(FormSlot::Target)
        }
        if self.has_extra() {
            fields.push(FormSlot::Extra)
        }
        if self.slot_enabled(FormSlot::Body) {
            fields.push(FormSlot::Body);
        }
        let index = fields.iter().position(|s| *s == slot).unwrap_or(0);
        fields[(index + if reverse { fields.len() - 1 } else { 1 }) % fields.len()]
    }
    pub fn value(&self, slot: FormSlot) -> &str {
        match slot {
            FormSlot::Target => &self.target,
            FormSlot::Extra => &self.extra,
            FormSlot::Body => &self.body,
        }
    }
    pub fn value_mut(&mut self, slot: FormSlot) -> &mut String {
        match slot {
            FormSlot::Target => &mut self.target,
            FormSlot::Extra => &mut self.extra,
            FormSlot::Body => &mut self.body,
        }
    }
    pub fn limit(&self, slot: FormSlot) -> usize {
        match slot {
            FormSlot::Target => {
                if matches!(self.purpose, FormPurpose::Moderation(_)) {
                    1600
                } else if self.is_direct() || self.tic_intent().is_some() {
                    64
                } else {
                    100
                }
            }
            FormSlot::Extra => 16,
            FormSlot::Body => {
                if self.secret_admin_kind().is_some() {
                    3
                } else if self.configuration_intent().is_some() {
                    800
                } else if let Some(i) = self.sanction_intent() {
                    if i.remove { 0 } else { 240 }
                } else if let FormPurpose::Report(i) = &self.purpose {
                    if i.has_body() { 240 } else { 0 }
                } else if self.guess_intent().is_some() || self.word_intent().is_some() {
                    24
                } else if self
                    .community_intent()
                    .is_some_and(|i| i.action == crate::community_mutation::CommunityAction::Poll)
                {
                    810
                } else if self.has_body() {
                    800
                } else {
                    0
                }
            }
        }
    }
    pub fn bound_to(&self, owner: &str, room: &str, generation: u64) -> bool {
        !owner.is_empty()
            && self.owner == owner
            && self.room == room
            && self.generation == generation
    }
    pub fn routes_input(
        &self,
        chat_screen: bool,
        direct_field: bool,
        owner: &str,
        generation: u64,
    ) -> bool {
        chat_screen
            && direct_field
            && !owner.is_empty()
            && self.owner == owner
            && self.generation == generation
    }
    pub fn pending(&self) -> bool {
        self.pending.is_some()
    }
    pub fn command(
        &self,
        owner: &str,
        room: &str,
        generation: u64,
    ) -> Result<String, &'static str> {
        if !self.bound_to(owner, room, generation) {
            return Err("原账户或对话已变化；内容未发送，请返回原对话核对。");
        }
        if self.pending() {
            return Err("正在发送，请勿重复提交。");
        }
        match &self.purpose {
            FormPurpose::SecretAdmin(kind) => return kind.command(&self.target, &self.body),
            FormPurpose::Configuration(intent) => {
                return intent.form_command(&self.target, &self.body);
            }
            FormPurpose::Management(intent) => {
                return intent.form_command(&self.target, &self.extra);
            }
            FormPurpose::Sanction(intent) => {
                return intent.form_command(&self.target, &self.extra, &self.body);
            }
            FormPurpose::Report(intent) => return intent.command(&self.target, &self.body),
            FormPurpose::Moderation(intent) => return intent.form_command(&self.target),
            FormPurpose::Word(intent) => return intent.command(&self.body),
            FormPurpose::Guess(intent) => return intent.command(&self.body),
            FormPurpose::Tic(intent) => return intent.command(&self.target),
            FormPurpose::Simple(action) => return action.command(&self.body),
            FormPurpose::Community(intent) => {
                return intent.command(&self.target, &self.extra, &self.body);
            }
            FormPurpose::EditMessage { id } => {
                if *id <= 0 || self.body.trim().is_empty() || self.body.chars().count() > 800 {
                    return Err("消息不能为空且最多800字符");
                }
                return Ok(format!("/edit {id} {}", self.body));
            }
            FormPurpose::Direct => {}
        }
        let target = self.target.trim();
        if target.is_empty() || target.chars().any(char::is_whitespace) {
            return Err("请输入一个完整用户名（不是房间或 UID）。");
        }
        if target == owner {
            return Err("不能给自己发私信。");
        }
        if self.body.trim().is_empty() || self.body.chars().count() > 800 {
            return Err("消息不能为空，且最多 800 个字符。");
        }
        Ok(format!("/msg {target} {}", self.body))
    }
    pub fn begin(&mut self, id: String) -> bool {
        if id.is_empty() || self.pending() {
            return false;
        }
        self.pending = Some((
            id,
            self.target.clone(),
            self.body.clone(),
            self.extra.clone(),
            self.purpose.clone(),
        ));
        true
    }
    /// Returns true only if a matching success may close this unchanged form.
    /// A late success cannot erase newly edited text; errors retain all input.
    pub fn finish(&mut self, id: &str, owner: &str, generation: u64, success: bool) -> bool {
        if self.owner != owner
            || self.generation != generation
            || self.pending.as_ref().is_none_or(|p| p.0 != id)
        {
            return false;
        }
        let (_, target, body, extra, purpose) = self.pending.take().unwrap();
        success
            && self.target == target
            && self.body == body
            && self.extra == extra
            && self.purpose == purpose
    }
}

/// Credential/connect screens cannot retain an editable or pending DM buffer.
/// The caller also cancels its matching pending action so a late ACK is ignored.
pub fn discard_outside_chat(form: &mut Option<DirectMessageForm>, chat_screen: bool) {
    if !chat_screen {
        *form = None;
    }
}

pub fn message_belongs_to_view(message_room: &str, current_room: &str) -> bool {
    !current_room.is_empty() && message_room == current_room
}

/// Empty submitted text denotes a separate native form, never the chat draft.
pub fn clear_unchanged_submission(composer: &mut String, submitted: &str) -> bool {
    if !submitted.is_empty() && composer == submitted {
        composer.clear();
        true
    } else {
        false
    }
}

/// A generic action acknowledgement must not consume one-off input unless it
/// confirms the intended sender, recipient and original conversation scope.
pub fn accepts_one_off_ack(
    response: &crate::session::Response,
    owner: &str,
    target: &str,
    origin_room: &str,
) -> bool {
    !owner.is_empty()
        && !target.is_empty()
        && !origin_room.is_empty()
        && response.room == origin_room
        && !response.replace
        && !response.clear_view
        && response.message.as_ref().is_some_and(|m| {
            m.id > 0
                && m.sender == owner
                && response.direct_target.as_ref().is_some_and(|t| {
                    t.requested == target && !t.canonical.is_empty() && t.canonical == m.recipient
                })
                && m.room == format!("dm:{}", m.recipient)
        })
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn reset_interrupt_discards_form_and_cannot_route_password_or_reanimate_on_ack() {
        let mut f = form();
        f.begin("old-send".into());
        // Defense in depth: even if an obsolete buffer were retained, a
        // credential screen or Password field never routes into its content.
        assert!(!f.routes_input(false, true, "alice", 7));
        assert!(!f.routes_input(true, false, "alice", 7));
        let mut active = Some(f);
        discard_outside_chat(&mut active, false);
        assert!(active.is_none());
        assert!(
            !active
                .as_mut()
                .is_some_and(|f| f.finish("old-send", "alice", 7, true))
        );
        let password = "synthetic-new-password";
        assert!(active.is_none());
        assert_eq!(password, "synthetic-new-password");
    }
    #[test]
    fn scope_fallback_freezes_unicode_input_and_normalizes_shared_cursor_for_copy() {
        let f = form();
        let fallback_draft = "a";
        let selection =
            crate::editing::valid_selection(&f.body, fallback_draft.len()..fallback_draft.len());
        assert!(!f.bound_to("alice", "other-room", 7)); // UI blocks edits/arrows while frozen
        assert!(f.body.get(selection.clone()).is_some());
        assert!(
            f.body
                .is_char_boundary(crate::editing::previous(&f.body, selection.start))
        );
        assert!(
            f.body
                .is_char_boundary(crate::editing::next(&f.body, selection.end))
        );
        assert_eq!(f.body, "独立私信\n第二行");
    }
    #[test]
    fn native_form_ack_and_late_typed_ack_preserve_the_current_draft() {
        let mut draft = "原对话的未发送草稿".to_string();
        assert!(!clear_unchanged_submission(&mut draft, ""));
        assert!(!clear_unchanged_submission(
            &mut draft,
            "/msg bob previous text"
        ));
        assert_eq!(draft, "原对话的未发送草稿");
        draft = "/msg bob confirmed typed command".into();
        assert!(clear_unchanged_submission(
            &mut draft,
            "/msg bob confirmed typed command"
        ));
        assert!(draft.is_empty());
    }
    #[test]
    fn one_off_ack_must_confirm_the_original_recipient_not_just_request_scope() {
        use crate::session::{DirectTarget, Message, Response};
        let good = Response {
            direct_target: Some(DirectTarget {
                requested: "BOB".into(),
                canonical: "bob".into(),
            }),
            room: "lobby".into(),
            message: Some(Message {
                id: 17,
                sender: "alice".into(),
                recipient: "bob".into(),
                room: "dm:bob".into(),
                ..Message::default()
            }),
            ..Response::default()
        };
        assert!(accepts_one_off_ack(&good, "alice", "BOB", "lobby"));
        assert!(!accepts_one_off_ack(&good, "alice", "carol", "lobby"));
        assert!(!accepts_one_off_ack(&good, "mallory", "BOB", "lobby"));
        assert!(!accepts_one_off_ack(&good, "alice", "BOB", "dm:carol"));
        for response in [
            Response {
                message: None,
                ..good.clone()
            },
            Response {
                replace: true,
                ..good.clone()
            },
            Response {
                clear_view: true,
                ..good.clone()
            },
        ] {
            assert!(!accepts_one_off_ack(&response, "alice", "BOB", "lobby"));
        }
        let unicode = Response {
            direct_target: Some(DirectTarget {
                requested: "İuser".into(),
                canonical: "iuser".into(),
            }),
            message: Some(Message {
                id: 18,
                sender: "alice".into(),
                recipient: "iuser".into(),
                room: "dm:iuser".into(),
                ..Message::default()
            }),
            ..good
        };
        assert!(accepts_one_off_ack(&unicode, "alice", "İuser", "lobby"));
    }
    fn form() -> DirectMessageForm {
        let mut f = DirectMessageForm::new("alice".into(), "lobby".into(), 7);
        f.target = "bob".into();
        f.body = "独立私信\n第二行".into();
        f
    }
    #[test]
    fn one_off_form_binds_owner_scope_generation_and_keeps_chat_draft_separate() {
        let chat_draft = "原房间未发送草稿";
        let f = form();
        assert_eq!(
            f.command("alice", "lobby", 7).unwrap(),
            "/msg bob 独立私信\n第二行"
        );
        for (owner, room, generation) in [
            ("mallory", "lobby", 7),
            ("alice", "dm:other", 7),
            ("alice", "lobby", 8),
        ] {
            assert!(f.command(owner, room, generation).is_err());
        }
        assert_eq!(chat_draft, "原房间未发送草稿");
        assert!(!message_belongs_to_view("dm:bob", "lobby"));
        assert!(!message_belongs_to_view("dm:bob", "dm:other"));
        assert!(message_belongs_to_view("dm:bob", "dm:bob"));
    }
    #[test]
    fn errors_duplicates_old_ack_and_new_edits_do_not_erase_input() {
        let mut f = form();
        assert!(f.begin("one".into()));
        assert!(!f.begin("two".into()));
        assert!(f.command("alice", "lobby", 7).is_err());
        assert!(!f.finish("old", "alice", 7, true));
        assert!(!f.finish("one", "other", 7, true));
        assert!(!f.finish("one", "alice", 8, true));
        assert!(f.pending());
        assert!(!f.finish("one", "alice", 7, false));
        assert_eq!(f.body, "独立私信\n第二行");
        assert!(f.begin("two".into()));
        f.body = "较新的输入".into();
        assert!(!f.finish("two", "alice", 7, true));
        assert_eq!(f.body, "较新的输入");
        assert!(!f.pending());
        assert!(f.begin("three".into()));
        assert!(f.finish("three", "alice", 7, true));
    }
    #[test]
    fn invalid_recipient_and_body_never_form_a_command() {
        for target in ["", " ", "two users", "alice", "bob\n/clear"] {
            let mut f = form();
            f.target = target.into();
            assert!(f.command("alice", "lobby", 7).is_err());
        }
        for body in [" ".to_string(), "字".repeat(801)] {
            let mut f = form();
            f.body = body;
            assert!(f.command("alice", "lobby", 7).is_err());
        }
        let mut f = form();
        f.body = "字".repeat(800);
        assert!(f.command("alice", "lobby", 7).is_ok());
    }
}
