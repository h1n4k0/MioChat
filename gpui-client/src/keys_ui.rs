use super::*;

impl MioChatApp {
    pub(super) fn secret_field(&self) -> bool {
        self.word_start.is_some()
            || self.field == Field::WordSecret
            || self.field == Field::KeyPassphrase
            || matches!(
                self.screen,
                Screen::Connect | Screen::Auth | Screen::Password | Screen::KeyBinding
            ) && matches!(self.field, Field::Password | Field::Confirm | Field::Invite)
    }
    pub(super) fn clear_key_secrets(&mut self) {
        self.identity_passphrase.clear();
        self.pending_key_bind = None;
        self.pending_key_login = None;
    }
    pub(super) fn fail_key_request(&mut self, id: &str) {
        if self.pending_key_bind.as_deref() == Some(id) {
            self.pending_key_bind = None;
            self.password.clear();
        }
        if self.pending_key_login.as_deref() == Some(id) {
            self.pending_key_login = None;
        }
    }
    pub(super) fn authenticate_key(&mut self, cx: &mut Context<Self>) {
        if self.screen != Screen::Auth
            || !self.login_gate.can_submit(self.generation)
            || !self.device_keys_enabled
            || self.pending_key_login.is_some()
        {
            return;
        }
        let id = self.next_request_id("key-login");
        if let Err(error) = self.send_request(&Request::simple("login_key", &id)) {
            self.error = Some(format!("{error:#}"));
        } else {
            self.pending_key_login = Some(id);
            self.status = "正在验证设备密钥…".into();
        }
        cx.notify();
    }
    pub(super) fn bind_device_key(&mut self, cx: &mut Context<Self>) {
        if self.screen != Screen::KeyBinding
            || !self.device_keys_enabled
            || self.device_key.is_none()
            || self.pending_key_bind.is_some()
        {
            return;
        }
        if self.key_label.chars().count() > 40 {
            self.error = Some("设备名称最多40个字符。".into());
            cx.notify();
            return;
        }
        let id = self.next_request_id("bind-key");
        let mut request = Request::simple("bind_key", &id);
        request.body = &self.key_label;
        request.password = &self.password;
        let result = self.send_request(&request);
        self.password.clear();
        match result {
            Ok(()) => {
                self.pending_key_bind = Some(id);
                self.status = "正在验证并绑定本次设备密钥…".into();
            }
            Err(error) => self.error = Some(format!("{error:#}")),
        }
        cx.notify();
    }
    pub(super) fn finish_key_binding(&mut self, response: Response) {
        if self.pending_key_bind.as_deref() != Some(&response.request_id) {
            return;
        }
        self.pending_key_bind = None;
        self.password.clear();
        self.device_key = response.device_key;
        self.screen = Screen::Chat;
        self.status = response.status;
        self.error = None;
        self.reset_selection();
    }
    pub(super) fn cancel_key_binding(&mut self, cx: &mut Context<Self>) {
        if self.pending_key_bind.is_some() {
            return;
        }
        self.password.clear();
        self.screen = Screen::Chat;
        self.reset_selection();
        cx.notify();
    }
    pub(super) fn choose_identity(&mut self, cx: &mut Context<Self>) {
        if !self.connection_field_enabled(Field::Identity) {
            return;
        }
        let generation = self.generation;
        let selection = cx.prompt_for_paths(gpui::PathPromptOptions {
            files: true,
            directories: false,
            multiple: false,
            prompt: Some("选择本次连接使用的设备私钥".into()),
        });
        cx.spawn(async move |this, cx| {
            let result = selection.await;
            let _ = this.update(cx, |app, cx| {
                if app.generation != generation || !app.connection_field_enabled(Field::Identity) {
                    return;
                }
                match result {
                    Ok(Ok(Some(paths))) if paths.len() == 1 => {
                        app.identity_file = paths[0].to_string_lossy().into_owned();
                        app.identity_passphrase.clear();
                        app.field = Field::KeyPassphrase;
                        app.reset_selection();
                    }
                    Ok(Ok(None)) => {}
                    _ => {
                        app.error =
                            Some("无法选择设备密钥；可手动输入本次要使用的文件路径。".into())
                    }
                }
                cx.notify();
            });
        })
        .detach();
    }
    pub(super) fn identity_controls(&self, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .flex()
            .flex_col()
            .gap(px(12.))
            .child(
                div()
                    .flex()
                    .gap(px(8.))
                    .child(
                        Self::button("choose-device-key", "选择设备密钥（可选）")
                            .on_click(cx.listener(|this, _, _, cx| this.choose_identity(cx))),
                    )
                    .when(!self.identity_file.is_empty(), |d| {
                        d.child(Self::button("clear-device-key", "不使用密钥").on_click(
                            cx.listener(|this, _, _, cx| {
                                if !this.connection_field_enabled(Field::Identity) {
                                    return;
                                }
                                this.identity_file.clear();
                                this.identity_passphrase.clear();
                                this.field = Field::Host;
                                this.reset_selection();
                                cx.notify();
                            }),
                        ))
                    }),
            )
            .child(self.input(
                "设备私钥路径",
                &self.identity_file,
                Field::Identity,
                false,
                cx,
            ))
            .when(!self.identity_file.is_empty(), |d| {
                d.child(self.input(
                    "私钥口令（仅本次连接）",
                    &self.identity_passphrase,
                    Field::KeyPassphrase,
                    true,
                    cx,
                ))
            })
            .child(
                div()
                    .text_size(px(11.))
                    .text_color(rgb(MUTED))
                    .child("只读取你明确选择的密钥；不搜索默认密钥、不使用代理、不保存口令。"),
            )
    }
    pub(super) fn render_key_binding(&self, cx: &mut Context<Self>) -> impl IntoElement {
        self.centered(self.panel()
            .child(self.heading("绑定当前设备","只绑定本次 SSH 已完成签名证明的密钥，不接收粘贴的公钥。"))
            .when_some(self.device_key.clone(),|d,key|d.child(div().p(px(12.)).rounded(px(8.)).bg(self.raised_color()).text_size(px(12.)).child(format!("{}\n{}",key.algorithm,key.fingerprint))))
            .when(self.device_key.is_none(),|d|d.child("本次连接没有设备密钥。请断开，在连接页选择私钥后重新连接，再验证账户密码。"))
            .when(self.device_key.is_some() && self.device_keys_enabled,|d|d
                .child(self.input("设备名称",&self.key_label,Field::KeyLabel,false,cx))
                .child(self.input("当前账户密码",&self.password,Field::Password,true,cx))
                .child(div().text_size(px(12.)).text_color(rgb(MUTED)).child("两分钟内刚验证过账户密码时可留空；密钥登录后或验证过期时需重新输入。"))
                .child(Self::button("bind-device-key",if self.pending_key_bind.is_some(){"绑定中…"}else{"绑定设备"}).on_click(cx.listener(|this,_,_,cx|this.bind_device_key(cx)))))
            .when(self.pending_key_bind.is_none(),|d|d.child(Self::button("cancel-key-bind","返回聊天").on_click(cx.listener(|this,_,_,cx|this.cancel_key_binding(cx)))))
            .when_some(self.error.clone(),|d,error|d.child(self.error_box(error))))
    }
}
