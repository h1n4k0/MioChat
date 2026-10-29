//! Native custom-image recent picker and keyboard access to attachment actions.
//! No image bytes or composer contents are stored by this module.
use super::*;
use miochat_gpui::attachments::{
    Binding, LocalAction, Operation, Phase as UploadPhase, Purpose, Receipt,
};
use miochat_gpui::emoji_attachments::{Catalog, Phase, ReferenceAction, ReferenceTicket};
#[derive(Clone, Debug, PartialEq, Eq)]
enum KeyAction {
    ChooseRecent(i64),
    CloseRecent,
    UploadIntent,
    UploadCommit,
    UploadStatus,
    UploadCancel,
    UploadResume,
    UploadClear,
    UploadForgetUnknown,
    ReferenceConfirm,
    ReferenceStatus,
    ReferenceClear,
}
impl KeyAction {
    fn label(&self) -> String {
        match self {
            Self::ChooseRecent(id) => format!("预览最近图片 #{id}（尚不发送）"),
            Self::CloseRecent => "关闭最近图片列表".into(),
            Self::UploadIntent => "确认目标并开始上传（尚不发布）".into(),
            Self::UploadCommit => "确认发布附件到预览目标".into(),
            Self::UploadStatus => "查询原上传状态".into(),
            Self::UploadCancel => "请求取消未发布上传（不能撤回已发消息）".into(),
            Self::UploadResume => "继续原上传".into(),
            Self::UploadClear => "清除本地预览".into(),
            Self::UploadForgetUnknown => "仅清本地未知记录（不取消服务器操作，不自动重试）".into(),
            Self::ReferenceConfirm => "确认发送该图片引用到预览目标".into(),
            Self::ReferenceStatus => "查询原图片引用状态".into(),
            Self::ReferenceClear => "关闭图片引用预览".into(),
        }
    }
}
#[derive(Clone, PartialEq, Eq)]
struct PickerTicket {
    binding: Binding,
    revision: u64,
    serial: u64,
}
#[derive(Default)]
pub(super) struct State {
    catalog: Catalog,
    context: Option<Binding>,
    picker: Option<PickerTicket>,
    serial: u64,
    pub(super) reference_supported: bool,
    pending_since: Option<(String, Instant)>,
    key_binding: Option<Binding>,
    key_tag: String,
    key_actions: Vec<KeyAction>,
    key_cursor: Option<usize>,
}
impl State {
    pub(super) fn freeze(&mut self, authority_changed: bool) {
        self.catalog.freeze(authority_changed);
        self.serial = self.serial.wrapping_add(1);
        self.picker = None;
        self.pending_since = None;
        self.clear_keys();
    }
    fn clear_keys(&mut self) {
        self.key_binding = None;
        self.key_tag.clear();
        self.key_actions.clear();
        self.key_cursor = None;
    }
}
fn claims(response: &Response) -> bool {
    response.request_id.starts_with("emoji-attachment:")
}
impl MioChatApp {
    fn custom_picker_current(&self, b: &Binding) -> bool {
        self.custom_emoji.picker.as_ref().is_some_and(|p| {
            p.binding == *b
                && p.revision == self.custom_emoji.catalog.revision
                && p.serial == self.custom_emoji.serial
        })
    }
    fn close_custom_picker(&mut self, ticket: &PickerTicket, cx: &mut Context<Self>) {
        if self.custom_emoji.picker.as_ref() != Some(ticket)
            || self.attachment_binding().as_ref() != Some(&ticket.binding)
        {
            return;
        }
        self.custom_emoji.serial = self.custom_emoji.serial.wrapping_add(1);
        self.custom_emoji.picker = None;
        self.custom_emoji.clear_keys();
        cx.notify();
    }
    pub(super) fn custom_emoji_context_occupied(&self) -> bool {
        self.custom_emoji.picker.is_some() || self.custom_emoji.catalog.reference.is_some()
    }
    fn custom_emoji_action_context(&self) -> bool {
        // Parent's "without attachment" gate intentionally excludes these two
        // attachment overlays, avoiding a recursive modal eligibility check.
        self.ordinary_chat_context_without_attachment()
            && !self.attachment_context_occupied()
            && !self.persistence.quarantined()
            && (!self.persistence_enabled || self.persistence.ready())
    }
    pub(super) fn remember_custom_emoji_upload(&mut self, receipt: &Receipt) {
        let Some(upload) = self.attachment_ui.transfers.upload.as_ref() else {
            return;
        };
        if upload.phase != UploadPhase::Committed || upload.message_id != receipt.message_id {
            return;
        }
        self.custom_emoji
            .catalog
            .remember_committed(upload.purpose, &upload.binding, receipt);
    }
    pub(super) fn poll_custom_emoji(&mut self, cx: &mut Context<Self>) {
        let now = self.attachment_binding();
        if self.custom_emoji.context != now {
            let force = self
                .custom_emoji
                .context
                .as_ref()
                .zip(now.as_ref())
                .is_some_and(|(a, b)| a.owner != b.owner || a.origin != b.origin);
            self.custom_emoji.freeze(force);
            self.custom_emoji.context = now;
        }
        if let Some(binding) = self.attachment_binding() {
            if self.custom_emoji.picker.is_some() && !self.custom_picker_current(&binding) {
                self.custom_emoji.picker = None;
                self.custom_emoji.serial = self.custom_emoji.serial.wrapping_add(1);
            }
            let (tag, actions) = self.attachment_key_actions(&binding);
            if self.custom_emoji.key_binding.as_ref() != Some(&binding)
                || self.custom_emoji.key_tag != tag
                || self.custom_emoji.key_actions != actions
            {
                self.custom_emoji.clear_keys();
            }
        }
        if self
            .custom_emoji
            .pending_since
            .as_ref()
            .is_some_and(|(_, at)| at.elapsed() > Duration::from_secs(20))
            && let Some((id, _)) = self.custom_emoji.pending_since.take()
        {
            self.custom_emoji.catalog.transport_uncertain(&id);
            self.custom_emoji.clear_keys();
            cx.notify();
        }
    }
    pub(super) fn open_custom_emoji(&mut self, cx: &mut Context<Self>) {
        if !self.ordinary_chat_context_allowed()
            || !self.custom_emoji_action_context()
            || !self.attachment_ui.transfers.supported
            || !self.custom_emoji.reference_supported
        {
            return;
        }
        let Some(binding) = self.attachment_binding() else {
            return;
        };
        self.custom_emoji.context = Some(binding.clone());
        self.custom_emoji.serial = self.custom_emoji.serial.wrapping_add(1);
        self.custom_emoji.picker = Some(PickerTicket {
            binding,
            revision: self.custom_emoji.catalog.revision,
            serial: self.custom_emoji.serial,
        });
        self.custom_emoji.clear_keys();
        cx.notify();
    }
    fn choose_custom_emoji(
        &mut self,
        source_id: i64,
        ticket: &PickerTicket,
        cx: &mut Context<Self>,
    ) {
        if !self.custom_emoji_action_context() {
            return;
        }
        let Some(binding) = self.attachment_binding() else {
            return;
        };
        if !self.custom_emoji.reference_supported
            || self.custom_emoji.picker.as_ref() != Some(ticket)
            || ticket.binding != binding
            || !self.custom_picker_current(&binding)
        {
            return;
        }
        let key = format!(
            "eref_{:x}_{:x}_{:x}",
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|v| v.as_nanos())
                .unwrap_or(0),
            self.generation,
            self.request_counter
        );
        self.request_counter = self.request_counter.wrapping_add(1);
        match self
            .custom_emoji
            .catalog
            .select(binding, source_id, ticket.revision, key)
        {
            Ok(()) => {
                self.custom_emoji.serial = self.custom_emoji.serial.wrapping_add(1);
                self.custom_emoji.picker = None;
                self.custom_emoji.clear_keys();
            }
            Err(e) => self.error = Some(e.into()),
        }
        cx.notify();
    }
    fn custom_emoji_action_if_current(&mut self, ticket: &ReferenceTicket, cx: &mut Context<Self>) {
        let Some(binding) = self.attachment_binding() else {
            return;
        };
        if !self.custom_emoji_action_context()
            || !self.custom_emoji.catalog.admits_action(ticket, &binding)
        {
            return;
        }
        match ticket.action() {
            ReferenceAction::Operation(op) => self.custom_emoji_operation(op, cx),
            ReferenceAction::Clear => {
                self.custom_emoji.catalog.dismiss_preview_or_terminal();
                self.custom_emoji.clear_keys();
                cx.notify();
            }
        }
    }
    fn custom_emoji_ticket_button(
        &self,
        id: &'static str,
        label: &'static str,
        action: ReferenceAction,
        cx: &mut Context<Self>,
    ) -> impl IntoElement {
        let ticket = self
            .attachment_binding()
            .and_then(|b| self.custom_emoji.catalog.action_ticket(action, &b));
        Self::button(id, label).on_click(cx.listener(move |app, _, _, cx| {
            if let Some(ticket) = ticket.as_ref() {
                app.custom_emoji_action_if_current(ticket, cx);
            }
        }))
    }
    fn custom_emoji_operation(&mut self, op: Operation, cx: &mut Context<Self>) {
        if !self.custom_emoji_action_context()
            || !self.attachment_ui.transfers.supported
            || !self.custom_emoji.reference_supported
        {
            return;
        }
        let Some(binding) = self.attachment_binding() else {
            return;
        };
        let id = self.next_request_id("emoji-attachment");
        match self.custom_emoji.catalog.begin(op, id.clone(), &binding) {
            Ok(data) => {
                let mut request = Request::simple("attachment", &id);
                request.room = &binding.room;
                request.attachment = Some(&data);
                self.custom_emoji.pending_since = Some((id.clone(), Instant::now()));
                if self.send_request(&request).is_err() {
                    self.custom_emoji.catalog.transport_uncertain(&id);
                    self.custom_emoji.pending_since = None;
                }
            }
            Err(e) => self.error = Some(e.into()),
        }
        self.custom_emoji.clear_keys();
        cx.notify();
    }
    /// Must be delegated to BEFORE the broad attachment response handler. Old
    /// reference errors remain claimed after state/credentials have disappeared.
    pub(super) fn custom_emoji_response(
        &mut self,
        response: &Response,
        cx: &mut Context<Self>,
    ) -> bool {
        if !claims(response) {
            return false;
        }
        if !super::attachments_ui::attachment_response_exclusive(response) {
            return true;
        }
        let Some(binding) = self.attachment_binding() else {
            return true;
        };
        if response.room != binding.room {
            return true;
        }
        if let Some(receipt) = response.attachment.as_ref()
            && self
                .custom_emoji
                .catalog
                .accept(&response.request_id, &binding, receipt)
        {
            self.custom_emoji.pending_since = None;
            self.custom_emoji.clear_keys();
            cx.notify();
        }
        true
    }
    pub(super) fn custom_emoji_controls(&self, cx: &mut Context<Self>) -> impl IntoElement {
        Self::button(
            "custom-image-recents",
            if self.custom_emoji.reference_supported {
                "最近自定义图片表情 · Ctrl+Shift+I"
            } else {
                "最近图片引用（服务器未支持）"
            },
        )
        .on_click(cx.listener(|app, _, _, cx| app.open_custom_emoji(cx)))
    }
    pub(super) fn render_custom_emoji_picker(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let binding = self.attachment_binding();
        let picker = self.custom_emoji.picker.as_ref().filter(|p| {
            binding
                .as_ref()
                .is_some_and(|b| self.custom_picker_current(b) && *b == p.binding)
        });
        let reference = self.custom_emoji.catalog.reference.as_ref().filter(|r| {
            binding
                .as_ref()
                .is_some_and(|b| r.binding.same_authority(b))
        });
        let hint = self
            .custom_emoji
            .key_cursor
            .and_then(|i| self.custom_emoji.key_actions.get(i))
            .map(|a| {
                format!(
                    "键盘当前动作：{}；Enter执行，Tab/Shift+Tab切换，Esc只关闭未发送预览。",
                    a.label()
                )
            });
        div().flex().flex_col().gap(px(8.)).when_some(hint,|d,h|d.child(h))
            .when_some(self.custom_emoji.catalog.notice.clone(),|d,n|d.child(n))
            .when_some(picker,|d,p|{
                let entries=self.custom_emoji.catalog.items(&p.binding);let close=p.clone();
                d.child(format!("最近自定义图片 · 账户 {} · 当前目标 {}",p.binding.owner,p.binding.room))
                    .child("仅保留最多24项图片元信息；选择后再次确认。服务端重新验证权限，引用不增加接收字节额度。")
                    .when(entries.is_empty(),|d|d.child("尚无可引用的自定义图片，请先上传并发布自定义图片表情。"))
                    .children(entries.iter().map(|entry|{let id=entry.source_message_id;let ticket=p.clone();
                        custom_button(format!("custom-image-source-{id}"),format!("{} · {} 字节 · 源消息 #{}",entry.info.filename,entry.info.size,id))
                            .on_click(cx.listener(move|app,_,_,cx|app.choose_custom_emoji(id,&ticket,cx)))
                    }))
                    .child(Self::button("custom-image-close","关闭最近列表").on_click(cx.listener(move|app,_,_,cx|app.close_custom_picker(&close,cx))))
            })
            .when_some(reference,|d,r|{
                d.child(format!("待引用图片：{} · {} 字节 · 源消息 #{}",r.entry.info.filename,r.entry.info.size,r.entry.source_message_id))
                    .child(format!("确认目标：{} · 账户：{}",r.binding.room,r.binding.owner))
                    .child(format!("图片ID：{} · SHA-256：{}",r.entry.info.id,r.entry.info.sha256))
                    .child("不重新上传图片、不重复扣上传额度。原图元数据仍可能包含EXIF；源消息撤回/权限失效后引用可能不可用。")
                    .child(r.notice.clone())
                    .when(!r.pending()&&r.phase==Phase::Preview&&binding.as_ref().is_some_and(|b|r.current(b)),|d|d.child(self.custom_emoji_ticket_button("custom-image-confirm-reference","确认发送图片引用到上述目标",ReferenceAction::Operation(Operation::Reference),cx)))
                    .when(!r.pending()&&binding.as_ref().is_some_and(|b|r.recoverable(b)),|d|d.child(self.custom_emoji_ticket_button("custom-image-reference-status","查询原引用状态（不会重发）",ReferenceAction::Operation(Operation::Status),cx)))
                    .when(!r.pending()&&(r.phase==Phase::Preview||r.terminal()),|d|d.child(self.custom_emoji_ticket_button("custom-image-reference-dismiss","关闭引用预览",ReferenceAction::Clear,cx)))
                    .when(r.pending(),|d|d.child("等待原引用回执；关闭界面不等于撤回。"))
            })
    }
    fn attachment_key_actions(&self, b: &Binding) -> (String, Vec<KeyAction>) {
        if self.custom_picker_current(b) {
            let mut actions = self
                .custom_emoji
                .catalog
                .items(b)
                .iter()
                .map(|e| KeyAction::ChooseRecent(e.source_message_id))
                .collect::<Vec<_>>();
            actions.push(KeyAction::CloseRecent);
            return (
                format!("recent:{}", self.custom_emoji.catalog.revision),
                actions,
            );
        }
        if let Some(r) = self
            .custom_emoji
            .catalog
            .reference
            .as_ref()
            .filter(|r| r.binding.same_authority(b))
        {
            let mut actions = Vec::new();
            if !r.pending() {
                if r.phase == Phase::Preview && r.current(b) {
                    actions.push(KeyAction::ReferenceConfirm);
                }
                if r.recoverable(b) {
                    actions.push(KeyAction::ReferenceStatus);
                }
                if r.phase == Phase::Preview || r.terminal() {
                    actions.push(KeyAction::ReferenceClear);
                }
            }
            return (
                format!("reference:{}:{:?}:{}", r.intent_key, r.phase, r.pending()),
                actions,
            );
        }
        if let Some(u) = self
            .attachment_ui
            .transfers
            .upload
            .as_ref()
            .filter(|u| u.binding.same_authority(b))
        {
            let mut actions = Vec::new();
            if !u.pending() {
                if u.phase == UploadPhase::Preview && u.binding == *b {
                    actions.push(KeyAction::UploadIntent);
                }
                if u.phase == UploadPhase::ReadyToCommit && u.binding == *b {
                    actions.push(KeyAction::UploadCommit);
                }
                if matches!(u.phase, UploadPhase::Unknown | UploadPhase::Paused) {
                    actions.push(KeyAction::UploadStatus);
                }
                if u.can_resume(b) {
                    actions.push(KeyAction::UploadResume);
                }
                if !u.upload_id.is_empty() && !u.terminal() && u.phase != UploadPhase::Preview {
                    actions.push(KeyAction::UploadCancel);
                }
                if u.phase == UploadPhase::Unknown {
                    actions.push(KeyAction::UploadForgetUnknown);
                }
                if u.phase == UploadPhase::Preview || u.terminal() {
                    actions.push(KeyAction::UploadClear);
                }
            }
            return (
                format!("upload:{}:{:?}:{}", u.intent_key, u.phase, u.pending()),
                actions,
            );
        }
        (String::new(), Vec::new())
    }
    /// Install before dot/Unicode-emoji key handlers. Ctrl+V is never consumed.
    /// No default armed action: Tab selects, Enter executes only that selection.
    pub(super) fn attachment_key(
        &mut self,
        event: &KeyDownEvent,
        _window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        let m = &event.keystroke.modifiers;
        let key = event.keystroke.key.as_str();
        let shortcut = attachment_shortcut(event);
        if shortcut {
            if !attachment_key_press_allowed(event, self.marked_range.is_some()) {
                return true;
            }
            if !self.ordinary_chat_context_allowed() {
                return true;
            }
            match key {
                "u" => self.choose_attachment(Purpose::Attachment, cx),
                "v" => self.paste_attachment_image(Purpose::Attachment, cx),
                "i" => self.open_custom_emoji(cx),
                _ => {}
            }
            self.custom_emoji.clear_keys();
            return true;
        }
        let Some(binding) = self.attachment_binding() else {
            return false;
        };
        let (tag, actions) = self.attachment_key_actions(&binding);
        if tag.is_empty() || !matches!(key, "tab" | "enter" | "escape") {
            return false;
        }
        if m.control || m.alt || m.platform || m.function {
            return false;
        }
        if !attachment_key_press_allowed(event, self.marked_range.is_some()) {
            return true;
        }
        // The parent base gate excludes attachment overlays; all other modals,
        // secrets, IME, pending navigation and account transitions still block.
        if !self.ordinary_chat_context_without_attachment() {
            return true;
        }
        if self.custom_emoji.key_binding.as_ref() != Some(&binding)
            || self.custom_emoji.key_tag != tag
            || self.custom_emoji.key_actions != actions
        {
            self.custom_emoji.key_binding = Some(binding.clone());
            self.custom_emoji.key_tag = tag;
            self.custom_emoji.key_actions = actions;
            self.custom_emoji.key_cursor = None;
        }
        if key == "escape" {
            if let Some(ticket) = self.custom_emoji.picker.clone() {
                self.close_custom_picker(&ticket, cx);
            } else if self
                .custom_emoji
                .catalog
                .reference
                .as_ref()
                .is_some_and(|r| r.phase == Phase::Preview || r.terminal())
            {
                if let Some(ticket) = self
                    .custom_emoji
                    .catalog
                    .action_ticket(ReferenceAction::Clear, &binding)
                {
                    self.custom_emoji_action_if_current(&ticket, cx);
                }
            } else if self
                .attachment_ui
                .transfers
                .upload
                .as_ref()
                .is_some_and(|u| u.phase == UploadPhase::Preview || u.terminal())
            {
                if let Some(ticket) = self
                    .attachment_ui
                    .transfers
                    .action_ticket(LocalAction::Discard, &binding)
                {
                    self.attachment_action_if_current(&ticket, cx);
                }
            } else {
                self.status =
                    "上传/引用可能已到达服务器；Esc不会假装撤回。请等待回执或Tab选择原状态查询。"
                        .into();
            }
            self.custom_emoji.clear_keys();
            cx.notify();
            return true;
        }
        let n = self.custom_emoji.key_actions.len();
        if key == "tab" {
            self.custom_emoji.key_cursor =
                cycle_key_cursor(self.custom_emoji.key_cursor, n, m.shift);
            cx.notify();
            return true;
        }
        if key == "enter" && !m.shift {
            if let Some(action) = self
                .custom_emoji
                .key_cursor
                .and_then(|i| self.custom_emoji.key_actions.get(i))
                .cloned()
            {
                self.custom_emoji.clear_keys();
                self.execute_attachment_key_action(action, &binding, cx);
            } else {
                self.status =
                    "请先按Tab选择明确的附件动作，再按Enter确认；此键没有发送内容。".into();
                cx.notify();
            }
        }
        true
    }
    fn execute_attachment_key_action(
        &mut self,
        action: KeyAction,
        binding: &Binding,
        cx: &mut Context<Self>,
    ) {
        let upload_action = match action {
            KeyAction::UploadIntent => Some(LocalAction::Operation(Operation::Intent)),
            KeyAction::UploadCommit => Some(LocalAction::Operation(Operation::Commit)),
            KeyAction::UploadStatus => Some(LocalAction::Operation(Operation::Status)),
            KeyAction::UploadCancel => Some(LocalAction::Operation(Operation::Cancel)),
            KeyAction::UploadResume => Some(LocalAction::Resume),
            KeyAction::UploadClear => Some(LocalAction::Discard),
            KeyAction::UploadForgetUnknown => Some(LocalAction::ForgetUnknown),
            _ => None,
        };
        if let Some(action) = upload_action {
            if let Some(ticket) = self.attachment_ui.transfers.action_ticket(action, binding) {
                self.attachment_action_if_current(&ticket, cx);
            }
            return;
        }
        let reference_action = match action {
            KeyAction::ReferenceConfirm => Some(ReferenceAction::Operation(Operation::Reference)),
            KeyAction::ReferenceStatus => Some(ReferenceAction::Operation(Operation::Status)),
            KeyAction::ReferenceClear => Some(ReferenceAction::Clear),
            _ => None,
        };
        if let Some(action) = reference_action {
            if let Some(ticket) = self.custom_emoji.catalog.action_ticket(action, binding) {
                self.custom_emoji_action_if_current(&ticket, cx);
            }
            return;
        }
        match action {
            KeyAction::ChooseRecent(id) => {
                if let Some(ticket) = self.custom_emoji.picker.clone() {
                    self.choose_custom_emoji(id, &ticket, cx);
                }
            }
            KeyAction::CloseRecent => {
                if let Some(ticket) = self.custom_emoji.picker.clone() {
                    self.close_custom_picker(&ticket, cx);
                }
            }
            _ => {}
        }
    }
}
fn attachment_key_press_allowed(event: &KeyDownEvent, marked: bool) -> bool {
    !event.is_held && !event.prefer_character_input && !marked
}
fn attachment_shortcut(event: &KeyDownEvent) -> bool {
    let m = &event.keystroke.modifiers;
    m.control
        && m.shift
        && !m.alt
        && !m.platform
        && !m.function
        && matches!(event.keystroke.key.as_str(), "u" | "v" | "i")
}
fn cycle_key_cursor(current: Option<usize>, len: usize, back: bool) -> Option<usize> {
    if len == 0 {
        return None;
    }
    Some(match current.filter(|i| *i < len) {
        None => {
            if back {
                len - 1
            } else {
                0
            }
        }
        Some(i) => {
            if back {
                (i + len - 1) % len
            } else {
                (i + 1) % len
            }
        }
    })
}
fn custom_button(id: String, label: String) -> Stateful<Div> {
    div()
        .id(id)
        .role(gpui::Role::Button)
        .aria_label(label.clone())
        .p(px(8.))
        .rounded(px(6.))
        .bg(rgb(0x292d32))
        .text_color(rgb(0xfdfcf9))
        .cursor_pointer()
        .child(label)
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn authentication_privacy_freeze_keeps_negotiated_capability_until_new_connection() {
        let mut state = State {
            reference_supported: true,
            ..Default::default()
        };
        state.freeze(true);
        assert!(state.reference_supported);
        state.freeze(false);
        assert!(state.reference_supported);
        // open_session invokes attachment_set_capabilities(&[]) before a new hello.
        state.reference_supported = false;
        state.freeze(true);
        assert!(!state.reference_supported);
    }
    #[test]
    fn tab_requires_an_explicit_selection_and_wraps() {
        assert_eq!(cycle_key_cursor(None, 3, false), Some(0));
        assert_eq!(cycle_key_cursor(None, 3, true), Some(2));
        assert_eq!(cycle_key_cursor(Some(2), 3, false), Some(0));
        assert_eq!(cycle_key_cursor(Some(0), 3, true), Some(2));
        assert_eq!(cycle_key_cursor(None, 0, false), None);
    }
    #[test]
    fn attachment_shortcuts_do_not_steal_plain_paste_and_held_ime_cannot_confirm() {
        let mut event = KeyDownEvent {
            keystroke: gpui::Keystroke::parse("ctrl-v").unwrap(),
            is_held: false,
            prefer_character_input: false,
        };
        assert!(!attachment_shortcut(&event));
        event.keystroke = gpui::Keystroke::parse("ctrl-shift-v").unwrap();
        assert!(attachment_shortcut(&event));
        assert!(attachment_key_press_allowed(&event, false));
        event.is_held = true;
        assert!(!attachment_key_press_allowed(&event, false));
        event.is_held = false;
        event.prefer_character_input = true;
        assert!(!attachment_key_press_allowed(&event, false));
        event.prefer_character_input = false;
        assert!(!attachment_key_press_allowed(&event, true));
    }
    #[test]
    fn reference_prefix_errors_stay_claimed() {
        let r = Response {
            kind: "error".into(),
            request_id: "emoji-attachment:1:8".into(),
            replace: true,
            ..Default::default()
        };
        assert!(claims(&r));
        let r = Response {
            kind: "error".into(),
            request_id: "chat:1:8".into(),
            ..Default::default()
        };
        assert!(!claims(&r));
    }
}
