use super::*;
use miochat_gpui::direct_message::DirectMessageForm;

impl MioChatApp {
    pub(super) fn set_screen(&mut self, screen: Screen) {
        self.attachment_ui.screen_changed(screen);
        self.custom_emoji.freeze(screen != Screen::Chat);
        self.dot_ui.clear();
        self.cancel_completion();
        self.reading_move = None;
        self.reset_emoji();
        let chat = screen == Screen::Chat;
        if !chat {
            self.input_history.clear();
            self.dismiss_feature_panel();
            self.discard_secret_admin();
        }
        discard_chat_action_state(screen, &mut self.direct_form, &mut self.pending_send);
        if !chat {
            super::maintenance_ui::discard_for_credentials(
                &mut self.maintenance,
                &mut self.maintenance_notice,
            );
            self.maintenance_cursor = 0;
            if let Some(e) = self.composition.as_mut() {
                e.interrupt();
                self.composition_layout = None;
                self.composition_drag = None;
            }
            self.composition_after = None;
            discard_word_input(&mut self.word_start, &mut self.field);
            self.quarantine_confirmation = None;
            // No transient chat editor or old action is active on credential screens.
            self.pending_send = None;
            self.room_form = None;
            self.pending_room_form = None;
            self.room_target.clear();
            self.clear_profile();
            self.set_feature_panel(None);
            self.center_open = false;
            self.preferences_open = false;
            self.appearance_menu = false;
            self.mock_menu = false;
        }
        self.screen = screen;
    }
    pub(super) fn direct_input_active(&self) -> bool {
        self.secret_form_context_allowed()
            && routes_direct_input(
                self.screen,
                self.field,
                self.direct_form.as_ref(),
                &self.user,
                self.generation,
            )
    }
    pub(super) fn open_simple_form(
        &mut self,
        action: miochat_gpui::simple_actions::SimpleAction,
        cx: &mut Context<Self>,
    ) {
        if self.direct_form.is_some() {
            return;
        }
        self.open_direct_form(cx);
        if let Some(form) = self.direct_form.as_mut() {
            form.purpose = miochat_gpui::direct_message::FormPurpose::Simple(action);
            if action == miochat_gpui::simple_actions::SimpleAction::Roll {
                form.body = "6".into();
            }
            self.field = Field::DirectBody;
            self.reset_selection();
        }
    }
    pub(super) fn open_direct_form(&mut self, cx: &mut Context<Self>) {
        if self.screen != Screen::Chat || !self.native_actions || self.pending_send.is_some() {
            return;
        }
        if self.direct_form.is_none() {
            self.direct_form = Some(DirectMessageForm::new(
                self.user.clone(),
                self.room.clone(),
                self.generation,
            ));
        }
        self.begin_feature_navigation();
        self.clear_profile();
        self.cancel_room_form(cx);
        self.center_open = false;
        self.preferences_open = false;
        self.appearance_menu = false;
        self.mock_menu = false;
        self.message_selection = None;
        self.set_feature_panel(None);
        self.field = Field::DirectTarget;
        self.reset_selection();
        cx.notify();
    }
    pub(super) fn cancel_direct_form(&mut self, cx: &mut Context<Self>) {
        if self
            .direct_form
            .as_ref()
            .is_some_and(|f| f.secret_admin_kind().is_some())
        {
            self.discard_secret_admin();
            self.direct_form = None;
            self.reset_selection();
            cx.notify();
            return;
        }
        if self.direct_form.as_ref().is_some_and(|f| f.pending()) {
            self.error = Some("请求已发出，不能撤回；请等待结果，避免重复发送。".into());
        } else {
            self.direct_form = None;
            self.reset_selection();
        }
        cx.notify();
    }
    pub(super) fn submit_direct_form(&mut self, cx: &mut Context<Self>) {
        if self
            .direct_form
            .as_ref()
            .is_some_and(|f| f.secret_admin_kind().is_some())
        {
            self.submit_secret_admin(cx);
            return;
        }
        if !self.direct_input_active() || self.marked_range.is_some() {
            return;
        }
        let Some(form) = self.direct_form.as_ref() else {
            return;
        };
        let command = match form.command(&self.user, &self.room, self.generation) {
            Ok(command) => command,
            Err(error) => {
                self.error = Some(error.into());
                cx.notify();
                return;
            }
        };
        if self.pending_send.is_some() {
            return;
        }
        self.run_command(command, true, cx);
        if let Some(pending) = self.pending_send.as_ref() {
            self.direct_form
                .as_mut()
                .unwrap()
                .begin(pending.request_id.clone());
        }
        cx.notify();
    }
    pub(super) fn finish_direct_form(&mut self, id: &str, success: bool) {
        if self.screen != Screen::Chat {
            self.direct_form = None;
            return;
        }
        if self
            .direct_form
            .as_mut()
            .is_some_and(|f| f.finish(id, &self.user, self.generation, success))
        {
            self.direct_form = None;
            self.reset_selection();
        }
    }
    pub(super) fn direct_form_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let form = self.direct_form.as_ref().expect("visible one-off DM form");
        let panel = div()
            .id("one-off-dm")
            .absolute()
            .top(px(70.))
            .right(px(24.))
            .bottom(px(24.))
            .overflow_y_scroll()
            .w(px(420.))
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
            .child(
                div()
                    .font_weight(gpui::FontWeight::SEMIBOLD)
                    .child(form.title()),
            )
            .when(matches!(form.purpose,miochat_gpui::direct_message::FormPurpose::EditMessage{..}),|d| {
                if let miochat_gpui::direct_message::FormPurpose::EditMessage{id}=form.purpose {d.child(format!("固定消息 #{id} · 原对话 {}",form.room))} else {d}
            })
            .child(div().text_size(px(12.)).text_color(rgb(MUTED)).child(
                "仅在确认后提交到下方原目标；聊天草稿保留。临时输入不保存，断线或进入凭据页会清除。结果不明时先刷新核对，勿重复提交。",
            ))
            .when(
                !form.bound_to(&self.user, &self.room, self.generation),
                |d| d.child("原对话已变化，提交已禁用；输入保留，请返回原对话核对。"),
            )
            .when(form.has_target(), |d| d.child(self.input(form.target_label(), &form.target, Field::DirectTarget, false, cx)))
            .when(form.has_extra(), |d|d.child(self.input(form.extra_label(),&form.extra,Field::DirectExtra,false,cx)))
            .when(form.has_body(), |d| d.child(self.input(form.body_label(), &form.body, Field::DirectBody, false, cx)))
            .when(form.simple_action().is_some() || form.community_intent().is_some(), |d| d.child(div().text_size(px(12.)).child(form.community_intent().map_or_else(||format!("目标会话：{} · 点击确认后发送消息", form.room),|i|format!("目标频道：#{} · 条目：{}{} · 原会话：{}",i.room,if i.object_id==0 {"新建".into()}else{format!("#{}",i.object_id)},if i.choice>0 {format!(" · 选项 {}",i.choice)}else{String::new()},form.room)))))
            .when_some(form.secret_admin_kind(), |d,k|d.child(k.explanation()))
            .when_some(form.configuration_intent(),|d,i|d.child(i.explanation()))
            .when_some(form.management_intent(),|d,i|d.child(i.explanation()))
            .when_some(form.sanction_intent(),|d,i|d.child(i.explanation()).when_some(i.self_ip_force,|d,checked|d.child(Self::button("confirm-self-ip-impact",if checked {"☑ 允许影响当前聊天室连接 · Ctrl+Shift+F"}else{"□ 允许影响当前聊天室连接 · Ctrl+Shift+F"}).on_click(cx.listener(|this,_,_,cx|this.toggle_sanction_force(cx))))))
            .when_some(form.report_intent(),|d,i|d.child(if i.has_body(){miochat_gpui::report::DISCLOSURE.into()}else{format!("举报 #{} 设置为 {}；可覆盖当前处理状态，确认后写入审计。",i.id,i.status)}))
            .when_some(form.word_intent(),|d,i|d.child(format!("目标频道 #{} · 游戏 #{} · 此字段是公开猜测，不是创建答案",i.room,i.object_id)))
            .when_some(form.guess_intent(),|d,i|d.child(if i.object_id==0 {format!("目标频道 #{} · 新游戏上限按上方输入 · 猜测和结果对频道可见",i.room)} else {format!("目标频道 #{} · 游戏 #{} · 范围1–{} · 猜测和结果对频道可见",i.room,i.object_id,i.maximum)}))
            .when_some(form.tic_intent(),|d,i|d.child(if i.object_id==0 {format!("目标频道 #{} · 邀请对手进入新棋局",i.room)} else {format!("棋盘频道 #{} · 对局 #{}{} · 棋盘版本 {}",i.room,i.object_id,if i.cell>0 {format!(" · 格子 {}",i.cell)}else{String::new()},i.revision.map_or("未知".into(),|v|v.to_string()))}))
            .when(form.has_body(), |d| d.child(
                div()
                    .text_size(px(12.))
                    .text_color(rgb(MUTED))
                    .child(format!(
                        "{} / {} · Shift+Enter 换行",
                        form.body.chars().count(),form.limit(miochat_gpui::direct_message::FormSlot::Body)
                    )),
            ))
            .child(
                div()
                    .flex()
                    .gap(px(10.))
                    .child(
                        Self::button("cancel-one-off-dm", "取消")
                            .on_click(cx.listener(|this, _, _, cx| this.cancel_direct_form(cx))),
                    )
                    .child(
                        Self::button(
                            "send-one-off-dm",
                            if form.pending() {
                                if matches!(form.purpose,miochat_gpui::direct_message::FormPurpose::Moderation(_)) {"查询中…"}else{"发送中…"}
                            } else {
                                form.title()
                            },
                        )
                        .on_click(cx.listener(|this, _, _, cx| this.submit_direct_form(cx))),
                    ),
            )
            .when_some(self.error.clone(), |d, e| d.child(self.error_box(e)));
        div()
            .id("one-off-dm-overlay")
            .absolute()
            .top(px(0.))
            .left(px(0.))
            .size_full()
            .bg(gpui::rgba(0x00000020))
            .on_click(|_, _, cx| cx.stop_propagation())
            .child(panel)
    }
}
