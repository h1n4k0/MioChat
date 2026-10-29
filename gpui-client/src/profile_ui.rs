use super::*;
impl MioChatApp {
    pub(super) fn clear_profile(&mut self) {
        self.profile = None;
        self.pending_profile = None;
        self.profile_editor = None;
        self.profile_text.clear();
    }
    pub(super) fn open_profile(&mut self, target: String, cx: &mut Context<Self>) {
        if self.composition_blocks_navigation(cx) {
            return;
        }
        if !self.profile_enabled || self.screen != Screen::Chat || self.pending_profile.is_some() {
            return;
        }
        if self.profile_editor.is_some() {
            self.error = Some("请先保存或取消当前编辑。".into());
            cx.notify();
            return;
        }
        let id = self.next_request_id("profile");
        let mut request = Request::simple("profile", &id);
        request.body = &target;
        if let Err(e) = self.send_request(&request) {
            self.error = Some(e.to_string());
        } else {
            self.begin_feature_navigation();
            self.pending_profile = Some(id);
            self.profile = None;
            self.center_open = false;
            self.preferences_open = false;
            self.set_feature_panel(None);
            self.cancel_room_form(cx);
        }
        cx.notify();
    }
    pub(super) fn accept_profile(
        &mut self,
        p: Profile,
        show: bool,
        saved: bool,
        cx: &mut Context<Self>,
    ) {
        if p.viewer != self.user || self.user.is_empty() {
            return;
        }
        if !show && self.profile.as_ref().is_none_or(|old| old.name != p.name) {
            return;
        }
        if saved {
            self.profile_editor = None;
            self.profile_text.clear();
            self.reset_selection();
        }
        if show {
            self.center_open = false;
            self.preferences_open = false;
            self.set_feature_panel(None);
            self.cancel_room_form(cx);
        }
        self.profile = Some(p);
    }
    pub(super) fn begin_profile_edit(&mut self, kind: EditKind, cx: &mut Context<Self>) {
        let Some(p) = self.profile.as_ref() else {
            return;
        };
        let own = p.own && p.name == self.user;
        if (kind == EditKind::Bio || kind == EditKind::Status || kind == EditKind::Afk) && !own {
            return;
        }
        self.profile_text = match kind {
            EditKind::Bio => p.bio.clone(),
            EditKind::Status => p.status.clone(),
            EditKind::Note => p.note.clone(),
            EditKind::Afk => p.status.strip_prefix("AFK · ").unwrap_or("").to_string(),
        };
        self.profile_editor = Some(ProfileEdit {
            owner: self.user.clone(),
            target: p.name.clone(),
            kind,
        });
        self.begin_feature_navigation();
        self.field = Field::ProfileText;
        self.reset_selection();
        cx.notify();
    }
    pub(super) fn submit_profile_edit(&mut self, cx: &mut Context<Self>) {
        if let Some(command) = self
            .profile_editor
            .as_ref()
            .and_then(|e| e.command(&self.user, &self.profile_text))
        {
            self.run_command(command, true, cx);
        }
        cx.notify();
    }
    pub(super) fn profile_command(&mut self, command: String, cx: &mut Context<Self>) {
        if command.starts_with("/dm ") {
            self.activate_center_command(command, cx);
            if self.pending_room.is_some() || self.queued_room.is_some() {
                self.clear_profile();
            }
            return;
        }
        self.run_command(command.clone(), true, cx);
        if matches!(
            command.as_str(),
            "/keys" | "/passwd" | "/sessions" | "/logoutothers"
        ) && self.pending_send.is_some()
        {
            self.clear_profile();
        }
    }
    pub(super) fn profile_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let p = self.profile.as_ref().expect("visible profile");
        let own = p.own && p.name == self.user;
        let name = p.name.clone();
        let follow = p.name.clone();
        let friend = p.name.clone();
        let block = p.name.clone();
        let unnote = p.name.clone();
        div()
            .id("profile-card")
            .absolute()
            .top(px(54.))
            .right(px(12.))
            .bottom(px(12.))
            .w(px(410.))
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
            .child(
                div()
                    .flex()
                    .justify_between()
                    .items_center()
                    .child(format!("@{} · UID #{}", p.name, p.uid))
                    .child(Self::button("close-profile", "关闭").on_click(cx.listener(
                        |this, _, _, cx| {
                            this.clear_profile();
                            this.reset_selection();
                            cx.notify();
                        },
                    ))),
            )
            .child(
                div()
                    .id("profile-scroll")
                    .flex_1()
                    .min_h(px(0.))
                    .overflow_y_scroll()
                    .flex()
                    .flex_col()
                    .gap(px(12.))
                    .child(
                        div()
                            .p(px(12.))
                            .rounded(px(10.))
                            .bg(self.canvas_color())
                            .flex()
                            .flex_col()
                            .gap(px(5.))
                            .child(
                                div()
                                    .font_family("Consolas")
                                    .text_size(px(11.))
                                    .child(p.avatar.clone()),
                            )
                            .child(format!(
                                "{} · {}",
                                p.role,
                                if p.online { "在线" } else { "离线" }
                            ))
                            .child(format!("注册：{}", p.created))
                            .child(format!("最近登录：{}", p.last_login))
                            .child(format!("状态：{}", p.status))
                            .child(format!("简介：{}", p.bio))
                            .child(div().flex().flex_wrap().gap(px(6.)).children(
                                p.badges.iter().map(|badge| {
                                    div()
                                        .px(px(8.))
                                        .py(px(4.))
                                        .rounded(px(8.))
                                        .bg(self.raised_color())
                                        .child(badge.clone())
                                }),
                            ))
                            .when(p.badges.is_empty(), |d| d.child("尚无活动成就")),
                    )
                    .when(self.profile_editor.is_none(), |d| {
                        d.when(own, |d| {
                            d.when(self.presence_enabled, |d| {
                                d.child(
                                    div().child("头像").flex().flex_wrap().gap(px(6.)).children(
                                        [
                                            ("camp", "营地"),
                                            ("cat", "猫"),
                                            ("robot", "机器人"),
                                            ("planet", "行星"),
                                            ("flower", "花"),
                                            ("ident", "标识"),
                                        ]
                                        .into_iter()
                                        .map(
                                            |(key, label)| {
                                                Self::button(
                                                    key,
                                                    format!(
                                                        "{}{}",
                                                        if p.avatar_kind == key {
                                                            "✓ "
                                                        } else {
                                                            ""
                                                        },
                                                        label
                                                    ),
                                                )
                                                .on_click(cx.listener(move |this, _, _, cx| {
                                                    this.profile_command(
                                                        format!("/avatar {key}"),
                                                        cx,
                                                    )
                                                }))
                                            },
                                        ),
                                    ),
                                )
                                .child(Self::button("set-afk", "暂时离开").on_click(cx.listener(
                                    |this, _, _, cx| this.begin_profile_edit(EditKind::Afk, cx),
                                )))
                                .child(
                                    Self::button("clear-afk", "返回大厅并清除状态").on_click(
                                        cx.listener(|this, _, _, cx| {
                                            this.profile_command("/back".into(), cx)
                                        }),
                                    ),
                                )
                            })
                            .child(Self::button("edit-bio", "编辑简介").on_click(cx.listener(
                                |this, _, _, cx| this.begin_profile_edit(EditKind::Bio, cx),
                            )))
                            .child(
                                Self::button("edit-status", "编辑状态").on_click(cx.listener(
                                    |this, _, _, cx| this.begin_profile_edit(EditKind::Status, cx),
                                )),
                            )
                            .children(
                                [
                                    ("my-keys", "设备密钥", "/keys"),
                                    ("my-password", "修改密码", "/passwd"),
                                    ("my-sessions", "我的会话", "/sessions"),
                                    ("logout-others", "退出其他设备", "/logoutothers"),
                                ]
                                .into_iter()
                                .map(|(id, label, command)| {
                                    Self::button(id, label).on_click(cx.listener(
                                        move |this, _, _, cx| {
                                            this.profile_command(command.into(), cx)
                                        },
                                    ))
                                }),
                            )
                        })
                        .when(!own, |d| {
                            let mention = p.name.clone();
                            let poke = p.name.clone();
                            d.when(self.presence_enabled, |d| {
                                d.child(Self::button("profile-mention", "@ 提及").on_click(
                                    cx.listener(move |this, _, _, cx| {
                                        this.profile_command(format!("/mention {mention}"), cx)
                                    }),
                                ))
                                .child(
                                    Self::button("profile-poke", "戳一戳").on_click(cx.listener(
                                        move |this, _, _, cx| {
                                            this.profile_command(format!("/poke {poke}"), cx)
                                        },
                                    )),
                                )
                            })
                            .child(Self::button("profile-dm", "私信").on_click(cx.listener(
                                move |this, _, _, cx| {
                                    this.profile_command(format!("/dm {name}"), cx)
                                },
                            )))
                            .child(
                                Self::button(
                                    "profile-follow",
                                    if p.following {
                                        "取消关注"
                                    } else {
                                        "关注"
                                    },
                                )
                                .on_click(cx.listener(
                                    move |this, _, _, cx| {
                                        this.profile_command(
                                            format!(
                                                "{} {follow}",
                                                if this
                                                    .profile
                                                    .as_ref()
                                                    .is_some_and(|p| p.following)
                                                {
                                                    "/unfollow"
                                                } else {
                                                    "/follow"
                                                }
                                            ),
                                            cx,
                                        )
                                    },
                                )),
                            )
                            .when(!p.friend && !p.requested, |d| {
                                d.child(Self::button("profile-friend", "申请好友").on_click(
                                    cx.listener(move |this, _, _, cx| {
                                        this.profile_command(format!("/friend {friend}"), cx)
                                    }),
                                ))
                            })
                            .when(p.friend, |d| {
                                let target = p.name.clone();
                                d.child(Self::button("profile-unfriend", "解除好友").on_click(
                                    cx.listener(move |this, _, _, cx| {
                                        this.profile_command(format!("/unfriend {target}"), cx)
                                    }),
                                ))
                            })
                            .when(p.requested, |d| d.child("好友申请已发送"))
                            .when(p.can_admin, |d| {
                                d.children(
                                    [
                                        ("profile-ip", "登录 IP", format!("/ip {}", p.name)),
                                        (
                                            "profile-sessions",
                                            "连接会话",
                                            format!("/sessions {}", p.name),
                                        ),
                                        (
                                            "profile-mute",
                                            "禁言10分钟（确认）",
                                            format!("/mute {} 10m", p.name),
                                        ),
                                        (
                                            "profile-ban",
                                            "封号1天（确认）",
                                            format!("/ban {} 1d", p.name),
                                        ),
                                    ]
                                    .into_iter()
                                    .map(
                                        |(id, label, command)| {
                                            Self::button(id, label).on_click(cx.listener(
                                                move |this, _, window, cx| {
                                                    if this.profile_editor.is_some() {
                                                        return;
                                                    }
                                                    let action = PanelAction {
                                                        label: label.into(),
                                                        command: command.clone(),
                                                        fill: false,
                                                    };
                                                    this.clear_profile();
                                                    this.activate_feature_action(
                                                        &action, window, cx,
                                                    );
                                                },
                                            ))
                                        },
                                    ),
                                )
                            })
                            .child(
                                Self::button(
                                    "profile-block",
                                    if p.blocked { "解除屏蔽" } else { "屏蔽" },
                                )
                                .on_click(cx.listener(
                                    move |this, _, _, cx| {
                                        this.profile_command(
                                            format!(
                                                "{} {block}",
                                                if this.profile.as_ref().is_some_and(|p| p.blocked)
                                                {
                                                    "/unignore"
                                                } else {
                                                    "/ignore"
                                                }
                                            ),
                                            cx,
                                        )
                                    },
                                )),
                            )
                            .child(
                                div()
                                    .p(px(10.))
                                    .rounded(px(8.))
                                    .bg(self.canvas_color())
                                    .child(format!("仅自己可见的备注：{}", p.note)),
                            )
                            .child(Self::button("edit-private-note", "编辑私有备注").on_click(
                                cx.listener(|this, _, _, cx| {
                                    this.begin_profile_edit(EditKind::Note, cx)
                                }),
                            ))
                            .when(!p.note.is_empty(), |d| {
                                d.child(Self::button("clear-private-note", "删除备注").on_click(
                                    cx.listener(move |this, _, _, cx| {
                                        this.profile_command(format!("/unnote {unnote}"), cx)
                                    }),
                                ))
                            })
                        })
                    })
                    .when_some(self.profile_editor.as_ref(), |d, e| {
                        d.child(self.input(
                            match e.kind {
                                EditKind::Bio => "本人简介",
                                EditKind::Status => "本人状态",
                                EditKind::Note => "仅自己可见的备注",
                                EditKind::Afk => "离开原因（可留空）",
                            },
                            &self.profile_text,
                            Field::ProfileText,
                            false,
                            cx,
                        ))
                        .child(
                            Self::button("save-profile-field", "保存").on_click(
                                cx.listener(|this, _, _, cx| this.submit_profile_edit(cx)),
                            ),
                        )
                        .child(
                            Self::button("cancel-profile-field", "取消").on_click(cx.listener(
                                |this, _, _, cx| {
                                    this.profile_editor = None;
                                    this.profile_text.clear();
                                    this.reset_selection();
                                    cx.notify();
                                },
                            )),
                        )
                    })
                    .when_some(self.error.clone(), |d, e| d.child(self.error_box(e))),
            )
    }
}
