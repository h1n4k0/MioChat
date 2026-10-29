use super::*;

impl MioChatApp {
    pub(super) fn key_reminder_active(&self) -> bool {
        self.key_reminder_visible
            && self.profile.is_none()
            && !self.center_open
            && !self.appearance_menu
            && !self.mock_menu
            && self.screen == Screen::Chat
            && !self.preferences_open
            && self.feature_panel.is_none()
            && self.room_form.is_none()
    }
    pub(super) fn activate_key_reminder(&mut self, index: usize, cx: &mut Context<Self>) {
        match index {
            0 => {
                if self.pending_send.is_none() {
                    self.run_command(
                        if self.device_key.is_some() {
                            "/key-add"
                        } else {
                            "/key-guide"
                        }
                        .into(),
                        true,
                        cx,
                    );
                    if self.pending_send.is_some() {
                        self.key_reminder_visible = false;
                    }
                }
            }
            1 => self.key_reminder_visible = false,
            2 => self.run_command("/pref key_reminder off".into(), true, cx),
            _ => {}
        }
        cx.notify();
    }
    pub(super) fn activate_preference_cursor(&mut self, cx: &mut Context<Self>) {
        let keys = [
            "dnd",
            "key_reminder",
            "compact",
            "contrast",
            "poke",
            "bell",
            "motion",
            "ascii",
            "welcome",
        ];
        let command = if self.preferences_cursor < keys.len() {
            let key = keys[self.preferences_cursor];
            format!(
                "/pref {key} {}",
                if self.account_preferences.on(key) {
                    "off"
                } else {
                    "on"
                }
            )
        } else {
            format!(
                "/theme {}",
                ["aurora", "sakura", "amber", "mono", "light", "contrast"]
                    [self.preferences_cursor - keys.len()]
            )
        };
        self.run_command(command, true, cx);
    }
    pub(super) fn accept_preferences(&mut self, p: Preferences, cx: &mut Context<Self>) {
        if !p.belongs_to(&self.user) {
            return;
        }
        if !p.alerts() {
            for tag in self.notification_targets.keys() {
                cx.dismiss_system_notification(tag);
            }
            self.notification_targets.clear();
        }
        if !p.reminder() {
            self.key_reminder_visible = false;
        }
        let unblocked = self
            .account_preferences
            .blocked
            .iter()
            .any(|name| !p.blocks(name));
        if self.account_preferences.blocked != p.blocked {
            self.messages.retain(|m| !p.blocks(&m.sender));
            for m in &mut self.messages {
                if p.blocks(&m.reply_name) {
                    m.reply_name.clear();
                    m.reply_body.clear();
                }
            }
            self.message_ids = self.messages.iter().map(|m| m.id).collect();
            self.message_revision = self.message_revision.wrapping_add(1);
            for tag in self.notification_targets.keys() {
                cx.dismiss_system_notification(tag);
            }
            self.notification_targets.clear();
            self.notifications
                .unread
                .retain(|room, _| !p.blocks(room.trim_start_matches("dm:")));
        }
        if self.account_preferences.rooms != p.rooms {
            self.notifications
                .unread
                .retain(|room, _| room.starts_with("dm:"));
            for (name, room) in &p.rooms {
                if room.subscribed && room.unread > 0 && !(self.foreground && self.room == *name) {
                    self.notifications.unread.insert(name.clone(), room.unread);
                }
            }
            self.notification_targets.retain(|tag, (_, room)| {
                let keep = !p.rooms.get(room).is_some_and(|p| p.muted);
                if !keep {
                    cx.dismiss_system_notification(tag);
                }
                keep
            });
        }
        self.account_preferences = p;
        if unblocked
            && self.persistence_enabled
            && self.screen == Screen::Chat
            && !self.room.is_empty()
        {
            let position = self.reading_position();
            if let Err(e) = self.request_view(position, false) {
                self.error = Some(e.to_string());
            }
        }
    }
    pub(super) fn show_preferences(&mut self, cx: &mut Context<Self>) {
        if !self.preferences_enabled
            || self.screen != Screen::Chat
            || self.pending_preferences.is_some()
        {
            return;
        }
        let id = self.next_request_id("preferences");
        if let Err(e) = self.send_request(&Request::simple("preferences", &id)) {
            self.error = Some(e.to_string());
        } else {
            self.begin_feature_navigation();
            self.pending_preferences = Some(id);
            self.center_open = false;
            self.clear_profile();
            self.set_feature_panel(None);
            self.cancel_room_form(cx);
            self.appearance_menu = false;
        }
        cx.notify();
    }
    pub(super) fn preference_border(&self) -> gpui::Hsla {
        rgb(if self.account_preferences.on("contrast") {
            0x222222
        } else {
            BORDER
        })
        .into()
    }
    pub(super) fn preference_text(&self) -> gpui::Hsla {
        rgb(if self.account_preferences.on("contrast") {
            0x000000
        } else {
            TEXT
        })
        .into()
    }
    pub(super) fn preferences_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .id("account-preferences")
            .absolute()
            .top(px(56.))
            .right(px(12.))
            .bottom(px(12.))
            .w(px(390.))
            .p(px(16.))
            .rounded(px(12.))
            .bg(self.raised_color())
            .border_1()
            .border_color(self.preference_border())
            .shadow_lg()
            .flex()
            .flex_col()
            .gap(px(12.))
            .on_click(|_, _, cx| cx.stop_propagation())
            .child(
                div()
                    .flex()
                    .justify_between()
                    .items_center()
                    .child(format!("@{} 的偏好", self.account_preferences.user))
                    .child(
                        Self::button("close-preferences", "关闭").on_click(cx.listener(
                            |this, _, _, cx| {
                                this.preferences_open = false;
                                cx.notify();
                            },
                        )),
                    ),
            )
            .child(
                div()
                    .id("preferences-scroll")
                    .flex_1()
                    .min_h(px(0.))
                    .overflow_y_scroll()
                    .flex()
                    .flex_col()
                    .gap(px(10.))
                    .children(
                        [
                            ("dnd", "勿扰", "关闭桌面消息提醒，仍保留未读。"),
                            (
                                "key_reminder",
                                "设备密钥登录提醒",
                                "无密钥时，在下次密码登录后提醒。",
                            ),
                            ("compact", "紧凑显示", "减小消息气泡和列表间距。"),
                            ("contrast", "高对比度", "使用清晰边框与高对比文字。"),
                            ("poke", "允许戳一戳", "控制其他用户能否向你发送戳一戳。"),
                            ("bell", "终端铃声", "终端在收到私信或提及时响铃。"),
                            ("motion", "终端动效", "终端提示的动态效果。"),
                            ("ascii", "终端 ASCII 显示", "终端界面使用兼容符号。"),
                            ("welcome", "终端欢迎提示", "密码登录后的终端欢迎信息。"),
                        ]
                        .into_iter()
                        .enumerate()
                        .map(|(index, (key, label, help))| {
                            let on = self.account_preferences.on(key);
                            let command = format!("/pref {key} {}", if on { "off" } else { "on" });
                            div()
                                .id(gpui::SharedString::from(format!("preference-{key}")))
                                .p(px(10.))
                                .rounded(px(8.))
                                .bg(self.canvas_color())
                                .when(self.preferences_cursor == index, |d| {
                                    d.border_1().border_color(self.preference_border())
                                })
                                .flex()
                                .items_center()
                                .justify_between()
                                .gap(px(10.))
                                .child(div().flex_1().flex().flex_col().child(label).child(
                                    div().text_size(px(11.)).text_color(rgb(MUTED)).child(help),
                                ))
                                .child(
                                    Self::button("toggle-preference", if on { "开" } else { "关" })
                                        .aria_label(format!(
                                            "{label}: {}",
                                            if on { "开" } else { "关" }
                                        ))
                                        .on_click(cx.listener(move |this, _, _, cx| {
                                            this.preferences_cursor = index;
                                            this.run_command(command.clone(), true, cx)
                                        })),
                                )
                        }),
                    )
                    .child(div().mt(px(10.)).child("终端配色（同步账户）"))
                    .children(
                        ["aurora", "sakura", "amber", "mono", "light", "contrast"]
                            .into_iter()
                            .enumerate()
                            .map(|(index, theme)| {
                                div()
                                    .id(gpui::SharedString::from(format!("account-theme-{theme}")))
                                    .p(px(8.))
                                    .rounded(px(7.))
                                    .cursor_pointer()
                                    .role(gpui::Role::Button)
                                    .aria_label(format!("终端配色 {theme}"))
                                    .bg(self.canvas_color())
                                    .when(self.preferences_cursor == index + 9, |d| {
                                        d.border_1().border_color(self.preference_border())
                                    })
                                    .child(format!(
                                        "{} {theme}",
                                        if self.account_preferences.theme == theme {
                                            "●"
                                        } else {
                                            "○"
                                        }
                                    ))
                                    .on_click(cx.listener(move |this, _, _, cx| {
                                        this.preferences_cursor = index + 9;
                                        this.run_command(format!("/theme {theme}"), true, cx)
                                    }))
                            }),
                    )
                    .child(
                        div()
                            .text_size(px(11.))
                            .text_color(rgb(MUTED))
                            .child("本机默认、毛玻璃、透明外观仍在外观菜单中选择。"),
                    ),
            )
    }
    pub(super) fn key_reminder_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .id("key-reminder")
            .absolute()
            .top(px(70.))
            .right(px(24.))
            .w(px(370.))
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
            .child("为这台设备设置密钥登录")
            .child(div().text_size(px(12.)).child(
                "每台设备可使用独立密钥。私钥留在本机；确认后才会绑定当前 SSH 已验证的公钥。",
            ))
            .child(
                Self::button(
                    "key-reminder-primary",
                    if self.device_key.is_some() {
                        "绑定当前设备"
                    } else {
                        "查看步骤"
                    },
                )
                .when(self.key_reminder_cursor == 0, |d| {
                    d.border_2().border_color(self.preference_border())
                })
                .on_click(cx.listener(|this, _, _, cx| this.activate_key_reminder(0, cx))),
            )
            .child(
                Self::button("key-reminder-later", "稍后再说")
                    .when(self.key_reminder_cursor == 1, |d| {
                        d.border_2().border_color(self.preference_border())
                    })
                    .on_click(cx.listener(|this, _, _, cx| {
                        this.activate_key_reminder(1, cx);
                    })),
            )
            .child(
                Self::button("key-reminder-disable", "不再提醒")
                    .when(self.key_reminder_cursor == 2, |d| {
                        d.border_2().border_color(self.preference_border())
                    })
                    .on_click(cx.listener(|this, _, _, cx| this.activate_key_reminder(2, cx))),
            )
    }
}
