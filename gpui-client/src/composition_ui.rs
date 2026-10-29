use super::*;
use miochat_gpui::composition::{Editor, Effect, Operation, Slot};

#[derive(Clone)]
pub(super) enum CompositionAfter {
    Editor,
    Room(String),
    Window,
}
pub(super) fn composition_slot(field: Field) -> Option<Slot> {
    match field {
        Field::CompositionTitle => Some(Slot::Title),
        Field::CompositionBody => Some(Slot::Body),
        _ => None,
    }
}
fn composition_is_visible(
    screen: Screen,
    editor: Option<&Editor>,
    owner: &str,
    origin: Option<&miochat_gpui::composition::Origin>,
) -> bool {
    screen == Screen::Chat && editor.is_some_and(|e| e.belongs_to_origin(owner, origin))
}
fn recovery_eligible(
    screen: Screen,
    editor: Option<&Editor>,
    owner: &str,
    origin: Option<&miochat_gpui::composition::Origin>,
    room: &str,
    connected: bool,
) -> bool {
    connected
        && composition_is_visible(screen, editor, owner, origin)
        && editor.is_some_and(|e| e.room == room)
}
fn consume_opening_input(current: &str, captured: Option<&str>, origin: &str, scope: &str) -> bool {
    origin == scope && captured.is_some_and(|text| text == current)
}
fn routes_composition_input(
    screen: Screen,
    field: Field,
    editor: Option<&Editor>,
    owner: &str,
    generation: u64,
) -> bool {
    screen == Screen::Chat
        && composition_slot(field).is_some()
        && editor
            .is_some_and(|e| e.bound(owner, generation) && e.ready() && !e.frozen && !e.preview)
}
fn matching_composition_response(editor: Option<&Editor>, request_id: &str) -> bool {
    editor.is_some_and(|e| {
        e.pending
            .as_ref()
            .is_some_and(|p| p.request_id == request_id)
    })
}
fn composition_control_allowed(event: &KeyDownEvent, composing: bool) -> bool {
    !event.is_held && !event.prefer_character_input && !composing
}
pub(super) fn composition_command(command: &str) -> Option<(&str, &str)> {
    let (head, arg) = miochat_gpui::configuration::split_argument(command);
    match head {
        "/post" => Some(("post", arg)),
        "/draw" => Some(("art", arg)),
        _ => None,
    }
}
impl MioChatApp {
    pub(super) fn composition_blocks_navigation(&mut self, cx: &mut Context<Self>) -> bool {
        if !self.composition_visible() {
            return false;
        }
        if let Some(e) = self.composition.as_mut() {
            e.status = "请先保存并返回长文编辑器，或明确保留/舍弃本地输入。".into();
        }
        cx.notify();
        true
    }
    pub(super) fn composition_visible(&self) -> bool {
        composition_is_visible(
            self.screen,
            self.composition.as_ref(),
            &self.user,
            self.session_origin.as_ref(),
        )
    }
    pub(super) fn composition_input_active(&self) -> bool {
        self.composition_visible()
            && routes_composition_input(
                self.screen,
                self.field,
                self.composition.as_ref(),
                &self.user,
                self.generation,
            )
    }
    pub(super) fn activate_composition(
        &mut self,
        command: &str,
        preserve_draft: bool,
        cx: &mut Context<Self>,
    ) -> bool {
        let Some((kind, title)) = composition_command(command) else {
            return false;
        };
        if kind == "post" && title.parse::<i64>().is_ok_and(|id| id > 0) {
            self.run_command(format!("/object {title}"), preserve_draft, cx);
            return true;
        }
        if self.screen != Screen::Chat || self.session.is_none() || self.session_origin.is_none() {
            return true;
        }
        if self.composition.is_some() {
            self.error = Some("请先保存/处理当前长文编辑器，再打开另一个；本地输入仍保留。".into());
            cx.notify();
            return true;
        }
        if self.pending_send.is_some()
            || self.direct_form.is_some()
            || self.word_start.is_some()
            || self.pending_room.is_some()
            || self.room_form.is_some()
            || self.profile_editor.is_some()
        {
            self.error = Some("请先完成当前操作，再打开长文编辑器。".into());
            cx.notify();
            return true;
        }
        if self.room.starts_with("dm:") {
            self.error = Some("长帖与画板请在实际频道中使用，不会使用上一次频道。".into());
            cx.notify();
            return true;
        }
        self.begin_feature_navigation();
        self.set_feature_panel(None);
        self.clear_profile();
        self.clear_center();
        self.preferences_open = false;
        self.appearance_menu = false;
        self.composition = Editor::new(
            self.user.clone(),
            self.room.clone(),
            kind.into(),
            title.into(),
            self.generation,
        );
        if let Some(e) = self.composition.as_mut() {
            e.origin = self.session_origin.clone();
        }
        self.composition_chat_input = if !preserve_draft && self.composer.trim() == command.trim() {
            Some(self.composer.clone())
        } else {
            None
        };
        self.field = Field::CompositionBody;
        self.reset_selection();
        self.queue_composition(Operation::Load, cx);
        true
    }
    pub(super) fn queue_composition(&mut self, operation: Operation, cx: &mut Context<Self>) {
        if !self.composition_visible() || self.session.is_none() || self.marked_range.is_some() {
            return;
        }
        let Some(e) = self.composition.as_ref() else {
            return;
        };
        if !e.bound(&self.user, self.generation) {
            return;
        }
        let now = self.persistence_now();
        let id = self.next_request_id("composition");
        let Some(data) = self
            .composition
            .as_mut()
            .and_then(|e| e.begin(operation, id.clone(), now))
        else {
            return;
        };
        let mut request = Request::simple("composition", &id);
        request.room = &self.room;
        request.composition = Some(&data);
        if let Err(error) = self.send_request(&request) {
            self.composition.as_mut().unwrap().failed(
                &id,
                &format!("请求未确认：{error:#}；本地输入保留，勿自动重发。"),
            );
            self.composition_after = None;
        }
        cx.notify();
    }
    pub(super) fn keep_composition_local(&mut self, cx: &mut Context<Self>) {
        if !self.composition_visible() || self.session.is_none() || self.marked_range.is_some() {
            return;
        }
        if self
            .composition
            .as_ref()
            .is_none_or(|e| !e.bound(&self.user, self.generation))
        {
            return;
        }
        let now = self.persistence_now();
        let id = self.next_request_id("composition-keep");
        let Some(data) = self
            .composition
            .as_mut()
            .and_then(|e| e.keep_local(id.clone(), now))
        else {
            return;
        };
        let mut request = Request::simple("composition", &id);
        request.room = &self.room;
        request.composition = Some(&data);
        if let Err(error) = self.send_request(&request) {
            self.composition
                .as_mut()
                .unwrap()
                .failed(&id, &format!("保留本地请求未确认：{error:#}"));
        }
        cx.notify();
    }
    pub(super) fn close_composition(
        &mut self,
        after: CompositionAfter,
        cx: &mut Context<Self>,
    ) -> bool {
        let Some(e) = self.composition.as_mut() else {
            return true;
        };
        if !e.belongs_to_origin(&self.user, self.session_origin.as_ref())
            || self.screen != Screen::Chat
            || self.session.is_none()
        {
            self.error = Some(
                "仍有原账号的未确认长文输入；请回原账号恢复或明确舍弃，窗口不会自动关闭。".into(),
            );
            cx.notify();
            return false;
        }
        self.composition_after = Some(after);
        if e.request_close() {
            self.finish_composition_close(cx);
            return false;
        }
        if e.blocked || e.frozen && !e.bound(&self.user, self.generation) {
            e.status = "保存未确认；保留本地窗口，请读取原频道版本明确处理或复制/舍弃。".into();
        } else if e.pending.is_none() {
            let op = if e.frozen {
                Operation::Suspend
            } else {
                Operation::Save
            };
            self.queue_composition(op, cx);
        }
        cx.notify();
        false
    }
    fn finish_composition_close(&mut self, cx: &mut Context<Self>) {
        self.composition = None;
        self.composition_layout = None;
        self.composition_drag = None;
        self.composition_cursor = None;
        self.composition_chat_input = None;
        let after = self.composition_after.take();
        self.field = Field::Username;
        self.reset_selection();
        match after {
            Some(CompositionAfter::Room(room)) => self.select_room(room, cx),
            Some(CompositionAfter::Window) if self.request_close(cx) => {
                if let Some(handle) = self.window_handle {
                    let _ = handle.update(cx, |_, window, _| window.remove_window());
                }
            }
            _ => {}
        }
        cx.notify();
    }
    pub(super) fn handle_composition_response(
        &mut self,
        response: &Response,
        cx: &mut Context<Self>,
    ) -> bool {
        let relevant = response.composition.is_some()
            || response.kind == "composition"
            || self.composition.as_ref().is_some_and(|e| {
                e.pending
                    .as_ref()
                    .is_some_and(|p| p.request_id == response.request_id)
            });
        if !relevant {
            return false;
        }
        if !self.composition_visible()
            || !miochat_gpui::composition::response_admitted(
                response,
                self.composition.as_ref(),
                &self.user,
                self.generation,
                self.screen == Screen::Chat,
            )
        {
            if matching_composition_response(self.composition.as_ref(), &response.request_id)
                && let Some(e) = self.composition.as_mut()
            {
                e.failed(
                    &response.request_id,
                    "编辑器回执不匹配，输入保留；请核对原频道，勿重发发布。",
                );
                self.composition_after = None;
                cx.notify();
            }
            return true;
        }
        if response.kind == "error" {
            self.composition
                .as_mut()
                .unwrap()
                .failed(&response.request_id, &response.error);
            self.composition_after = None;
            cx.notify();
            return true;
        }
        let effect = self.composition.as_mut().and_then(|e| e.accept(response));
        if response
            .composition
            .as_ref()
            .is_some_and(|c| c.operation == Operation::Reserve)
            && effect.is_some()
        {
            if consume_opening_input(
                &self.composer,
                self.composition_chat_input.as_deref(),
                &self.composition.as_ref().unwrap().room,
                &self.room,
            ) {
                self.composer.clear();
            }
            self.composition_chat_input = None;
        }
        match effect {
            Some(Effect::NeedReserve) => self.queue_composition(Operation::Reserve, cx),
            Some(Effect::Ready | Effect::Reloaded) => self.reset_selection(),
            Some(Effect::Close) => self.finish_composition_close(cx),
            Some(Effect::Published {
                object_id,
                newer_input: false,
            }) => {
                if self.composition_after.is_some() {
                    self.finish_composition_close(cx);
                    return true;
                }
                let origin = self.composition.as_ref().unwrap().room.clone();
                self.composition_after = None;
                self.composition = None;
                self.composition_layout = None;
                self.composition_drag = None;
                self.composition_cursor = None;
                self.composition_chat_input = None;
                self.reset_selection();
                self.status = response.status.clone();
                if self.room == origin {
                    self.run_command(format!("/object {object_id}"), true, cx);
                }
            }
            Some(Effect::Published {
                newer_input: true, ..
            }) => {
                self.composition_after = None;
            }
            _ => {}
        }
        cx.notify();
        true
    }
    pub(super) fn composition_scope_changed(&mut self, room: &str, forced: bool) {
        if self
            .composition
            .as_mut()
            .is_some_and(|e| e.scope_changed(room, forced))
        {
            self.composition_after = None;
            self.composition_chat_input = None;
            self.composition_layout = None;
            self.composition_drag = None;
            self.composition_cursor = None;
        }
    }
    pub(super) fn poll_composition(&mut self, cx: &mut Context<Self>) {
        if !self.composition_visible() {
            return;
        }
        let now = self.persistence_now();
        let Some(e) = self.composition.as_mut() else {
            return;
        };
        if self.screen != Screen::Chat || e.owner != self.user || e.generation != self.generation {
            return;
        }
        if e.room != self.room {
            e.scope_changed(&self.room, false);
            self.composition_after = None;
        }
        if e.expire(now) {
            self.composition_after = None;
            cx.notify();
        }
        if self.marked_range.is_some() || self.session.is_none() {
            return;
        }
        let due = e.autosave_due(now)
            || e.close_after == Some(e.revision) && e.dirty() && !e.blocked && e.pending.is_none();
        if due {
            let op = if e.frozen {
                Operation::Suspend
            } else {
                Operation::Save
            };
            self.queue_composition(op, cx);
        }
    }
    pub(super) fn resume_composition(&mut self, cx: &mut Context<Self>) {
        if !recovery_eligible(
            self.screen,
            self.composition.as_ref(),
            &self.user,
            self.session_origin.as_ref(),
            &self.room,
            self.session.is_some(),
        ) {
            self.error = Some(
                "请回原连接/原账号和原频道后明确恢复；不会在另一服务器显示或提交保留文本。".into(),
            );
            cx.notify();
            return;
        }
        let Some(e) = self.composition.as_mut() else {
            return;
        };
        if e.room != self.room {
            self.error =
                Some("请先返回编辑器标明的原频道，再核对远端草稿；不会改发其它频道。".into());
            cx.notify();
            return;
        }
        e.interrupt();
        e.generation = self.generation;
        e.frozen = false;
        e.blocked = true;
        self.field = Field::CompositionBody;
        self.reset_selection();
        self.queue_composition(Operation::Remote, cx);
    }
    pub(super) fn composition_key(
        &mut self,
        event: &KeyDownEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        if !self.composition_visible() {
            if let Some(e) = self.composition.as_mut() {
                let key = event.keystroke.key.as_str();
                if matches!(key, "f9" | "f10") {
                    if composition_control_allowed(event, self.marked_range.is_some()) {
                        if key == "f9" {
                            e.discard_armed = true;
                        } else if e.discard_armed && e.pending.is_none() {
                            self.composition = None;
                            self.composition_layout = None;
                            self.composition_drag = None;
                            self.composition_cursor = None;
                            self.composition_chat_input = None;
                            self.composition_after = None;
                        }
                    }
                    cx.stop_propagation();
                    cx.notify();
                    return true;
                }
                e.discard_armed = false;
            }
            return false;
        }
        let key = event.keystroke.key.as_str();
        let command = event.keystroke.modifiers.control || event.keystroke.modifiers.platform;
        self.selection = editing::valid_selection(self.current_value(), self.selection.clone());
        if !matches!(key, "f9" | "f10")
            && let Some(e) = self.composition.as_mut()
        {
            e.discard_armed = false;
        }
        if self.marked_range.is_none()
            && !event.prefer_character_input
            && self.composition_move(key, event.keystroke.modifiers.shift, command)
        {
            cx.stop_propagation();
            cx.notify();
            return true;
        }
        if matches!(
            key,
            "f2" | "f4" | "f6" | "f7" | "f8" | "f9" | "f10" | "escape" | "tab" | "enter"
        ) || command && key == "s"
        {
            if !composition_control_allowed(event, self.marked_range.is_some()) {
                cx.stop_propagation();
                return true;
            }
            match key {
                "f2" => {
                    if let Some(e) = self.composition.as_mut() {
                        e.preview = !e.preview;
                    }
                }
                "f4" => self.queue_composition(Operation::Publish, cx),
                "f6" => self.queue_composition(Operation::Remote, cx),
                "f7" => self.keep_composition_local(cx),
                "f8" => self.queue_composition(Operation::Reload, cx),
                "f9" => {
                    if let Some(e) = self.composition.as_mut() {
                        e.discard_armed = true;
                        e.status = "F10明确舍弃本窗口未保存文本；服务器草稿不删除。".into();
                    }
                }
                "f10" => {
                    if self
                        .composition
                        .as_ref()
                        .is_some_and(|e| e.discard_armed && e.pending.is_none())
                    {
                        self.composition_after = None;
                        self.composition = None;
                        self.composition_layout = None;
                        self.composition_drag = None;
                        self.composition_cursor = None;
                        self.composition_chat_input = None;
                        self.reset_selection();
                    }
                }
                "escape" => {
                    self.close_composition(CompositionAfter::Editor, cx);
                }
                "tab" => {
                    if self.field == Field::CompositionBody
                        && !command
                        && !event.keystroke.modifiers.shift
                    {
                        self.replace_current(None, "    ");
                        cx.stop_propagation();
                        cx.notify();
                        return true;
                    }
                    self.composition_cursor = None;
                    self.field = if self.field == Field::CompositionTitle {
                        Field::CompositionBody
                    } else {
                        Field::CompositionTitle
                    };
                    self.reset_selection();
                    self.focus.focus(window, cx);
                }
                "enter" => {
                    if self.field == Field::CompositionBody {
                        self.replace_current(None, "\n");
                    } else {
                        self.field = Field::CompositionBody;
                        self.reset_selection();
                    }
                }
                "s" => {
                    let op = if self.composition.as_ref().is_some_and(|e| e.frozen) {
                        Operation::Suspend
                    } else {
                        Operation::Save
                    };
                    self.queue_composition(op, cx);
                }
                _ => {}
            }
            cx.stop_propagation();
            cx.notify();
            return true;
        }
        if let Some(e) = self.composition.as_mut() {
            e.discard_armed = false;
        }
        if !self.composition_input_active() {
            if command
                && key == "c"
                && let Some(e) = self.composition.as_ref()
            {
                cx.write_to_clipboard(gpui::ClipboardItem::new_string(format!(
                    "{}\n{}",
                    e.title, e.body
                )));
            }
            cx.stop_propagation();
            return true;
        }
        // Stop this editor's keys reaching room/profile/chat command handlers.
        // Unrecognized text keys remain unconsumed for the platform input handler.
        if self.marked_range.is_some() && !command {
            return true;
        }
        let mut handled = true;
        if command && key == "v" {
            if let Some(text) = cx.read_from_clipboard().and_then(|v| v.text()) {
                self.replace_current(None, &text);
            }
        } else if command && key == "a" {
            self.selection = 0..self.current_value().len();
            self.composition_cursor = Some((0, self.current_value().len()));
        } else if command && matches!(key, "c" | "x") {
            if !self.selection.is_empty() {
                cx.write_to_clipboard(gpui::ClipboardItem::new_string(
                    self.current_value()[self.selection.clone()].into(),
                ));
                if key == "x" {
                    self.replace_current(None, "");
                }
            }
        } else {
            match key {
                "backspace" | "delete" => {
                    let previous = self.selection.clone();
                    if self.selection.is_empty() {
                        if key == "backspace" {
                            self.selection.start =
                                editing::previous(self.current_value(), self.selection.start);
                        } else {
                            self.selection.end =
                                editing::next(self.current_value(), self.selection.end);
                        }
                    }
                    if !self.replace_current(None, "") {
                        self.selection = previous;
                    }
                }
                "left" | "right" | "home" | "end" => {
                    let at = match key {
                        "left"
                            if !event.keystroke.modifiers.shift && !self.selection.is_empty() =>
                        {
                            self.selection.start
                        }
                        "right"
                            if !event.keystroke.modifiers.shift && !self.selection.is_empty() =>
                        {
                            self.selection.end
                        }
                        "left" => editing::previous(self.current_value(), self.composition_caret()),
                        "right" => editing::next(self.current_value(), self.composition_caret()),
                        "home" => 0,
                        _ => self.current_value().len(),
                    };
                    self.move_composition_selection(at, event.keystroke.modifiers.shift);
                }
                _ => handled = false,
            }
        }
        if handled {
            self.composition_reveal = true;
            cx.stop_propagation();
            cx.notify();
        }
        true
    }
    pub(super) fn composition_recovery_notice(&self, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .absolute()
            .bottom(px(12.))
            .left(px(12.))
            .right(px(12.))
            .p(px(12.))
            .bg(self.raised_color())
            .border_1()
            .border_color(self.preference_border())
            .flex()
            .gap(px(8.))
            .child("保留着原账号的未确认长文输入。此页面不显示/编辑正文；回原账号可恢复。F9 后 F10 可明确舍弃本地输入。")
            .child(
                Self::button("composition-hidden-arm", "舍弃说明").on_click(cx.listener(
                    |this, _, _, cx| {
                        if let Some(e) = this.composition.as_mut() {
                            e.discard_armed = true;
                        }
                        cx.notify();
                    },
                )),
            )
            .child(
                Self::button("composition-hidden-discard", "确认仅舍弃本地输入").on_click(
                    cx.listener(|this, _, _, cx| {
                        if this
                            .composition
                            .as_ref()
                            .is_some_and(|e| e.discard_armed && e.pending.is_none())
                        {
                            this.composition = None;
                            this.composition_layout = None;
                            this.composition_drag = None;
                            this.composition_cursor = None;
                            this.composition_chat_input = None;
                            this.composition_after = None;
                        }
                        cx.notify();
                    }),
                ),
            )
    }
    pub(super) fn composition_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let e = self.composition.as_ref().unwrap();
        let mut panel = div()
            .id("composition-editor")
            .absolute()
            .top(px(65.))
            .left(px(16.))
            .right(px(16.))
            .bottom(px(16.))
            .overflow_y_scroll()
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
            .child(format!(
                "{} · #{} · {}",
                if e.kind == "art" {
                    "ASCII画板"
                } else {
                    "长帖编辑"
                },
                e.room,
                if e.frozen {
                    "原频道已暂停"
                } else {
                    "独立草稿"
                }
            ))
            .child(div().text_size(px(12.)).child(e.status.clone()))
            .child(self.input("作品标题", &e.title, Field::CompositionTitle, false, cx));
        if e.preview || e.frozen || !e.bound(&self.user, self.generation) {
            panel = panel.child(
                div()
                    .id("composition-preview")
                    .max_h(px(360.))
                    .overflow_y_scroll()
                    .overflow_x_scroll()
                    .font_family("Consolas")
                    .whitespace_nowrap()
                    .child(e.body.clone()),
            );
        } else {
            panel = panel.child(self.composition_text(cx));
        }
        panel = panel.child(
            div()
                .flex()
                .flex_wrap()
                .gap(px(6.))
                .child(
                    Self::button("composition-preview-toggle", "F2 编辑/预览").on_click(
                        cx.listener(|this, _, _, cx| {
                            if this.composition_visible()
                                && this.marked_range.is_none()
                                && let Some(e) = this.composition.as_mut()
                            {
                                e.discard_armed = false;
                                e.preview = !e.preview;
                            }
                            cx.notify();
                        }),
                    ),
                )
                .child(
                    Self::button("composition-save", "保存 Ctrl+S").on_click(cx.listener(
                        |this, _, _, cx| {
                            let op = if this.composition.as_ref().is_some_and(|e| e.frozen) {
                                Operation::Suspend
                            } else {
                                Operation::Save
                            };
                            this.queue_composition(op, cx);
                        },
                    )),
                )
                .child(Self::button("composition-publish", "F4 发布").on_click(
                    cx.listener(|this, _, _, cx| this.queue_composition(Operation::Publish, cx)),
                ))
                .child(
                    Self::button("composition-close", "Esc 保存并返回").on_click(cx.listener(
                        |this, _, _, cx| {
                            this.close_composition(CompositionAfter::Editor, cx);
                        },
                    )),
                )
                .child(
                    Self::button("composition-remote", "F6 读取远端预览").on_click(
                        cx.listener(|this, _, _, cx| this.queue_composition(Operation::Remote, cx)),
                    ),
                )
                .child(
                    Self::button("composition-resume", "恢复连接后核对原稿")
                        .on_click(cx.listener(|this, _, _, cx| this.resume_composition(cx))),
                )
                .child(
                    Self::button("composition-copy", "复制本地输入").on_click(cx.listener(
                        |this, _, _, cx| {
                            if let Some(e) = this.composition.as_ref().filter(|e| {
                                e.belongs_to_origin(&this.user, this.session_origin.as_ref())
                                    && this.screen == Screen::Chat
                            }) {
                                cx.write_to_clipboard(gpui::ClipboardItem::new_string(format!(
                                    "{}\n{}",
                                    e.title, e.body
                                )));
                            }
                        },
                    )),
                )
                .child(
                    Self::button("composition-discard-arm", "F9 舍弃说明").on_click(cx.listener(
                        |this, _, _, cx| {
                            if let Some(e) = this.composition.as_mut() {
                                e.discard_armed = true;
                                e.status =
                                    "确认仅舍弃本地文本；不删除服务器草稿，正在提交时不能舍弃。"
                                        .into();
                            }
                            cx.notify();
                        },
                    )),
                )
                .child(
                    Self::button("composition-discard-confirm", "F10 确认舍弃").on_click(
                        cx.listener(|this, _, _, cx| {
                            if this
                                .composition
                                .as_ref()
                                .is_some_and(|e| e.discard_armed && e.pending.is_none())
                            {
                                this.composition = None;
                                this.composition_layout = None;
                                this.composition_drag = None;
                                this.composition_cursor = None;
                                this.composition_chat_input = None;
                                this.composition_after = None;
                                this.reset_selection();
                            }
                            cx.notify();
                        }),
                    ),
                ),
        );
        if let Some(remote) = e.remote.as_ref() {
            panel = panel
                .child(div().text_size(px(12.)).child(format!(
                    "远端版本 {} 只读预览；本地输入尚未替换",
                    remote.version
                )))
                .child(
                    div()
                        .id("composition-remote-preview")
                        .max_h(px(200.))
                        .overflow_y_scroll()
                        .overflow_x_scroll()
                        .font_family("Consolas")
                        .whitespace_nowrap()
                        .child(format!("{}\n{}", remote.title, remote.body)),
                )
                .child(
                    div()
                        .flex()
                        .gap(px(8.))
                        .child(
                            Self::button("composition-reload", "F8 重载并舍弃本地").on_click(
                                cx.listener(|this, _, _, cx| {
                                    this.queue_composition(Operation::Reload, cx)
                                }),
                            ),
                        )
                        .child(
                            Self::button("composition-keep", "F7 以本地覆盖此版本").on_click(
                                cx.listener(|this, _, _, cx| this.keep_composition_local(cx)),
                            ),
                        ),
                );
        }
        panel.child(div().text_size(px(12.)).child("正文Tab插入4空格，Shift+Tab/Ctrl+Tab切标题；32行 / 每行160字符 / 总4096字符；草稿可含中文，ASCII作品仅发布时校验。断线/强制退出只能保证已确认保存的内容。"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn actual_composition_input_and_control_routes_isolate_credentials_ime_and_repeat() {
        let mut e = Editor::new(
            "alice".into(),
            "lobby".into(),
            "post".into(),
            "title".into(),
            7,
        )
        .unwrap();
        e.snapshot = Some(miochat_gpui::composition::Snapshot {
            owner: "alice".into(),
            room: "lobby".into(),
            kind: "post".into(),
            title: "title".into(),
            body: "private".into(),
            version: 1,
            exists: true,
        });
        for field in [Field::CompositionTitle, Field::CompositionBody] {
            assert!(routes_composition_input(
                Screen::Chat,
                field,
                Some(&e),
                "alice",
                7
            ));
        }
        for screen in [
            Screen::Auth,
            Screen::Password,
            Screen::KeyBinding,
            Screen::Connect,
            Screen::Trust,
        ] {
            assert!(!routes_composition_input(
                screen,
                Field::CompositionBody,
                Some(&e),
                "alice",
                7
            ));
        }
        assert!(!routes_composition_input(
            Screen::Chat,
            Field::Password,
            Some(&e),
            "alice",
            7
        ));
        assert!(!routes_composition_input(
            Screen::Chat,
            Field::CompositionBody,
            Some(&e),
            "bob",
            7
        ));
        assert!(!routes_composition_input(
            Screen::Chat,
            Field::CompositionBody,
            Some(&e),
            "alice",
            8
        ));
        e.preview = true;
        assert!(!routes_composition_input(
            Screen::Chat,
            Field::CompositionBody,
            Some(&e),
            "alice",
            7
        ));
        e.preview = false;
        e.interrupt();
        assert!(!routes_composition_input(
            Screen::Chat,
            Field::CompositionBody,
            Some(&e),
            "alice",
            7
        ));
        let mut key = KeyDownEvent {
            keystroke: gpui::Keystroke {
                key: "f4".into(),
                ..Default::default()
            },
            is_held: false,
            prefer_character_input: false,
        };
        assert!(composition_control_allowed(&key, false));
        assert!(!composition_control_allowed(&key, true));
        key.is_held = true;
        assert!(!composition_control_allowed(&key, false));
        key.is_held = false;
        key.prefer_character_input = true;
        assert!(!composition_control_allowed(&key, false));
        assert_eq!(composition_command("/post +17"), Some(("post", "+17")));
        assert_eq!(
            composition_command("/draw 原频道标题"),
            Some(("art", "原频道标题"))
        );
        assert!(composition_command("/postx title").is_none());
    }
    #[test]
    fn actual_origin_recovery_and_typed_opening_consumption_are_scope_bound() {
        use miochat_gpui::composition::Origin;
        let spec = ConnectionSpec {
            host: "server-a.example".into(),
            port: 2222,
            ssh_user: "chat".into(),
        };
        let origin = Origin::ssh(&spec, "ssh-ed25519 SYNTHETIC_A".into(), "").unwrap();
        let mut e = Editor::new(
            "alice".into(),
            "lobby".into(),
            "post".into(),
            "title".into(),
            7,
        )
        .unwrap();
        e.origin = Some(origin.clone());
        assert!(composition_is_visible(
            Screen::Chat,
            Some(&e),
            "alice",
            Some(&origin)
        ));
        assert!(recovery_eligible(
            Screen::Chat,
            Some(&e),
            "alice",
            Some(&origin),
            "lobby",
            true
        ));
        e.generation = 6; // a new authenticated connection still requires explicit resume
        assert!(recovery_eligible(
            Screen::Chat,
            Some(&e),
            "alice",
            Some(&origin),
            "lobby",
            true
        ));
        assert!(!e.bound("alice", 7));
        for n in 0..5 {
            let mut other = spec.clone();
            let mut key = "ssh-ed25519 SYNTHETIC_A";
            let mut identity = "";
            match n {
                0 => other.host = "server-b.example".into(),
                1 => other.port = 2223,
                2 => other.ssh_user = "other".into(),
                3 => key = "ssh-ed25519 SYNTHETIC_B",
                _ => identity = "synthetic-other-key",
            };
            let other = Origin::ssh(&other, key.into(), identity).unwrap();
            assert!(!composition_is_visible(
                Screen::Chat,
                Some(&e),
                "alice",
                Some(&other)
            ));
            assert!(!recovery_eligible(
                Screen::Chat,
                Some(&e),
                "alice",
                Some(&other),
                "lobby",
                true
            ));
        }
        assert!(!composition_is_visible(
            Screen::Chat,
            Some(&e),
            "alice",
            None
        ));
        assert!(!composition_is_visible(
            Screen::Password,
            Some(&e),
            "alice",
            Some(&origin)
        ));
        assert!(!recovery_eligible(
            Screen::Chat,
            Some(&e),
            "bob",
            Some(&origin),
            "lobby",
            true
        ));
        assert!(!recovery_eligible(
            Screen::Chat,
            Some(&e),
            "alice",
            Some(&origin),
            "elsewhere",
            true
        ));
        assert!(!recovery_eligible(
            Screen::Chat,
            Some(&e),
            "alice",
            Some(&origin),
            "lobby",
            false
        ));
        assert!(!composition_is_visible(
            Screen::Chat,
            Some(&e),
            "alice",
            Some(&Origin::synthetic("mock", 1))
        ));
        assert!(consume_opening_input(
            "/post title",
            Some("/post title"),
            "lobby",
            "lobby"
        ));
        assert!(!consume_opening_input(
            "ordinary draft",
            None,
            "lobby",
            "lobby"
        ));
        assert!(!consume_opening_input(
            "newer input",
            Some("/post title"),
            "lobby",
            "lobby"
        ));
        assert!(!consume_opening_input(
            "/post title",
            Some("/post title"),
            "lobby",
            "other"
        ));
        e.begin(Operation::Load, "current".into(), 0).unwrap();
        assert!(matching_composition_response(Some(&e), "current"));
        assert!(!matching_composition_response(Some(&e), "old"));
    }
}
