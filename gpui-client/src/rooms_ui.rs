use super::*;

impl MioChatApp {
    pub(super) fn open_room_form(&mut self, action: RoomAction, cx: &mut Context<Self>) {
        if self.screen != Screen::Chat || self.room.starts_with("dm:") || !self.native_actions {
            return;
        }
        let value = self
            .feature_panel
            .as_ref()
            .and_then(|p| p.details.as_ref())
            .map(|r| match &action {
                RoomAction::Setting(key) => match key.as_str() {
                    "category" => r.category.clone(),
                    "rules" => r.rules.clone(),
                    "announcement" => r.announcement.clone(),
                    "slow" => r.slow.to_string(),
                    _ => String::new(),
                },
                _ => String::new(),
            })
            .unwrap_or_default();
        self.begin_feature_navigation();
        self.room_form = Some(RoomIntent {
            room: self.room.clone(),
            action,
        });
        self.room_target = value;
        self.set_feature_panel(None);
        self.field = Field::RoomTarget;
        self.reset_selection();
        cx.notify();
    }
    pub(super) fn cancel_room_form(&mut self, cx: &mut Context<Self>) {
        self.room_form = None;
        self.pending_room_form = None;
        self.room_target.clear();
        self.reset_selection();
        cx.notify();
    }
    pub(super) fn submit_room_form(&mut self, cx: &mut Context<Self>) {
        let Some(command) = self
            .room_form
            .as_ref()
            .and_then(|f| f.command(&self.room, &self.room_target))
        else {
            self.error = Some("请填写一个用户名或 UID；房间改变后请重新打开操作。".into());
            cx.notify();
            return;
        };
        self.observe_persistence();
        if self.persistence_enabled && self.persistence.dirty() {
            self.status = "正在保存当前草稿，请稍后再次确认。".into();
            cx.notify();
            return;
        }
        if self.pending_send.is_some() {
            return;
        }
        self.run_command(command, true, cx);
        if let Some(pending) = self.pending_send.as_ref() {
            self.pending_room_form = Some(pending.request_id.clone());
        }
    }
    pub(super) fn room_content(&self, panel: &Panel, cx: &mut Context<Self>) -> Div {
        let mut content = div().flex().flex_col().gap(px(10.));
        if !panel.scope.is_empty() && panel.scope != self.room {
            return content;
        }
        if let Some(room) = &panel.details {
            content = content.child(self.room_details(room, true));
        }
        content
            .children(panel.rooms.iter().filter_map(|room| {
                let original =
                    self.feature_action_original(panel, &format!("/join {}", room.name))?;
                let revision = self.feature_revision;
                Some(
                    div()
                        .id(gpui::SharedString::from(format!("directory-{}", room.name)))
                        .role(gpui::Role::Button)
                        .aria_label(format!("打开房间 {}", room.name))
                        .anchor_scroll(
                            self.feature_anchor_for(&feature_menu::Item::Action(original)),
                        )
                        .focusable()
                        .p(px(12.))
                        .rounded(px(10.))
                        .border_1()
                        .border_color(self.preference_border())
                        .when(self.feature_original_selected(original), |d| {
                            d.border_color(rgb(0x899bb0))
                        })
                        .cursor_pointer()
                        .bg(self.canvas_color())
                        .hover(|s| s.bg(rgb(0xe9e6df)))
                        .child(self.room_details(room, false))
                        .on_click(cx.listener(move |this, _, window, cx| {
                            this.activate_visible_feature(revision, original, window, cx)
                        })),
                )
            }))
            .children(panel.members.iter().cloned().map(|member| {
                let revision = self.feature_revision;
                let remove = self
                    .feature_action_original(panel, &format!("/remove {}", member.name))
                    .filter(|_| member.can_remove);
                let grant = self
                    .feature_member_item(panel, &member.name, true)
                    .filter(|_| member.can_mod);
                let revoke = self
                    .feature_member_item(panel, &member.name, false)
                    .filter(|_| member.can_unmod);
                div()
                    .id(gpui::SharedString::from(format!("member-{}", member.name)))
                    .p(px(10.))
                    .rounded(px(8.))
                    .bg(self.canvas_color())
                    .flex()
                    .items_center()
                    .justify_between()
                    .gap(px(8.))
                    .child(
                        div()
                            .flex()
                            .flex_col()
                            .child(format!("@{}", member.name))
                            .child(
                                div()
                                    .text_size(px(11.))
                                    .text_color(rgb(MUTED))
                                    .child(member.role),
                            ),
                    )
                    .when_some(remove, |d, original| {
                        d.child(
                            Self::button("remove-room-member", "移除")
                                .anchor_scroll(
                                    self.feature_anchor_for(&feature_menu::Item::Action(original)),
                                )
                                .when(self.feature_original_selected(original), |d| {
                                    d.border_1().border_color(rgb(0x899bb0))
                                })
                                .on_click(cx.listener(move |this, _, window, cx| {
                                    this.activate_visible_feature(revision, original, window, cx)
                                })),
                        )
                    })
                    .when_some(grant, |d, item| {
                        d.child(
                            Self::button("grant-room-mod", "设为管理员")
                                .anchor_scroll(self.feature_anchor_for(&item))
                                .when(self.feature_item_selected(&item), |d| {
                                    d.border_1().border_color(rgb(0x899bb0))
                                })
                                .on_click(cx.listener(move |this, _, window, cx| {
                                    this.activate_feature_item(revision, item.clone(), window, cx)
                                })),
                        )
                    })
                    .when_some(revoke, |d, item| {
                        d.child(
                            Self::button("revoke-room-mod", "撤销管理员")
                                .anchor_scroll(self.feature_anchor_for(&item))
                                .when(self.feature_item_selected(&item), |d| {
                                    d.border_1().border_color(rgb(0x899bb0))
                                })
                                .on_click(cx.listener(move |this, _, window, cx| {
                                    this.activate_feature_item(revision, item.clone(), window, cx)
                                })),
                        )
                    })
            }))
    }

