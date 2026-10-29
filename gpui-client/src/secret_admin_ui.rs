use super::*;
use miochat_gpui::{
    composition::Origin,
    direct_message::{DirectMessageForm, FormPurpose},
    secret_admin::{Attempt, Binding, Kind, State},
};

fn prepared(command: &str, owner: &str, room: &str, generation: u64) -> Option<DirectMessageForm> {
    let (head, args) = miochat_gpui::configuration::split_argument(command);
    let kind = Kind::from_command(head)?;
    let mut form = DirectMessageForm::new(owner.into(), room.into(), generation);
    form.purpose = FormPurpose::SecretAdmin(kind);
    if kind == Kind::Resetpass {
        form.target = args.into();
    } else {
        let mut args = args.split_whitespace();
        form.target = args.next().unwrap_or("1d").into();
        form.body = args.next().unwrap_or("1").into();
        // Extra arguments are not silently discarded from a native confirmation.
        if args.next().is_some() {
            form.body = "invalid".into();
        }
    }
    Some(form)
}
fn controls_allowed(event: &KeyDownEvent, marked: bool) -> bool {
    !event.is_held && !event.prefer_character_input && !marked
}
fn claimed(response: &Response, attempt: Option<&Attempt>) -> bool {
    response.secret_admin.is_some()
        || response.kind == "secret_admin"
        || attempt.is_some_and(|a| a.id == response.request_id)
}
fn origin_matches(captured: Option<&Origin>, current: Option<&Origin>) -> bool {
    captured.is_some() && captured == current
}
fn admitted(
    response: &Response,
    attempt: Option<&Attempt>,
    screen: Screen,
    identity: (&str, &str, u64, Option<&Origin>, u64),
) -> bool {
    let (owner, room, generation, origin, epoch) = identity;
    screen == Screen::Chat
        && attempt
            .is_some_and(|a| a.bound(owner, room, generation, origin, epoch) && a.admits(response))
}
impl MioChatApp {
    pub(super) fn secret_form_context_allowed(&self) -> bool {
        self.direct_form
            .as_ref()
            .is_none_or(|f| f.secret_admin_kind().is_none())
            || origin_matches(
                self.secret_form_origin.as_ref(),
                self.session_origin.as_ref(),
            )
    }
    pub(super) fn discard_secret_admin(&mut self) {
        let had = self.secret_admin.is_some() || self.secret_form_origin.is_some();
        miochat_gpui::secret_admin::discard(&mut self.secret_admin, &mut self.secret_notice);
        self.secret_form_origin = None;
        self.secret_open_input = None;
        if self
            .direct_form
            .as_ref()
            .is_some_and(|f| f.secret_admin_kind().is_some())
        {
            self.direct_form = None;
        }
        self.secret_cursor = 0;
        if had {
            self.reset_selection();
        }
    }
    pub(super) fn secret_admin_visible(&self) -> bool {
        self.screen == Screen::Chat
            && self.secret_admin.as_ref().is_some_and(|a| {
                a.bound(
                    &self.user,
                    &self.room,
                    self.generation,
                    self.session_origin.as_ref(),
                    self.moderation_epoch,
                ) && a.visible(self.persistence_now())
            })
    }
    pub(super) fn activate_secret_admin(
        &mut self,
        command: &str,
        preserve_draft: bool,
        cx: &mut Context<Self>,
    ) -> bool {
        let Some(form) = prepared(command, &self.user, &self.room, self.generation) else {
            return false;
        };
        if self.screen != Screen::Chat
            || !self.native_actions
            || self.session.is_none()
            || self.session_origin.is_none()
            || self.direct_form.is_some()
            || self.pending_send.is_some()
            || self.profile_editor.is_some()
            || self.room_form.is_some()
            || self.composition.is_some()
            || self.word_start.is_some()
            || self.marked_range.is_some()
            || self.secret_admin.is_some()
        {
            self.error = Some("请先完成或关闭当前输入/一次性结果，再开启管理凭据确认。".into());
            cx.notify();
            return true;
        }
        self.open_direct_form(cx);
        self.direct_form = Some(form);
        self.secret_form_origin = self.session_origin.clone();
        self.secret_open_input =
            (!preserve_draft && self.composer == command).then(|| self.composer.clone());
        self.field = Field::DirectTarget;
        self.reset_selection();
        cx.notify();
        true
    }
    pub(super) fn submit_secret_admin(&mut self, cx: &mut Context<Self>) {
        if !self.direct_input_active()
            || self.marked_range.is_some()
            || self.pending_send.is_some()
            || self.secret_admin.is_some()
        {
            return;
        }
        let Some(form) = self.direct_form.as_ref() else {
            return;
        };
        let Some(kind) = form.secret_admin_kind() else {
            return;
        };
        if !form.bound_to(&self.user, &self.room, self.generation) || form.pending() {
            return;
        }
        let data = match kind.data(&form.target, &form.body) {
            Ok(data) => data,
            Err(e) => {
                self.error = Some(e.into());
                cx.notify();
                return;
            }
        };
        let Some(origin) = self.secret_form_origin.clone() else {
            return;
        };
        let id = self.next_request_id("secret-admin");
        let now = self.persistence_now();
        self.secret_admin = Some(Attempt::new(
            data.clone(),
            id.clone(),
            Binding {
                owner: self.user.clone(),
                room: self.room.clone(),
                generation: self.generation,
                origin,
                epoch: self.moderation_epoch,
            },
            now,
            self.secret_open_input.take(),
        ));
        self.direct_form.as_mut().unwrap().begin(id.clone());
        let mut request = Request::simple("secret_admin", &id);
        request.room = &self.room;
        request.secret_admin = Some(&data);
        if self.send_request(&request).is_err() {
            self.discard_secret_admin();
        }
        cx.notify();
    }
    pub(super) fn handle_secret_admin_response(
        &mut self,
        response: &Response,
        cx: &mut Context<Self>,
    ) -> bool {
        if !claimed(response, self.secret_admin.as_ref()) {
            // Fresh own-profile loss or accepted state navigation discards all
            // private output before the general renderer can update anything.
            if response.kind == "state"
                || response.kind == "reset_required"
                || response.profile.as_ref().is_some_and(|p| {
                    p.own
                        && p.viewer == self.user
                        && p.name == self.user
                        && !matches!(p.role.as_str(), "owner" | "admin")
                })
            {
                self.discard_secret_admin();
            }
            return false;
        }
        let now = self.persistence_now();
        let matching = self
            .secret_admin
            .as_ref()
            .is_some_and(|a| a.id == response.request_id);
        let valid = admitted(
            response,
            self.secret_admin.as_ref(),
            self.screen,
            (
                &self.user,
                &self.room,
                self.generation,
                self.session_origin.as_ref(),
                self.moderation_epoch,
            ),
        );
        if !valid {
            if matching {
                self.discard_secret_admin();
            }
            // Any secret-bearing stale/mixed frame is consumed, never delivered
            // to profile/preferences/center or another pending operation.
            cx.notify();
            return true;
        }
        let a = self.secret_admin.as_mut().unwrap();
        a.receive(response, now);
        let consume =
            a.committed == Some(true) && a.opening_input.as_deref() == Some(self.composer.as_str());
        let state = a.state;
        self.direct_form = None;
        self.secret_form_origin = None;
        self.secret_open_input = None;
        if consume {
            self.composer.clear();
            self.reset_selection();
        }
        if state != State::Shown {
            if state == State::Rejected {
                self.error =
                    Some("管理凭据请求未完成；请核对当前权限、目标与参数。未自动重试。".into());
            }
            self.discard_secret_admin();
        }
        self.secret_cursor = 0;
        cx.notify();
        true
    }
    pub(super) fn poll_secret_admin(&mut self, cx: &mut Context<Self>) {
        let now = self.persistence_now();
        let invalid = self.secret_admin.as_ref().is_some_and(|a| {
            self.screen != Screen::Chat
                || !a.bound(
                    &self.user,
                    &self.room,
                    self.generation,
                    self.session_origin.as_ref(),
                    self.moderation_epoch,
                )
        });
        let expired = self.secret_admin.as_mut().is_some_and(|a| a.expire(now));
        if invalid || expired {
            self.discard_secret_admin();
            cx.notify();
        }
    }
    fn secret_result_action(&mut self, index: usize, cx: &mut Context<Self>) {
        if !self.secret_admin_visible() || self.marked_range.is_some() {
            return;
        }
        let now = self.persistence_now();
        match index {
            0 => {
                let a = self.secret_admin.as_mut().unwrap();
                a.revealed = !a.revealed;
            }
            1 => {
                if let Some(value) = self.secret_admin.as_ref().and_then(|a| a.value(now)) {
                    cx.write_to_clipboard(gpui::ClipboardItem::new_string(value.into()));
                }
            }
            _ => self.discard_secret_admin(),
        }
        cx.notify();
    }
    pub(super) fn secret_admin_key(
        &mut self,
        event: &KeyDownEvent,
        cx: &mut Context<Self>,
    ) -> bool {
        if self.secret_admin_visible() {
            if controls_allowed(event, self.marked_range.is_some()) {
                match event.keystroke.key.as_str() {
                    "escape" => self.discard_secret_admin(),
                    "tab" => {
                        self.secret_cursor = (self.secret_cursor
                            + if event.keystroke.modifiers.shift {
                                2
                            } else {
                                1
                            })
                            % 3
                    }
                    "enter" => self.secret_result_action(self.secret_cursor, cx),
                    _ => {}
                }
            }
            cx.stop_propagation();
            cx.notify();
            return true;
        }
        if self
            .direct_form
            .as_ref()
            .is_some_and(|f| f.secret_admin_kind().is_some())
            && matches!(event.keystroke.key.as_str(), "enter" | "escape" | "tab")
            && !controls_allowed(event, self.marked_range.is_some())
        {
            cx.stop_propagation();
            return true;
        }
        false
    }
    pub(super) fn secret_admin_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let a = self.secret_admin.as_ref().expect("visible one-time output");
        let text = if a.revealed {
            a.value(self.persistence_now()).unwrap_or("").to_string()
        } else {
            "••••••••••••••••••••••••••••••••".into()
        };
        let labels = [
            if a.revealed { "隐藏" } else { "明确显示" },
            "复制一次性值到剪贴板",
            "关闭并清除",
        ];
        div().id("secret-admin-overlay").absolute().top(px(0.)).left(px(0.)).size_full().bg(gpui::rgba(0x00000060)).on_click(|_,_,cx|cx.stop_propagation()).child(
            div().id("secret-admin-result").absolute().top(px(72.)).left(px(24.)).right(px(24.)).bottom(px(24.)).overflow_y_scroll().p(px(20.)).rounded(px(12.)).bg(self.raised_color()).flex().flex_col().gap(px(16.))
            .child(a.data.kind.title()).child(if a.data.kind==Kind::Resetpass {format!("实际目标 @{} · 原请求 {}",a.canonical_target,a.data.target)} else {format!("使用次数 {} · 到期Unix {}（0为永久）",a.data.uses,a.expires)}).child("已提交。此结果仅保留60秒；关闭、导航或凭据变化即清除，不能重新读取。请明确、安全地交付，勿自动重复操作。")
            .child(div().font_family("monospace").child(text))
            .children(labels.into_iter().enumerate().map(|(i,label)|Self::button(["secret-reveal","secret-copy","secret-close"][i],if i==self.secret_cursor {format!("› {label}")}else{label.into()}).on_click(cx.listener(move|this,_,_,cx|this.secret_result_action(i,cx)))))
            .child("复制会明确披露到系统剪贴板；本程序不承诺清除系统剪贴板或安全擦除内存。Tab选择 · Enter确认 · Esc关闭")
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn actual_secret_form_routes_separate_draft_and_bind_origin() {
        let f = prepared("/resetpass bob", "admin", "lobby", 9).unwrap();
        assert_eq!(f.target, "bob");
        assert!(!f.has_body());
        assert_eq!(f.command("admin", "lobby", 9).unwrap(), "/resetpass bob");
        assert!(f.command("admin", "other", 9).is_err());
        assert!(f.command("other", "lobby", 9).is_err());
        let f = prepared("/invitecode", "admin", "lobby", 9).unwrap();
        assert_eq!(f.command("admin", "lobby", 9).unwrap(), "/invitecode 1d 1");
        let a = Origin::synthetic("server-a", 9);
        let b = Origin::synthetic("server-b", 9);
        assert!(origin_matches(Some(&a), Some(&a)));
        assert!(!origin_matches(Some(&a), Some(&b)));
        assert!(!origin_matches(None, None));
        assert!(!claimed(
            &Response {
                kind: "state".into(),
                ..Default::default()
            },
            None
        ));
        assert!(claimed(
            &Response {
                kind: "secret_admin".into(),
                ..Default::default()
            },
            None
        ));
    }
    #[test]
    fn actual_secret_early_gate_and_keyboard_reject_mixed_stale_and_repeated_input() {
        let origin = Origin::synthetic("fixture", 7);
        let a = Attempt::new(
            Kind::Resetpass.data("BOB", "").unwrap(),
            "s1".into(),
            Binding {
                owner: "admin".into(),
                room: "lobby".into(),
                generation: 7,
                origin: origin.clone(),
                epoch: 9,
            },
            0,
            None,
        );
        let mut r:Response=serde_json::from_value(serde_json::json!({"type":"secret_admin","request_id":"s1","room":"lobby","secret_admin":{"kind":"resetpass","requested_target":"BOB","target":"bob","duration":"","uses":0,"expires":0,"state":"shown","committed":true,"value":"0123456789abcdef0123456789abcdef"}})).unwrap();
        let identity = ("admin", "lobby", 7, Some(&origin), 9);
        assert!(claimed(&r, Some(&a)));
        assert!(admitted(&r, Some(&a), Screen::Chat, identity));
        for screen in [
            Screen::Connect,
            Screen::Auth,
            Screen::Password,
            Screen::Trust,
            Screen::KeyBinding,
        ] {
            assert!(!admitted(&r, Some(&a), screen, identity));
        }
        assert!(!admitted(
            &r,
            Some(&a),
            Screen::Chat,
            ("admin", "other", 7, Some(&origin), 9)
        ));
        assert!(!admitted(
            &r,
            Some(&a),
            Screen::Chat,
            ("admin", "lobby", 8, Some(&origin), 9)
        ));
        r.preferences = Some(Default::default());
        assert!(!admitted(&r, Some(&a), Screen::Chat, identity));
        r.kind = "error".into();
        assert!(!admitted(&r, Some(&a), Screen::Chat, identity));
        r.preferences = None;
        r.secret_admin = None;
        assert!(admitted(&r, Some(&a), Screen::Chat, identity));
        r.request_id = "old".into();
        assert!(!claimed(&r, Some(&a)));
        assert!(!admitted(&r, Some(&a), Screen::Chat, identity));
        let mut event = KeyDownEvent {
            keystroke: gpui::Keystroke {
                key: "enter".into(),
                ..Default::default()
            },
            is_held: false,
            prefer_character_input: false,
        };
        assert!(controls_allowed(&event, false));
        event.is_held = true;
        assert!(!controls_allowed(&event, false));
        event.is_held = false;
        event.prefer_character_input = true;
        assert!(!controls_allowed(&event, false));
        event.prefer_character_input = false;
        assert!(!controls_allowed(&event, true));
    }
}
