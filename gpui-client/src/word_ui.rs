use super::*;
use miochat_gpui::{direct_message::FormPurpose, word::WordStartForm};
impl MioChatApp {
    pub(super) fn word_input_active(&self) -> bool {
        routes_word_input(
            self.screen,
            self.field,
            self.word_start.as_ref(),
            &self.user,
            &self.room,
            self.generation,
        )
    }
    pub(super) fn cancel_word_start(&mut self, cx: &mut Context<Self>) {
        discard_word_input(&mut self.word_start, &mut self.field);
        self.field = Field::Username;
        self.reset_selection();
        cx.notify()
    }
    pub(super) fn activate_word_action(
        &mut self,
        command: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        if miochat_gpui::word::is_view(command) {
            if self.screen == Screen::Chat
                && self.word_start.is_none()
                && self.direct_form.is_none()
            {
                self.run_command("/word".into(), true, cx)
            }
            return true;
        }
        if !command.starts_with("local:word-") {
            return false;
        }
        if self.screen != Screen::Chat
            || self.direct_form.is_some()
            || self.word_start.is_some()
            || self.pending_send.is_some()
        {
            return true;
        }
        let Some(intent) = self
            .feature_panel
            .as_ref()
            .and_then(|p| miochat_gpui::word::native_intent(command, p, &self.room))
        else {
            return true;
        };
        if intent.start {
            self.begin_feature_navigation();
            self.clear_profile();
            self.center_open = false;
            self.preferences_open = false;
            self.room_form = None;
            self.set_feature_panel(None);
            self.error = None;
            self.message_selection = None;
            self.word_start = Some(WordStartForm::new(
                self.user.clone(),
                self.room.clone(),
                self.generation,
                intent,
            ));
            self.field = Field::WordSecret;
            self.reset_selection();
        } else {
            self.open_direct_form(cx);
            if let Some(f) = self.direct_form.as_mut() {
                f.purpose = FormPurpose::Word(intent);
                self.field = Field::DirectBody;
                self.reset_selection()
            }
        }
        self.focus.focus(window, cx);
        cx.notify();
        true
    }
    pub(super) fn submit_word_start(&mut self, cx: &mut Context<Self>) {
        if !self.word_input_active() || self.marked_range.is_some() || self.pending_send.is_some() {
            return;
        }
        if self.persistence_enabled
            && (!self.persistence.ready()
                || self.persistence.pending()
                || self.queued_room.is_some()
                || self.close_started.is_some())
        {
            return;
        }
        let form = self.word_start.as_mut().unwrap();
        let command = match form.take_command() {
            Ok(c) => c,
            Err(e) => {
                self.error = Some(e.into());
                cx.notify();
                return;
            }
        };
        let intent = form.intent.clone();
        let origin = form.origin.clone();
        let id = self.next_request_id("word-start");
        let mut request = Request::simple("command", &id);
        request.room = &origin;
        request.word_mode = "start";
        request.expected_room = &intent.room;
        request.body = &command;
        // Never call PendingSend::command with the secret-bearing command.
        let queued = self.send_request(&request).is_ok();
        discard_word_input(&mut self.word_start, &mut self.field);
        self.field = Field::Username;
        self.reset_selection();
        if queued {
            self.pending_send = Some(PendingSend::word_start(id, origin, intent));
            self.status =
                "创建请求已排队，答案输入已清除；结果不明时先查看游戏，勿直接重试。".into();
        } else {
            self.error =
                Some("未能确认创建请求是否送达；答案输入已清除，请先查看游戏再决定重试。".into())
        }
        cx.notify();
    }
    pub(super) fn word_start_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let f = self.word_start.as_ref().unwrap();
        div().id("word-secret-overlay").absolute().size_full().top(px(0.)).left(px(0.)).bg(gpui::rgba(0x00000020)).on_click(|_,_,cx|cx.stop_propagation())
        .child(div().absolute().top(px(80.)).right(px(24.)).w(px(420.)).p(px(20.)).rounded(px(12.)).bg(self.raised_color()).flex().flex_col().gap(px(14.))
        .child("创建猜词 · 秘密答案")
        .child(format!("固定频道 #{} · 2–24字符；不会写入聊天草稿。服务器仍会存储答案，结束后按游戏规则公开。",f.intent.room))
        .child(self.input("秘密答案",f.value(),Field::WordSecret,true,cx))
        .child("答案不支持复制/剪切。提交尝试后清除输入；不保证输入法或进程内存安全擦除。")
        .child(div().flex().gap(px(10.)).child(Self::button("word-secret-cancel","取消并清除输入").on_click(cx.listener(|this,_,_,cx|this.cancel_word_start(cx))))
        .child(Self::button("word-secret-submit","确认创建").on_click(cx.listener(|this,_,_,cx|this.submit_word_start(cx)))))
        .when_some(self.error.clone(),|d,e|d.child(self.error_box(e))))
    }
}
