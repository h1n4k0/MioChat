use super::*;
use miochat_gpui::completion::{self, Context as CompletionContext, Intent};

#[derive(Clone, Copy, PartialEq, Eq)]
enum Key {
    Begin,
    Next,
    Previous,
    Choose,
    Cancel,
    Ignore,
}
fn route(event: &KeyDownEvent, eligible: bool, active: bool, ready: bool) -> Option<Key> {
    if !eligible {
        return None;
    }
    // A held Enter after choosing must not fall through to ordinary send once
    // the intent has been consumed. New non-held Enter still uses normal send.
    if event.is_held && event.keystroke.key == "enter" {
        return Some(Key::Ignore);
    }
    if event.is_held || event.prefer_character_input {
        return (active
            && matches!(
                event.keystroke.key.as_str(),
                "enter" | "tab" | "up" | "down" | "escape"
            ))
        .then_some(Key::Ignore);
    }
    let m = &event.keystroke.modifiers;
    if m.control || m.alt || m.platform || m.function {
        return (active && event.keystroke.key == "enter").then_some(Key::Ignore);
    }
    match event.keystroke.key.as_str() {
        "tab" if active => Some(if m.shift { Key::Previous } else { Key::Next }),
        "tab" if !m.shift => Some(Key::Begin),
        "up" if active && !m.shift && ready => Some(Key::Previous),
        "down" if active && !m.shift && ready => Some(Key::Next),
        "enter" if active && !m.shift => Some(Key::Choose),
        "escape" if active && !m.shift => Some(Key::Cancel),
        _ => None,
    }
}
// The final ordinary-chat send branch uses this even after completion context
// has disappeared or a different pending read makes its eligibility false.
pub(super) fn ordinary_chat_enter_is_press(event: &KeyDownEvent) -> bool {
    !event.is_held && !event.prefer_character_input
}
fn response_route(response: &Response, intent: Option<&Intent>, current: bool) -> Option<bool> {
    completion::claims(response, intent)
        .then(|| current && intent.is_some_and(|i| i.admits(response)))
}
impl MioChatApp {
    pub(super) fn cancel_completion(&mut self) {
        self.completion = None;
        self.completion_revision = self.completion_revision.wrapping_add(1);
    }
    fn completion_context(&self) -> CompletionContext<'_> {
        CompletionContext {
            enabled: self.ordinary_chat_context_allowed()
                && !self.persistence.quarantined()
                && (!self.persistence_enabled || self.persistence.ready()),
            owner: &self.user,
            room: &self.room,
            origin: self.session_origin.as_ref(),
            generation: self.generation,
            revision: self.completion_revision,
        }
    }
    pub(super) fn completion_current(&self) -> bool {
        self.completion.as_ref().is_some_and(|i| {
            self.persistence_now().saturating_sub(i.started) <= 5000
                && i.current(
                    self.completion_context(),
                    &self.composer,
                    self.selection.clone(),
                    input_history::safe_text(&self.composer),
                )
        })
    }
    pub(super) fn poll_completion(&mut self) {
        if self.completion.is_some()
            && (!self.completion_current()
                || self
                    .completion
                    .as_ref()
                    .is_some_and(|i| self.persistence_now().saturating_sub(i.started) > 5000))
        {
            self.cancel_completion();
        }
    }
    fn begin_completion(&mut self, cx: &mut Context<Self>) {
        if !self.completion_context().enabled {
            return;
        }
        let Some(token) = completion::token(
            &self.composer,
            self.selection.clone(),
            input_history::safe_text(&self.composer),
        ) else {
            return;
        };
        if !self.completion_enabled {
            self.error =
                Some("服务器未提供 completion-v1；不会上传整段草稿或模拟完整补全。".into());
            return;
        }
        let id = self.next_request_id("completion");
        let Some(intent) = Intent::new(
            id.clone(),
            token,
            self.completion_context(),
            self.persistence_now(),
        ) else {
            return;
        };
        let data = intent.data();
        let mut request = Request::simple("completion", &id);
        request.room = &self.room;
        request.completion = Some(&data);
        match self.session.as_ref().unwrap().send(&request) {
            Ok(()) => {
                self.completion = Some(intent);
                self.error = None
            }
            Err(e) => self.error = Some(e.to_string()),
        }
        cx.notify();
    }
    fn choose_completion(&mut self, id: &str, index: usize, cx: &mut Context<Self>) {
        if !self.completion_current() || self.completion.as_ref().is_none_or(|i| i.id != id) {
            return;
        }
        let intent = self.completion.take().unwrap();
        if !intent.insert(
            index,
            &mut self.composer,
            &mut self.selection,
            &mut self.marked_range,
        ) {
            self.error = Some("补全内容未插入，原草稿保留。".into());
        }
        self.completion_revision = self.completion_revision.wrapping_add(1);
        cx.notify();
    }
    pub(super) fn completion_key(
        &mut self,
        event: &KeyDownEvent,
        window: &Window,
        cx: &mut Context<Self>,
    ) -> bool {
        let had = self.completion.is_some();
        self.poll_completion();
        if had
            && self.completion.is_none()
            && matches!(
                event.keystroke.key.as_str(),
                "enter" | "tab" | "up" | "down" | "escape"
            )
        {
            cx.notify();
            cx.stop_propagation();
            return true;
        }
        let eligible = self.completion_context().enabled && self.focus.is_focused(window);
        let active = self.completion.is_some();
        let ready = self
            .completion
            .as_ref()
            .is_some_and(|i| i.choices.is_some());
        let Some(key) = route(event, eligible, active, ready) else {
            return false;
        };
        match key {
            Key::Ignore => {}
            Key::Begin => self.begin_completion(cx),
            Key::Cancel => self.cancel_completion(),
            Key::Choose => {
                if let Some(i) = self.completion.as_ref().filter(|i| i.choices.is_some()) {
                    let id = i.id.clone();
                    let cursor = i.cursor;
                    self.choose_completion(&id, cursor, cx);
                }
            }
            Key::Next | Key::Previous => {
                if let Some(i) = self.completion.as_mut() {
                    i.cycle(key == Key::Previous);
                }
            }
        }
        // Pending Enter is consumed, not a second request and not a chat send.
        cx.notify();
        cx.stop_propagation();
        true
    }
    pub(super) fn handle_completion_response(
        &mut self,
        response: &Response,
        cx: &mut Context<Self>,
    ) -> bool {
        let Some(admitted) = response_route(
            response,
            self.completion.as_ref(),
            self.completion_current(),
        ) else {
            return false;
        };
        let matching = self.completion.as_ref().is_some_and(|i| i.claims(response));
        if !admitted {
            if matching {
                self.cancel_completion();
                self.error = Some("补全响应已过期或范围不匹配；原输入未改变。".into());
            }
            return true;
        }
        if response.kind == "error" {
            self.cancel_completion();
            self.error = Some(response.error.clone());
            return true;
        }
        let choices = response.completion.as_ref().unwrap().choices.clone();
        if choices.is_empty() {
            self.cancel_completion();
            self.status = "没有匹配的补全项".into();
        } else {
            let id = {
                let i = self.completion.as_mut().unwrap();
                i.choices = Some(choices);
                i.id.clone()
            };
            if self
                .completion
                .as_ref()
                .unwrap()
                .choices
                .as_ref()
                .unwrap()
                .len()
                == 1
            {
                self.choose_completion(&id, 0, cx);
            }
        }
        cx.notify();
        true
    }
    pub(super) fn completion_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let mut panel = div()
            .absolute()
            .bottom(px(135.))
            .right(px(30.))
            .w(px(340.))
            .max_h(px(320.))
            .id("completion-choices")
            .overflow_y_scroll()
            .rounded(px(12.))
            .border_1()
            .border_color(rgb(BORDER))
            .bg(self.raised_color())
            .p(px(12.))
            .flex()
            .flex_col()
            .gap(px(5.))
            .child("Tab 补全 · Enter 插入，不发送 · Esc 关闭");
        if let Some(intent) = self
            .completion
            .as_ref()
            .filter(|_| self.completion_current())
        {
            if let Some(choices) = &intent.choices {
                let first = (intent.cursor / 8) * 8;
                panel = panel.child(format!(
                    "{} / {} · ↑↓ 或 Tab/Shift+Tab 切换",
                    intent.cursor + 1,
                    choices.len()
                ));
                for (index, value) in choices.iter().enumerate().skip(first).take(8) {
                    let id = intent.id.clone();
                    panel = panel.child(
                        div()
                            .id(("completion-choice", index))
                            .role(gpui::Role::Button)
                            .aria_label(value.clone())
                            .p(px(6.))
                            .rounded(px(4.))
                            .cursor_pointer()
                            .bg(if index == intent.cursor {
                                rgb(0xdce8f3).into()
                            } else {
                                self.raised_color()
                            })
                            .child(value.clone())
                            .on_click(cx.listener(move |this, _, _, cx| {
                                this.choose_completion(&id, index, cx)
                            })),
                    );
                }
                if choices.len() > 8 {
                    for (back, label) in [(true, "上一项"), (false, "下一项")] {
                        let id = intent.id.clone();
                        panel = panel.child(
                            Self::button(
                                if back {
                                    "completion-prev"
                                } else {
                                    "completion-next"
                                },
                                label,
                            )
                            .on_click(cx.listener(
                                move |this, _, _, cx| {
                                    if this.completion_current()
                                        && let Some(i) =
                                            this.completion.as_mut().filter(|i| i.id == id)
                                    {
                                        i.cycle(back);
                                        cx.notify();
                                    }
                                },
                            )),
                        );
                    }
                }
            } else {
                panel = panel.child("正在读取前缀候选…（只发送当前词片段）");
            }
        }
        panel
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn event(s: &str) -> KeyDownEvent {
        KeyDownEvent {
            keystroke: gpui::Keystroke::parse(s).unwrap(),
            is_held: false,
            prefer_character_input: false,
        }
    }
    #[test]
    fn actual_completion_choose_then_held_enter_never_falls_through_to_send() {
        let press = event("enter");
        assert!(route(&press, true, true, true) == Some(Key::Choose));
        // choose_completion takes the intent; the same physical press repeats.
        let mut repeated = event("enter");
        repeated.is_held = true;
        assert!(route(&repeated, true, false, false) == Some(Key::Ignore));
        assert!(route(&repeated, true, true, false) == Some(Key::Ignore));
        assert!(route(&press, true, false, false).is_none());
        assert!(route(&repeated, false, false, false).is_none()); // dedicated route does not claim it
        assert!(!ordinary_chat_enter_is_press(&repeated)); // actual final Chat send gate still rejects
        assert!(ordinary_chat_enter_is_press(&press));
    }
    #[test]
    fn actual_completion_early_gate_blocks_cancelled_mixed_error_before_global_effects() {
        let origin = miochat_gpui::composition::Origin::synthetic("fixture", 1);
        let t = completion::token("/tim", 4..4, true).unwrap();
        let intent = Intent::new(
            "completion:7:1".into(),
            t,
            CompletionContext {
                enabled: true,
                owner: "alice",
                room: "lobby",
                origin: Some(&origin),
                generation: 7,
                revision: 1,
            },
            0,
        )
        .unwrap();
        let good = Response {
            kind: "completion".into(),
            request_id: intent.id.clone(),
            room: "lobby".into(),
            completion: Some(completion::Receipt {
                kind: completion::Kind::Command,
                prefix: "/tim".into(),
                choices: vec!["/time".into()],
            }),
            ..Default::default()
        };
        assert_eq!(response_route(&good, Some(&intent), true), Some(true));
        assert_eq!(response_route(&good, Some(&intent), false), Some(false));
        for kind in ["error", "state", "action"] {
            let mixed = Response {
                kind: kind.into(),
                request_id: intent.id.clone(),
                preferences: Some(Default::default()),
                ..Default::default()
            };
            assert_eq!(response_route(&mixed, Some(&intent), true), Some(false));
            assert_eq!(response_route(&mixed, None, false), Some(false));
        }
        assert_eq!(
            response_route(
                &Response {
                    kind: "state".into(),
                    preferences: Some(Default::default()),
                    ..Default::default()
                },
                Some(&intent),
                true
            ),
            None
        );
    }
    #[test]
    fn actual_completion_key_route_consumes_pending_enter_and_never_sends() {
        assert!(route(&event("tab"), true, false, false) == Some(Key::Begin));
        assert!(route(&event("shift-tab"), true, false, false).is_none());
        assert!(route(&event("enter"), true, true, false) == Some(Key::Choose));
        assert!(route(&event("tab"), true, true, false) == Some(Key::Next));
        assert!(route(&event("shift-tab"), true, true, true) == Some(Key::Previous));
        for key in ["ctrl-tab", "alt-tab", "cmd-tab", "shift-enter"] {
            assert!(route(&event(key), true, false, false).is_none());
        }
        let mut e = event("enter");
        e.is_held = true;
        assert!(route(&e, true, true, false) == Some(Key::Ignore));
        e.is_held = false;
        e.prefer_character_input = true;
        assert!(route(&e, true, true, true) == Some(Key::Ignore));
        assert!(route(&event("tab"), false, false, false).is_none());
    }
    #[test]
    fn actual_completion_sensitive_preflight_is_shared_with_history_before_token_capture() {
        for text in [
            "/passwd secret",
            "/pass\u{200b}wd secret",
            "\u{feff}/key-bind secret",
            "/word \nstart hidden",
            "/resetpass bob",
            "/invitecode 1",
        ] {
            assert!(
                completion::token(text, text.len()..text.len(), input_history::safe_text(text))
                    .is_none()
            );
        }
        assert!(completion::token("/msg bo", 7..7, input_history::safe_text("/msg bo")).is_some());
    }
}
