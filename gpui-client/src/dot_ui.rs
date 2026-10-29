use super::*;
use miochat_gpui::dot::{Data, Receipt};
#[derive(Default)]
pub(super) struct State {
    pub supported: bool,
    view: Option<View>,
    serial: u64,
    pub notice: Option<String>,
}
struct View {
    owner: String,
    room: String,
    origin: miochat_gpui::composition::Origin,
    generation: u64,
    tag: String,
    serial: u64,
    receipt: Receipt,
    pending: Option<(String, Data, Instant)>,
    unknown: Option<Data>,
    cursor: usize,
    received: Instant,
}
#[derive(Clone, PartialEq, Eq)]
struct Action {
    label: String,
    data: Data,
}
fn captured_pointer_action(
    revision: u64,
    captured_revision: u64,
    current: &[Action],
    captured: &Action,
) -> Option<Data> {
    (revision == captured_revision && current.contains(captured)).then(|| captured.data.clone())
}
impl State {
    pub(super) fn clear(&mut self) {
        if self.view.as_ref().is_some_and(|v| {
            v.pending
                .as_ref()
                .is_some_and(|(_, d, _)| d.operation == "confirm")
                || v.unknown.is_some()
        }) {
            self.notice = Some(
                "此前 dot 确认可能已经提交；不会自动重发。请在原服务器重新授权核验操作状态。"
                    .into(),
            );
        }
        self.view = None;
        self.serial = self.serial.wrapping_add(1);
    }
}
impl MioChatApp {
    pub(super) fn dot_visible(&self) -> bool {
        self.dot_ui.view.as_ref().is_some_and(|v| {
            self.screen == Screen::Chat
                && !self.must_reset
                && self
                    .session
                    .as_ref()
                    .is_some_and(|s| s.generation() == v.generation)
                && v.owner == self.user
                && v.room == self.room
                && v.generation == self.generation
                && Some(&v.origin) == self.session_origin.as_ref()
        })
    }
    pub(super) fn dot_open(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if !self.dot_ui.supported {
            self.error = Some("此服务器不支持 dot 私有审阅协议。".into());
            cx.notify();
            return;
        }
        if !self.ordinary_chat_context_allowed() || self.room.starts_with('@') {
            return;
        }
        let Some(origin) = self.session_origin.clone() else {
            return;
        };
        self.begin_feature_navigation();
        self.dot_ui.clear();
        self.dot_ui.serial = self.dot_ui.serial.wrapping_add(1);
        let serial = self.dot_ui.serial;
        let tag = self.next_request_id("dot-owner");
        self.dot_ui.view = Some(View {
            owner: self.user.clone(),
            room: self.room.clone(),
            origin,
            generation: self.generation,
            tag,
            serial,
            receipt: Receipt::default(),
            pending: None,
            unknown: None,
            cursor: 0,
            received: Instant::now(),
        });
        self.dot_send(
            Data {
                operation: "list".into(),
                ..Default::default()
            },
            serial,
            cx,
        );
        self.menu_focus.focus(window, cx);
    }
    fn dot_actions(&self) -> Vec<Action> {
        let Some(v) = self.dot_ui.view.as_ref().filter(|_| self.dot_visible()) else {
            return Vec::new();
        };
        if v.pending.is_some() {
            return Vec::new();
        }
        if let Some(d) = &v.unknown {
            return if d.operation_ref.is_empty() {
                Vec::new()
            } else {
                vec![Action {
                    label: "查询原操作状态（不重发）".into(),
                    data: Data {
                        operation: "status".into(),
                        operation_ref: d.operation_ref.clone(),
                        ..Default::default()
                    },
                }]
            };
        }
        let mut out = vec![Action {
            label: "重新授权刷新".into(),
            data: Data {
                operation: "list".into(),
                ..Default::default()
            },
        }];
        if v.receipt.operation == "list"
            && v.receipt.bindings.is_empty()
            && v.receipt.binding.is_none()
            && v.receipt.review.is_none()
        {
            out.push(Action {
                label: "创建当前房间绑定（默认关闭）".into(),
                data: Data {
                    operation: "create".into(),
                    ..Default::default()
                },
            });
        }
        for b in v.receipt.bindings.iter().chain(v.receipt.binding.iter()) {
            for (op, label) in [
                ("pending", "查看待审回复"),
                ("enable", "请求启用：仍须真实订阅配置"),
                ("pause", "暂停此绑定"),
            ] {
                out.push(Action {
                    label: format!("{label} · {} · {}", b.binding_ref, b.status),
                    data: Data {
                        operation: op.into(),
                        binding_ref: b.binding_ref.clone(),
                        expected_generation: Some(b.generation),
                        ..Default::default()
                    },
                });
            }
        }
        for p in &v.receipt.pending {
            out.push(Action {
                label: format!("私下审阅 {} · {}", p.operation_ref, p.status),
                data: Data {
                    operation: "review".into(),
                    operation_ref: p.operation_ref.clone(),
                    ..Default::default()
                },
            });
        }
        if let Some(r) = &v.receipt.review {
            let now = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_secs() as i64)
                .unwrap_or(i64::MAX);
            if r.expires > now && v.received.elapsed() < Duration::from_secs(60) {
                out.push(Action {
                    label: "确认将以上完整正文回复到所列受众（不编辑/不改目标）".into(),
                    data: Data {
                        operation: "confirm".into(),
                        operation_ref: r.operation.operation_ref.clone(),
                        expected_generation: Some(r.generation),
                        review_token: r.review_token.clone(),
                        body_sha256: r.body_sha256.clone(),
                        audience_sha256: r.audience_sha256.clone(),
                        source_id: r.operation.source_id,
                        confirmed: true,
                        ..Default::default()
                    },
                });
            }
            out.push(Action {
                label: "取消这条未发布回复".into(),
                data: Data {
                    operation: "cancel".into(),
                    operation_ref: r.operation.operation_ref.clone(),
                    ..Default::default()
                },
            });
        }
        out
    }
    fn dot_send(&mut self, mut d: Data, serial: u64, cx: &mut Context<Self>) {
        if !self.dot_visible() {
            return;
        }
        let Some(v) = self
            .dot_ui
            .view
            .as_ref()
            .filter(|v| v.serial == serial && v.pending.is_none())
        else {
            return;
        };
        d.room = v.room.clone();
        d.owner_origin = v.tag.clone();
        d.edit_generation = v.serial;
        let id = self.next_request_id("dot");
        let mut q = Request::simple("dot", &id);
        q.room = &d.room;
        q.dot = Some(&d);
        let result = self.session.as_ref().unwrap().send(&q);
        if let Some(v) = self.dot_ui.view.as_mut() {
            v.receipt.review = None;
            v.pending = Some((id, d, Instant::now()));
            v.cursor = 0;
            if result.is_err() {
                let (_, d, _) = v.pending.take().unwrap();
                v.unknown = Some(d);
                self.error =
                    Some("dot 请求发送结果不确定；禁止自动重发，请查询原操作状态。".into());
            }
        }
        cx.notify();
    }
    pub(super) fn dot_response(&mut self, r: &Response, cx: &mut Context<Self>) -> bool {
        let claimed = r.kind == "dot" || r.dot.is_some() || r.request_id.starts_with("dot:");
        if !claimed {
            return false;
        }
        if !self.dot_visible() {
            return true;
        }
        let Some(v) = self.dot_ui.view.as_ref() else {
            return true;
        };
        let Some((id, d, _)) = &v.pending else {
            return true;
        };
        if id != &r.request_id || (r.kind != "error" && r.room != v.room) {
            return true;
        }
        if r.kind == "error" && miochat_gpui::information::is_plain_error_response(r) {
            self.error = Some(r.error.clone());
            let view = self.dot_ui.view.as_mut().unwrap();
            let (_, data, _) = view.pending.take().unwrap();
            if matches!(
                data.operation.as_str(),
                "confirm" | "cancel" | "create" | "enable" | "pause"
            ) {
                view.unknown = Some(data);
            }
            cx.notify();
            return true;
        }
        let Some(receipt) = &r.dot else {
            return true;
        };
        if !miochat_gpui::dot::exclusive(r) || !miochat_gpui::dot::matches(d, receipt) {
            return true;
        }
        self.dot_ui.serial = self.dot_ui.serial.wrapping_add(1);
        let v = self.dot_ui.view.as_mut().unwrap();
        v.serial = self.dot_ui.serial;
        v.pending = None;
        v.unknown = None;
        v.receipt = receipt.clone();
        v.received = Instant::now();
        v.cursor = 0;
        if receipt
            .result
            .as_ref()
            .is_some_and(|p| p.status == "COMMITTED")
        {
            self.dot_ui.notice = Some("dot 回复已经提交；不会自动重发。".into());
        }
        cx.notify();
        true
    }
    pub(super) fn dot_tick(&mut self) {
        if self.dot_ui.view.is_some() && !self.dot_visible() {
            self.dot_ui.clear();
            return;
        }
        if let Some(v) = self.dot_ui.view.as_mut() {
            if v.pending
                .as_ref()
                .is_some_and(|(_, _, at)| at.elapsed() > Duration::from_secs(15))
            {
                let (_, d, _) = v.pending.take().unwrap();
                v.unknown = Some(d);
                self.error = Some("dot 请求结果未知；不要重复确认，可查询原操作状态。".into());
            }
            if v.received.elapsed() > Duration::from_secs(60) {
                v.receipt.review = None;
            }
        }
    }
    pub(super) fn dot_key(
        &mut self,
        e: &KeyDownEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        if !self.dot_visible() {
            let m = &e.keystroke.modifiers;
            if !e.is_held
                && !e.prefer_character_input
                && m.control
                && m.shift
                && !m.alt
                && !m.platform
                && !m.function
                && e.keystroke.key.eq_ignore_ascii_case("d")
                && self.ordinary_chat_context_allowed()
            {
                self.dot_open(window, cx);
                cx.stop_propagation();
                return true;
            }
            return false;
        }
        if !e.is_held && !e.prefer_character_input && self.marked_range.is_none() {
            let m = &e.keystroke.modifiers;
            let k = e.keystroke.key.as_str();
            if !m.modified() && k == "escape" {
                self.dot_ui.clear();
                self.focus.focus(window, cx);
            } else if !m.control && !m.platform && !m.alt && !m.function {
                let a = self.dot_actions();
                let n = a.len();
                if n > 0 {
                    let v = self.dot_ui.view.as_mut().unwrap();
                    match k {
                        "tab" => v.cursor = (v.cursor + if m.shift { n - 1 } else { 1 }) % n,
                        "up" => v.cursor = (v.cursor + n - 1) % n,
                        "down" => v.cursor = (v.cursor + 1) % n,
                        "enter" if !m.shift => {
                            let d = a[v.cursor.min(n - 1)].data.clone();
                            let serial = v.serial;
                            self.dot_send(d, serial, cx);
                        }
                        _ => {}
                    }
                }
            }
        }
        cx.notify();
        cx.stop_propagation();
        true
    }
    pub(super) fn dot_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let v = self.dot_ui.view.as_ref().unwrap();
        let serial = v.serial;
        let mut p=div().id("dot-private-review").absolute().top(px(24.)).bottom(px(24.)).left(px(24.)).right(px(24.)).bg(self.raised_color()).border_1().p(px(16.)).overflow_y_scroll().occlude().track_focus(&self.menu_focus).on_click(|_,_,cx|cx.stop_propagation()).flex().flex_col().gap(px(10.)).child("dot · 房间拥有者私有审阅 · Tab/方向键选择，Enter执行，Esc关闭").child("默认关闭；启用请求不等于真实 host 订阅已配置。没有自动发送。正文和受众须逐条确认。");
        if let Some(r) = &v.receipt.review {
            p = p
                .child(format!(
                    "原消息 #{} · 房间 {} · 受众 {} · UID {:?}",
                    r.operation.source_id, r.room, r.audience_label, r.audience_uids
                ))
                .child(div().whitespace_normal().child(r.body.clone()))
                .child(format!("正文 SHA256 {}", r.body_sha256));
        }
        if let Some(r) = &v.receipt.result {
            p = p.child(format!(
                "操作 {}：{} · 消息 #{}",
                r.operation_ref, r.status, r.message_id
            ));
        }
        if v.pending.is_some() {
            p = p.child("请求处理中；请勿重复操作。");
        }
        if v.unknown.is_some() {
            p = p.child("结果未知。断线/取消并不等于回滚；仅查原操作，不重发。");
        }
        p.children(self.dot_actions().into_iter().enumerate().map(|(i, a)| {
            let captured = a.clone();
            Self::button(gpui::SharedString::from(format!("dot-action-{i}")), a.label)
                .when(i == v.cursor, |d| d.bg(rgb(0xdde8ef)))
                .on_click(cx.listener(move |this, _, _, cx| {
                    if this.dot_visible()
                        && this
                            .dot_ui
                            .view
                            .as_ref()
                            .is_some_and(|v| v.serial == serial)
                    {
                        let actions = this.dot_actions();
                        if let Some(data) = captured_pointer_action(
                            this.dot_ui.view.as_ref().unwrap().serial,
                            serial,
                            &actions,
                            &captured,
                        ) {
                            this.dot_send(data, serial, cx);
                        }
                    }
                }))
        }))
        .child(
            Self::button("dot-close", "关闭私有审阅").on_click(cx.listener(
                |this, _, window, cx| {
                    this.dot_ui.clear();
                    this.focus.focus(window, cx);
                    cx.notify();
                },
            )),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn private_dot_old_pointer_never_becomes_new_confirmation() {
        let read = Action {
            label: "review".into(),
            data: Data {
                operation: "review".into(),
                operation_ref: "same".into(),
                ..Default::default()
            },
        };
        let confirm = Action {
            label: "confirm exact".into(),
            data: Data {
                operation: "confirm".into(),
                operation_ref: "same".into(),
                confirmed: true,
                ..Default::default()
            },
        };
        assert!(captured_pointer_action(2, 1, std::slice::from_ref(&confirm), &read).is_none());
        assert!(captured_pointer_action(1, 1, std::slice::from_ref(&confirm), &read).is_none());
        assert!(captured_pointer_action(2, 2, std::slice::from_ref(&confirm), &confirm).is_some());
    }
    #[test]
    fn private_dot_close_preserves_only_non_sensitive_uncertainty() {
        let mut state = State {
            view: Some(View {
                owner: "alice".into(),
                room: "private".into(),
                origin: miochat_gpui::composition::Origin::synthetic("fixture", 1),
                generation: 1,
                tag: "o".into(),
                serial: 1,
                receipt: Receipt {
                    review: Some(miochat_gpui::dot::Review {
                        body: "synthetic private body".into(),
                        review_token: "synthetic secret token".into(),
                        ..Default::default()
                    }),
                    ..Default::default()
                },
                pending: Some((
                    "dot:1:1".into(),
                    Data {
                        operation: "confirm".into(),
                        operation_ref: "private-operation".into(),
                        ..Default::default()
                    },
                    Instant::now(),
                )),
                unknown: None,
                cursor: 0,
                received: Instant::now(),
            }),
            ..Default::default()
        };
        state.clear();
        assert!(state.view.is_none());
        let n = state.notice.unwrap();
        assert!(n.contains("可能"));
        assert!(!n.contains("alice") && !n.contains("private") && !n.contains("synthetic"));
    }
}
