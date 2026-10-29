use super::*;

impl MioChatApp {
    pub(super) fn clear_center(&mut self) {
        self.center = Center::default();
        self.center_open = false;
        self.pending_center = None;
        self.center_cursor = None;
        self.center_bootstrapped = false;
    }
    pub(super) fn load_center(&mut self, cx: &mut Context<Self>) {
        if !self.center_enabled || self.screen != Screen::Chat || self.pending_center.is_some() {
            return;
        }
        let id = self.next_request_id("center");
        if let Err(e) = self.send_request(&Request::simple("control_center", &id)) {
            self.error = Some(e.to_string());
        } else {
            self.pending_center = Some(id);
        }
        cx.notify();
    }
    pub(super) fn open_center(&mut self, section: &str, cx: &mut Context<Self>) {
        if self.composition_blocks_navigation(cx) {
            return;
        }
        if !self.center_enabled || self.screen != Screen::Chat {
            return;
        }
        self.begin_feature_navigation();
        self.center_open = true;
        self.clear_profile();
        self.center_section = section.into();
        self.center_cursor = None;
        self.preferences_open = false;
        self.set_feature_panel(None);
        self.cancel_room_form(cx);
        self.appearance_menu = false;
        self.load_center(cx);
        cx.notify();
    }
    pub(super) fn accept_center(&mut self, p: Center, show: bool, cx: &mut Context<Self>) {
        if !p.belongs_to(&self.user) {
            return;
        }
        if show {
            self.center_section = p.section.clone();
            self.center_open = true;
            self.clear_profile();
            self.preferences_open = false;
            self.set_feature_panel(None);
            self.cancel_room_form(cx);
        }
        self.center = p;
        self.center_cursor = None;
    }
    pub(super) fn activate_center_command(&mut self, command: String, cx: &mut Context<Self>) {
        if let Some(name) = command.strip_prefix("/profile ") {
            self.open_profile(name.to_string(), cx);
            return;
        }
        if let Some(peer) = command.strip_prefix("/dm ") {
            self.select_room(format!("dm:{peer}"), cx);
            if self.pending_room.is_some() || self.queued_room.is_some() {
                self.center_open = false;
            }
            return;
        }
        if command.starts_with("/jump ") && self.persistence_enabled {
            self.observe_persistence();
            if self.persistence.dirty() {
                self.status = "正在保存草稿，请稍后再打开消息。".into();
                cx.notify();
                return;
            }
        }
        self.run_command(command.clone(), true, cx);
        if command.starts_with("/jump ") && self.pending_send.is_some() {
            self.center_open = false;
        }
        cx.notify();
    }
    pub(super) fn center_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let notices = self.center_section == "notifications";
        let selected = self
            .center_cursor
            .and_then(|i| self.center.actions(&self.center_section).get(i).cloned());
        div()
            .id("control-center")
            .absolute()
            .top(px(54.))
            .right(px(12.))
            .bottom(px(12.))
            .w(px(400.))
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
                    .child(format!("@{} 的控制中心", self.user))
                    .child(Self::button("close-center", "关闭").on_click(cx.listener(
                        |this, _, _, cx| {
                            this.center_open = false;
                            cx.notify();
                        },
                    ))),
            )
            .child(
                div()
                    .flex()
                    .gap(px(8.))
                    .child(
                        Self::button("center-contacts", "联系人").on_click(cx.listener(
                            |this, _, _, cx| {
                                this.center_section = "contacts".into();
                                this.center_cursor = None;
                                cx.notify();
                            },
                        )),
                    )
                    .child(
                        Self::button(
                            "center-notifications",
                            format!("通知 {}", self.center.unread),
                        )
                        .on_click(cx.listener(|this, _, _, cx| {
                            this.center_section = "notifications".into();
                            this.center_cursor = None;
                            cx.notify();
                        })),
                    )
                    .child(
                        Self::button("refresh-center", "刷新")
                            .on_click(cx.listener(|this, _, _, cx| this.load_center(cx))),
                    ),
            )
            .when(notices, |d| {
                d.child(
                    Self::button("mark-center-read", "全部标为已读").on_click(cx.listener(
                        |this, _, _, cx| this.activate_center_command("/notifyclear".into(), cx),
                    )),
                )
            })
            .child(
                div()
                    .id("center-scroll")
                    .flex_1()
                    .min_h(px(0.))
                    .overflow_y_scroll()
                    .flex()
                    .flex_col()
                    .gap(px(10.))
                    .when(!notices, |d| {
                        d.children(
                            self.center
                                .contacts_for(&self.center_section)
                                .into_iter()
                                .map(|contact| {
                                    let name = contact.name.clone();
                                    let other = contact.name.clone();
                                    let group = match contact.group.as_str() {
                                        "requests" => "好友申请",
                                        "friends" => "好友",
                                        "recent" => "最近私信",
                                        _ => "关注",
                                    };
                                    div()
                                        .id(gpui::SharedString::from(format!(
                                            "center-{}-{}",
                                            contact.group, contact.name
                                        )))
                                        .p(px(12.))
                                        .rounded(px(10.))
                                        .bg(self.canvas_color())
                                        .border_1()
                                        .border_color(self.preference_border())
                                        .flex()
                                        .items_center()
                                        .justify_between()
                                        .gap(px(8.))
                                        .child(
                                            div()
                                                .flex_1()
                                                .flex()
                                                .flex_col()
                                                .child(format!("@{}", contact.name))
                                                .child(
                                                    div()
                                                        .text_size(px(11.))
                                                        .text_color(rgb(MUTED))
                                                        .child(group),
                                                ),
                                        )
                                        .when(contact.group == "requests", |d| {
                                            d.child(
                                                Self::button("accept-request", "接受")
                                                    .when(
                                                        selected.as_deref()
                                                            == Some(
                                                                format!("/accept {name}").as_str(),
                                                            ),
                                                        |d| {
                                                            d.border_2().border_color(
                                                                self.preference_border(),
                                                            )
                                                        },
                                                    )
                                                    .on_click(cx.listener(
                                                        move |this, _, _, cx| {
                                                            this.activate_center_command(
                                                                format!("/accept {name}"),
                                                                cx,
                                                            )
                                                        },
                                                    )),
                                            )
                                            .child(
                                                Self::button("decline-request", "拒绝")
                                                    .when(
                                                        selected.as_deref()
                                                            == Some(
                                                                format!("/decline {other}")
                                                                    .as_str(),
                                                            ),
                                                        |d| {
                                                            d.border_2().border_color(
                                                                self.preference_border(),
                                                            )
                                                        },
                                                    )
                                                    .on_click(cx.listener(
                                                        move |this, _, _, cx| {
                                                            this.activate_center_command(
                                                                format!("/decline {other}"),
                                                                cx,
                                                            )
                                                        },
                                                    )),
                                            )
                                        })
                                        .when(contact.group != "requests", |d| {
                                            d.child(
                                                Self::button("contact-dm", "资料")
                                                    .when(
                                                        selected.as_deref()
                                                            == Some(
                                                                format!(
                                                                    "/profile {}",
                                                                    contact.name
                                                                )
                                                                .as_str(),
                                                            ),
                                                        |d| {
                                                            d.border_2().border_color(
                                                                self.preference_border(),
                                                            )
                                                        },
                                                    )
                                                    .on_click(cx.listener(
                                                        move |this, _, _, cx| {
                                                            this.activate_center_command(
                                                                format!(
                                                                    "/profile {}",
                                                                    contact.name
                                                                ),
                                                                cx,
                                                            )
                                                        },
                                                    )),
                                            )
                                        })
                                }),
                        )
                    })
                    .when(notices, |d| {
                        d.children(self.center.notices.iter().cloned().map(|n| {
                            let command = n.command.clone();
                            div()
                                .id(gpui::SharedString::from(format!("center-notice-{}", n.id)))
                                .p(px(12.))
                                .rounded(px(10.))
                                .bg(self.canvas_color())
                                .border_1()
                                .border_color(self.preference_border())
                                .flex()
                                .flex_col()
                                .gap(px(6.))
                                .child(
                                    div()
                                        .flex()
                                        .justify_between()
                                        .child(format!("{} · @{}", n.kind, n.actor))
                                        .child(if n.seen { "已读" } else { "未读" }),
                                )
                                .child(n.body)
                                .child(div().text_size(px(11.)).text_color(rgb(MUTED)).child(
                                    format!(
                                        "{:02}:{:02} UTC",
                                        (n.at / 3600) % 24,
                                        (n.at / 60) % 60
                                    ),
                                ))
                                .when(!command.is_empty(), |d| {
                                    d.child(
                                        Self::button("open-notice", "查看")
                                            .when(
                                                selected.as_deref() == Some(command.as_str()),
                                                |d| {
                                                    d.border_2()
                                                        .border_color(self.preference_border())
                                                },
                                            )
                                            .on_click(cx.listener(move |this, _, _, cx| {
                                                this.activate_center_command(command.clone(), cx)
                                            })),
                                    )
                                })
                        }))
                    })
                    .when(
                        (notices && self.center.notices.is_empty())
                            || (!notices
                                && self.center.contacts_for(&self.center_section).is_empty()),
                        |d| {
                            d.child(
                                div()
                                    .p(px(20.))
                                    .text_color(rgb(MUTED))
                                    .child("暂时没有记录"),
                            )
                        },
                    ),
            )
    }
}
