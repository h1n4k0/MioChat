use super::*;
use miochat_gpui::maintenance::{Job, Kind, Operation, State};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Action {
    Start,
    Cancel,
    View,
    Copy,
    Close,
}
impl Action {
    fn id(self) -> &'static str {
        match self {
            Self::Start => "maintenance-start",
            Self::Cancel => "maintenance-cancel",
            Self::View => "maintenance-view",
            Self::Copy => "maintenance-copy",
            Self::Close => "maintenance-close",
        }
    }
    fn label(self) -> &'static str {
        match self {
            Self::Start => "确认在服务器创建备份",
            Self::Cancel => "请求取消此作业",
            Self::View => "重新授权读取结果",
            Self::Copy => "复制当前结果",
            Self::Close => "关闭此窗",
        }
    }
}
fn actions(job: &Job) -> Vec<Action> {
    let mut out = Vec::new();
    if job.pending.is_none() && !job.expired {
        if job.confirming {
            out.push(Action::Start);
        } else if !job.start_id.is_empty()
            && !job.rejected
            && !job.state.is_some_and(State::terminal)
        {
            out.push(Action::Cancel);
        } else if !job.lines.is_empty() {
            out.push(Action::Copy);
        } else if job.state.is_some_and(State::terminal) && job.state != Some(State::Unavailable) {
            out.push(Action::View);
        }
    }
    out.push(Action::Close);
    out
}
fn visible(
    screen: Screen,
    job: Option<&Job>,
    owner: &str,
    generation: u64,
    origin: Option<&miochat_gpui::composition::Origin>,
    epoch: u64,
) -> bool {
    screen == Screen::Chat
        && job.is_some_and(|j| j.bound(owner, generation, origin) && j.visible(epoch))
}
fn inputs_clear(occupied: [bool; 7]) -> bool {
    !occupied.into_iter().any(|v| v)
}
pub(super) fn discard_for_credentials(job: &mut Option<Job>, notice: &mut Option<String>) {
    if let Some(j) = job.as_ref()
        && j.kind == Kind::Backup
        && !j.start_id.is_empty()
        && !j.rejected
    {
        if j.committed == Some(true) {
            *notice = Some(
                "此前备份已经提交到服务器。连接/凭据已变化，不会自动重跑；请先在原服务器核验。"
                    .into(),
            );
        } else if !matches!(j.state, Some(State::Cancelled | State::Failed)) {
            *notice = Some("此前备份可能已提交到服务器。断线/改密或取消不等于回滚，不会自动重跑；请先在原服务器核验，勿直接重复备份。".into());
        }
    }
    // Only the static notice survives. No job identity, endpoint, account,
    // path, diagnostic, pending request or private result can cross sessions.
    *job = None;
}
fn control_allowed(event: &KeyDownEvent, marked: bool) -> bool {
    !event.is_held && !event.prefer_character_input && !marked
}
fn consume_typed(current: &str, captured: Option<&str>, room: &str, origin: &str) -> bool {
    room == origin && captured.is_some_and(|s| s == current)
}
fn response_claimed(response: &Response, job: Option<&Job>) -> bool {
    response.maintenance.is_some()
        || response.kind == "maintenance"
        || job.is_some_and(|j| j.matches_request(&response.request_id))
}
impl MioChatApp {
    pub(super) fn maintenance_visible(&self) -> bool {
        visible(
            self.screen,
            self.maintenance.as_ref(),
            &self.user,
            self.generation,
            self.session_origin.as_ref(),
            self.moderation_epoch,
        )
    }
    fn maintenance_bound(&self) -> bool {
        self.screen == Screen::Chat
            && self.session.is_some()
            && self
                .maintenance
                .as_ref()
                .is_some_and(|j| j.bound(&self.user, self.generation, self.session_origin.as_ref()))
    }
    fn maintenance_open_allowed(&self) -> bool {
        self.screen == Screen::Chat
            && self.session.is_some()
            && self.session_origin.is_some()
            && inputs_clear([
                self.composition_visible(),
                self.direct_form.is_some(),
                self.word_start.is_some(),
                self.room_form.is_some(),
                self.pending_send.is_some(),
                self.marked_range.is_some(),
                self.profile_editor.is_some(),
            ])
    }
    pub(super) fn open_maintenance_view(&mut self, cx: &mut Context<Self>) {
        if !self.maintenance_open_allowed() || !self.maintenance_bound() {
            return;
        }
        self.begin_feature_navigation();
        self.clear_profile();
        self.clear_center();
        self.preferences_open = false;
        self.appearance_menu = false;
        self.mock_menu = false;
        self.maintenance
            .as_mut()
            .unwrap()
            .open(self.moderation_epoch);
        self.maintenance_cursor = 0;
        self.maybe_read_maintenance(cx);
        cx.notify();
    }
    pub(super) fn activate_maintenance(
        &mut self,
        command: &str,
        preserve_draft: bool,
        cx: &mut Context<Self>,
    ) -> bool {
        let (head, arg) = miochat_gpui::configuration::split_argument(command);
        if !matches!(head, "/backup" | "/check" | "/canceljob") {
            return false;
        }
        if !arg.is_empty() {
            self.error = Some("该命令不接受参数或文件路径".into());
            cx.notify();
            return true;
        }
        if !self.maintenance_open_allowed() {
            self.error = Some("请先完成当前输入/确认操作，再处理后台作业".into());
            cx.notify();
            return true;
        }
        if head == "/canceljob" {
            if self.maintenance_bound()
                && self
                    .maintenance
                    .as_ref()
                    .is_some_and(|j| j.active() && !j.confirming)
            {
                if self
                    .maintenance
                    .as_ref()
                    .is_some_and(|j| j.pending.is_some())
                {
                    self.status = "正在等待当前作业请求确认；未重复发送取消".into();
                    cx.notify();
                    return true;
                }
                self.open_maintenance_view(cx);
                self.queue_maintenance(Operation::Cancel, cx);
                if let Some(j) = self.maintenance.as_mut()
                    && j.pending
                        .as_ref()
                        .is_some_and(|p| p.operation == Operation::Cancel)
                {
                    j.cancel_input = (!preserve_draft && self.composer == command)
                        .then(|| self.composer.clone());
                }
            } else {
                self.status = "当前连接没有可取消的后台作业".into();
                if !preserve_draft && self.composer == command {
                    self.composer.clear();
                    self.reset_selection();
                }
            }
            cx.notify();
            return true;
        }
        if self.maintenance.as_ref().is_some_and(Job::active) {
            self.open_maintenance_view(cx);
            self.error = Some("当前作业仍未结束或结果未知；不会自动另建备份".into());
            cx.notify();
            return true;
        }
        self.begin_feature_navigation();
        self.clear_profile();
        self.clear_center();
        self.preferences_open = false;
        self.appearance_menu = false;
        self.mock_menu = false;
        let input = (!preserve_draft && self.composer == command).then(|| self.composer.clone());
        let kind = if head == "/backup" {
            Kind::Backup
        } else {
            Kind::Check
        };
        self.maintenance = Some(Job::new(
            self.user.clone(),
            self.generation,
            self.session_origin.clone().unwrap(),
            self.room.clone(),
            kind,
            self.moderation_epoch,
            input,
        ));
        self.maintenance_cursor = 0;
        if kind == Kind::Check {
            self.queue_maintenance(Operation::Start, cx);
        }
        cx.notify();
        true
    }
    fn queue_maintenance(&mut self, operation: Operation, cx: &mut Context<Self>) {
        if !self.maintenance_bound() || self.marked_range.is_some() {
            return;
        }
        let id = self.next_request_id("maintenance");
        let now = self.persistence_now();
        let Some(data) = self
            .maintenance
            .as_mut()
            .and_then(|j| j.begin(operation, id.clone(), self.moderation_epoch, now))
        else {
            return;
        };
        let mut request = Request::simple("maintenance", &id);
        request.room = &self.room;
        request.maintenance = Some(&data);
        if self.send_request(&request).is_err() {
            let j = self.maintenance.as_mut().unwrap();
            j.pending = None;
            j.uncertain = operation != Operation::View;
            j.status = "请求结果未知；不会自动重新执行，请明确处理原作业".into();
        }
        cx.notify();
    }
    fn maybe_read_maintenance(&mut self, cx: &mut Context<Self>) {
        let should_read = self.maintenance_visible()
            && self.maintenance.as_ref().is_some_and(|j| {
                !j.view_requested
                    && j.pending.is_none()
                    && j.state.is_some_and(State::terminal)
                    && j.state != Some(State::Unavailable)
                    && !j.expired
            });
        if should_read {
            self.queue_maintenance(Operation::View, cx);
        }
    }
    pub(super) fn handle_maintenance_response(
        &mut self,
        response: &Response,
        cx: &mut Context<Self>,
    ) -> bool {
        let relevant = response_claimed(response, self.maintenance.as_ref());
        if !relevant {
            return false;
        }
        // A forged/mixed/old maintenance frame cannot reach unrelated global
        // profile, center, preferences or other workflow effects.
        if !self.maintenance_bound() {
            return true;
        }
        let now = self.persistence_now();
        let j = self.maintenance.as_mut().unwrap();
        j.sync(now, self.moderation_epoch);
        let Some(consume) = j.accept(response, self.moderation_epoch, now) else {
            return true;
        };
        if consume {
            let opening = if response
                .maintenance
                .as_ref()
                .is_some_and(|r| r.operation == Operation::Cancel)
            {
                j.cancel_input.take()
            } else {
                j.opening_input.take()
            };
            if consume_typed(&self.composer, opening.as_deref(), &self.room, &j.room) {
                self.composer.clear();
                self.reset_selection();
            }
        }
        self.maybe_read_maintenance(cx);
        cx.notify();
        true
    }
    pub(super) fn poll_maintenance(&mut self, cx: &mut Context<Self>) {
        let now = self.persistence_now();
        if let Some(j) = self.maintenance.as_mut() {
            j.sync(now, self.moderation_epoch);
        }
        self.maybe_read_maintenance(cx);
    }
    fn maintenance_action(&mut self, action: Action, cx: &mut Context<Self>) {
        if !self.maintenance_visible()
            || self.marked_range.is_some()
            || !self
                .maintenance
                .as_ref()
                .is_some_and(|j| actions(j).contains(&action))
        {
            return;
        }
        match action {
            Action::Start => self.queue_maintenance(Operation::Start, cx),
            Action::Cancel => self.queue_maintenance(Operation::Cancel, cx),
            Action::View => self.queue_maintenance(Operation::View, cx),
            Action::Copy => {
                let j = self.maintenance.as_ref().unwrap();
                let mut text = j.lines.join("\n");
                if !j.warning.is_empty() {
                    text.push('\n');
                    text.push_str(&j.warning);
                }
                cx.write_to_clipboard(gpui::ClipboardItem::new_string(text));
            }
            Action::Close => {
                let unstarted = self
                    .maintenance
                    .as_ref()
                    .is_some_and(|j| j.start_id.is_empty());
                self.begin_feature_navigation();
                if unstarted {
                    self.maintenance = None;
                }
            }
        }
        cx.notify();
    }
    pub(super) fn maintenance_key(&mut self, event: &KeyDownEvent, cx: &mut Context<Self>) -> bool {
        if !self.maintenance_visible() {
            return false;
        }
        cx.stop_propagation();
        if !control_allowed(event, self.marked_range.is_some()) {
            return true;
        }
        let choices = actions(self.maintenance.as_ref().unwrap());
        match event.keystroke.key.as_str() {
            "escape" => self.maintenance_action(Action::Close, cx),
            "tab" => {
                self.maintenance_cursor = (self.maintenance_cursor + 1) % choices.len();
                cx.notify();
            }
            "enter" => {
                self.maintenance_action(choices[self.maintenance_cursor.min(choices.len() - 1)], cx)
            }
            "c" if event.keystroke.modifiers.control || event.keystroke.modifiers.platform => {
                self.maintenance_action(Action::Copy, cx)
            }
            _ => {}
        }
        true
    }
    pub(super) fn maintenance_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let j = self.maintenance.as_ref().unwrap();
        let mut panel = div()
            .id("maintenance-popup")
            .absolute()
            .top(px(64.))
            .right(px(20.))
            .w(px(380.))
            .max_h(px(560.))
            .overflow_y_scroll()
            .p(px(18.))
            .rounded(px(12.))
            .bg(self.raised_color())
            .border_1()
            .border_color(self.preference_border())
            .shadow_lg()
            .flex()
            .flex_col()
            .gap(px(12.))
            .on_click(|_, _, cx| cx.stop_propagation())
            .child(if j.kind == Kind::Backup {
                "服务器一致性备份"
            } else {
                "数据库与配置检查"
            })
            .child(j.status.clone());
        if j.confirming {
            panel=panel.child("此操作在服务器端创建包含数据库、主机密钥和摘要清单的敏感备份，不会下载到本设备。确认后在后台运行；取消只停止未提交阶段，不撤销已完成备份。");
        }
        for line in &j.lines {
            panel = panel.child(div().text_size(px(12.)).child(line.clone()));
        }
        if !j.warning.is_empty() {
            panel = panel.child(j.warning.clone());
        }
        panel=panel.child(div().text_size(px(11.)).child("结果仅原连接管理员可重新授权查看，5分钟后过期。Tab切换按钮，Enter确认，Esc仅关窗；不会自动重试作业。"));
        let mut buttons = div().flex().flex_wrap().gap(px(8.));
        for (index, action) in actions(j).into_iter().enumerate() {
            buttons = buttons.child(
                Self::button(action.id(), action.label())
                    .when(index == self.maintenance_cursor, |d| {
                        d.border_1().border_color(self.preference_text())
                    })
                    .on_click(cx.listener(move |this, _, _, cx| {
                        cx.stop_propagation();
                        this.maintenance_action(action, cx);
                    })),
            )
        }
        panel.child(buttons)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use miochat_gpui::composition::Origin;
    fn job() -> Job {
        Job::new(
            "admin".into(),
            5,
            Origin::synthetic("fixture", 3),
            "lobby".into(),
            Kind::Backup,
            9,
            Some("/backup".into()),
        )
    }
    #[test]
    fn actual_maintenance_visibility_controls_and_keyboard_guards_are_bound() {
        let mut j = job();
        let origin = j.origin.clone();
        assert!(visible(
            Screen::Chat,
            Some(&j),
            "admin",
            5,
            Some(&origin),
            9
        ));
        for (screen, owner, generation, epoch) in [
            (Screen::Password, "admin", 5, 9),
            (Screen::Chat, "other", 5, 9),
            (Screen::Chat, "admin", 6, 9),
            (Screen::Chat, "admin", 5, 10),
        ] {
            assert!(!visible(
                screen,
                Some(&j),
                owner,
                generation,
                Some(&origin),
                epoch
            ));
        }
        assert!(!visible(
            Screen::Chat,
            Some(&j),
            "admin",
            5,
            Some(&Origin::synthetic("other", 3)),
            9
        ));
        assert!(inputs_clear([false; 7]));
        for lane in 0..7 {
            let mut occupied = [false; 7];
            occupied[lane] = true;
            assert!(!inputs_clear(occupied));
        }
        // Lane6 is the actual unsaved profile editor; opening the job badge
        // must never reach clear_profile while it is occupied.
        assert!(!inputs_clear([
            false, false, false, false, false, false, true
        ]));
        assert_eq!(actions(&j), vec![Action::Start, Action::Close]);
        let mut event = KeyDownEvent {
            keystroke: gpui::Keystroke {
                key: "enter".into(),
                ..Default::default()
            },
            is_held: false,
            prefer_character_input: false,
        };
        assert!(control_allowed(&event, false));
        event.is_held = true;
        assert!(!control_allowed(&event, false));
        event.is_held = false;
        event.prefer_character_input = true;
        assert!(!control_allowed(&event, false));
        event.prefer_character_input = false;
        assert!(!control_allowed(&event, true));
        j.begin(Operation::Start, "start".into(), 9, 1).unwrap();
        assert_eq!(actions(&j), vec![Action::Close]);
        j.hide();
        assert!(!visible(
            Screen::Chat,
            Some(&j),
            "admin",
            5,
            Some(&origin),
            9
        ));
        j.sync(30_001, 9);
        j.open(10);
        assert_eq!(actions(&j), vec![Action::Cancel, Action::Close]);
        assert!(j.start_id == "start" && j.uncertain);
        let mut r = Response {
            kind: "state".into(),
            request_id: "start".into(),
            preferences: Some(miochat_gpui::preferences::Preferences::default()),
            ..Response::default()
        };
        assert!(response_claimed(&r, Some(&j)));
        assert!(!j.admits(&r, 10));
        r.request_id = "unrelated-state".into();
        assert!(!response_claimed(&r, Some(&j)));
        r.kind = "maintenance".into();
        assert!(response_claimed(&r, None));
    }
    #[test]
    fn actual_maintenance_typed_ack_consumption_preserves_newer_and_menu_drafts() {
        assert!(consume_typed("/backup", Some("/backup"), "lobby", "lobby"));
        assert!(consume_typed(
            "/canceljob",
            Some("/canceljob"),
            "lobby",
            "lobby"
        ));
        assert!(!consume_typed(
            "new 中文 draft",
            Some("/canceljob"),
            "lobby",
            "lobby"
        ));
        assert!(!consume_typed("original draft", None, "lobby", "lobby"));
        assert!(!consume_typed(
            "/backup",
            Some("/backup"),
            "dm:peer",
            "lobby"
        ));
        let mut j = job();
        j.state = Some(State::Complete);
        j.committed = Some(true);
        j.confirming = false;
        j.lines = vec!["private path".into()];
        assert_eq!(actions(&j), vec![Action::Copy, Action::Close]);
        j.hide();
        assert!(j.lines.is_empty());
        j.open(10);
        assert_eq!(actions(&j), vec![Action::View, Action::Close]);
        j.expired = true;
        assert_eq!(actions(&j), vec![Action::Close]);
        for state in [
            None,
            Some(State::Running),
            Some(State::CancelRequested),
            Some(State::Unavailable),
            Some(State::Complete),
            Some(State::Failed),
            Some(State::Cancelled),
        ] {
            let mut current = job();
            current.start_id = "sent-start".into();
            current.state = state;
            current.committed = (state == Some(State::Complete)).then_some(true);
            current.lines = vec!["private-sensitive-path".into()];
            current.warning = "private diagnostic".into();
            let mut slot = Some(current);
            let mut notice = None;
            discard_for_credentials(&mut slot, &mut notice);
            assert!(slot.is_none());
            assert_eq!(
                notice.is_some(),
                !matches!(state, Some(State::Failed | State::Cancelled))
            );
            if let Some(n) = notice {
                assert!(n.contains("不会自动重跑") && n.contains("原服务器"));
                assert!(!n.contains("private") && !n.contains("sent-start"));
            }
        }
        let mut unopened = Some(job());
        let mut notice = None;
        discard_for_credentials(&mut unopened, &mut notice);
        assert!(unopened.is_none() && notice.is_none());
    }
}