    fn room_details(&self, room: &miochat_gpui::session::RoomCard, expanded: bool) -> Div {
        let flags = [
            if room.private { "私密" } else { "公开" },
            if room.archived { "已归档" } else { "活跃" },
            if room.read_only {
                "只读"
            } else {
                "可发言"
            },
        ]
        .join(" · ");
        div()
            .flex()
            .flex_col()
            .gap(px(5.))
            .child(
                div()
                    .font_weight(gpui::FontWeight::MEDIUM)
                    .child(format!("#{}", room.name)),
            )
            .child(div().text_size(px(11.)).text_color(rgb(MUTED)).child(flags))
            .child(div().text_size(px(13.)).child(room.topic.clone()))
            .child(
                div()
                    .text_size(px(11.))
                    .text_color(rgb(MUTED))
                    .child(format!("房主 @{} · {}", room.owner, room.category)),
            )
            .when(expanded, |d| {
                d.child(format!("规则：{}", room.rules))
                    .child(format!("公告：{}", room.announcement))
                    .child(format!("慢速间隔：{} 秒", room.slow))
                    .child(div().text_size(px(11.)).text_color(rgb(MUTED)).child(
                        if !room.can_leave {
                            "房主须先转让房间，再离开。"
                        } else if room.can_manage {
                            "你可以管理成员。"
                        } else {
                            "你可以查看成员。"
                        },
                    ))
            })
    }
    pub(super) fn room_form_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let form = self.room_form.as_ref().expect("room form is visible");
        let (title, explanation) = match &form.action {
            RoomAction::Invite => (
                "邀请成员".to_string(),
                "受邀用户会成为此房间的成员。".to_string(),
            ),
            RoomAction::Remove(name) => (
                format!("移除 @{name}？"),
                "该用户在此房间的所有会话会返回大厅；私密房间访问权将被撤销。".to_string(),
            ),
            RoomAction::Setting(key) => (
                format!("编辑房间 {key}"),
                if key == "transfer" {
                    "转让后原房主成为普通成员；请输入新房主名称或 #UID。"
                } else {
                    "设置仅应用于此房间。规则/公告/分类最多240字，慢速0–3600秒。"
                }
                .into(),
            ),
            RoomAction::Toggle { key, on } => (
                format!("确认 {key} {}", if *on { "开启" } else { "关闭" }),
                "此操作会立即影响房间访问或发送权限。".into(),
            ),
            RoomAction::Moderator { name, grant } => (
                format!(
                    "{} @{name}",
                    if *grant {
                        "设为房间管理员"
                    } else {
                        "撤销房间管理员"
                    }
                ),
                "权限以提交时服务端校验为准。".into(),
            ),
            RoomAction::Leave => (
                "离开房间？".to_string(),
                "你将返回大厅。离开私密房间后，需要重新受邀才能进入。".to_string(),
            ),
        };
        div()
            .id("room-action-form")
            .absolute()
            .top(px(70.))
            .right(px(24.))
            .w(px(370.))
            .p(px(20.))
            .rounded(px(12.))
            .bg(self.raised_color())
            .border_1()
            .border_color(self.preference_border())
            .shadow_lg()
            .flex()
            .flex_col()
            .gap(px(14.))
            .on_click(|_, _, cx| cx.stop_propagation())
            .child(div().font_weight(gpui::FontWeight::SEMIBOLD).child(title))
            .child(format!("#{}", form.room))
            .child(
                div()
                    .text_size(px(12.))
                    .text_color(rgb(MUTED))
                    .child(explanation),
            )
            .when(form.room != self.room, |d| {
                d.child("房间已变化；未提交内容已保留，请取消后返回原房间重新核对。")
            })
            .when(form.action.needs_input(), |d| {
                d.child(self.input(
                    if matches!(form.action, RoomAction::Setting(_)) {
                        "设置值"
                    } else {
                        "用户名或 UID"
                    },
                    &self.room_target,
                    Field::RoomTarget,
                    false,
                    cx,
                ))
            })
            .child(
                div()
                    .flex()
                    .gap(px(10.))
                    .child(
                        Self::button("cancel-room-action", "取消")
                            .on_click(cx.listener(|this, _, _, cx| this.cancel_room_form(cx))),
                    )
                    .child(
                        Self::button("confirm-room-action", "确认")
                            .on_click(cx.listener(|this, _, _, cx| this.submit_room_form(cx))),
                    ),
            )
            .when_some(self.error.clone(), |d, e| d.child(self.error_box(e)))
    }
}
