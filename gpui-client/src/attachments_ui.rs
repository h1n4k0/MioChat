//! Native attachment controls. Integration points are in ATTACHMENTS_INTEGRATION.md.
use super::*;
use miochat_gpui::attachments::{
    self, ActionTicket, Attachments, Binding, Download, Info, LocalAction, Operation, Phase,
    Prepared, Purpose, Recovery, RecoveryAction, RecoveryTicket, Selection,
};
use std::sync::Arc;

#[derive(Clone, PartialEq, Eq)]
struct MessageAsset {
    binding: Binding,
    message_id: i64,
    info: Info,
    revision: u64,
}
impl MessageAsset {
    fn matches(&self, m: &Message) -> bool {
        m.id == self.message_id
            && m.room == self.binding.room
            && m.recalled == 0
            && m.attachment.as_ref() == Some(&self.info)
    }
    fn current(&self, binding: Option<&Binding>, revision: u64, messages: &[Message]) -> bool {
        binding == Some(&self.binding)
            && revision == self.revision
            && messages.iter().any(|m| self.matches(m))
    }
    fn changed_by(&self, m: &Message) -> bool {
        m.id == self.message_id && !self.matches(m)
    }
}
#[derive(Default)]
pub(super) struct State {
    pub transfers: Attachments,
    recovery: Option<Recovery>,
    recovery_pending: Option<(String, Instant)>,
    recovery_key: Option<RecoveryTicket>,
    context: Option<Binding>,
    image: Option<Arc<gpui::Image>>,
    retired_image: Option<Arc<gpui::Image>>,
    asset_revision: u64,
    image_message: Option<MessageAsset>,
    download_message: Option<MessageAsset>,
    save_message: Option<MessageAsset>,
    saved_message: Option<MessageAsset>,
    download: Option<Download>,
    download_save: bool,
    download_started: Option<Instant>,
    pending_since: Option<(String, Instant)>,
    next_chunk_at: Option<Instant>,
    next_fetch_at: Option<Instant>,
    saving: bool,
    preparing: bool,
    save_path: Option<std::path::PathBuf>,
    save_cancel: Option<Arc<std::sync::atomic::AtomicBool>>,
}
impl State {
    /// Only ordinary transport Error/Ended may create this hidden slot.
    pub(super) fn capture_transport_recovery(&mut self, credential_uncertain: bool) {
        if credential_uncertain {
            self.freeze(true);
            return;
        }
        if let Some(recovery) = self.transfers.seal_transport_recovery() {
            self.recovery = Some(recovery);
        }
        self.freeze(false);
    }
    pub(super) fn screen_changed(&mut self, screen: Screen) {
        let saved = if matches!(
            screen,
            Screen::Connect | Screen::Trust | Screen::Auth | Screen::Chat
        ) {
            self.recovery.take()
        } else {
            None
        };
        self.freeze(screen != Screen::Chat);
        self.recovery = saved;
    }
    pub(super) fn begin_session(&mut self) {
        let saved = self.recovery.take();
        self.freeze(true);
        self.recovery = saved;
    }
    fn recovery_origin(&mut self, origin: Option<&miochat_gpui::composition::Origin>) {
        if self
            .recovery
            .as_ref()
            .is_some_and(|r| origin.is_some_and(|o| !r.same_origin(o)))
        {
            self.recovery = None;
            self.recovery_pending = None;
            self.recovery_key = None;
        }
    }

    /// No-cx, message-only revocation: preserves the current upload source.
    /// A retired downloaded image is physically evicted by the next cx hook.
    pub(super) fn clear_message_assets(&mut self) {
        self.asset_revision = self.asset_revision.wrapping_add(1);
        if self.image_message.is_some()
            && let Some(image) = self.image.take()
        {
            self.retired_image = Some(image);
        }
        self.image_message = None;
        self.download_message = None;
        self.save_message = None;
        self.saved_message = None;
        self.download = None;
        self.download_started = None;
        self.next_fetch_at = None;
        self.save_path = None;
        if let Some(cancel) = self.save_cancel.take() {
            cancel.store(true, std::sync::atomic::Ordering::Release);
        }
    }
    /// Synchronous privacy barrier usable by set_screen/Origin changes without
    /// GPUI Context. No network operation or visible preview survives. Keep a
    /// single inaccessible image token only until the next asset-release hook;
    /// selecting another source is blocked while that token is outstanding.
    pub(super) fn freeze(&mut self, credentials_changed: bool) {
        if credentials_changed {
            self.recovery = None;
        } else if let Some(r) = self.recovery.as_mut() {
            r.interrupt();
        }
        self.recovery_pending = None;
        self.recovery_key = None;
        self.asset_revision = self.asset_revision.wrapping_add(1);
        self.image_message = None;
        self.download_message = None;
        self.save_message = None;
        self.saved_message = None;
        self.transfers.invalidate(credentials_changed);
        self.pending_since = None;
        self.next_chunk_at = None;
        self.next_fetch_at = None;
        self.download = None;
        self.download_started = None;
        self.save_path = None;
        if let Some(cancel) = self.save_cancel.take() {
            cancel.store(true, std::sync::atomic::Ordering::Release);
        }
        if let Some(image) = self.image.take() {
            self.retired_image = Some(image);
        }
        // preparing/saving remain locked until their one outstanding worker or
        // native dialog returns, preventing an accumulation of 20 MB buffers.
    }
}
fn attachment_claims_response(response: &Response) -> bool {
    response.kind == "attachment"
        || response.attachment.is_some()
        || response.request_id.starts_with("attachment:")
        || response.request_id.starts_with("attachment-fetch:")
}
/// Reject mixed effect envelopes before accepting even a correct upload ACK.
/// A committed response may carry its exact attachment message, but the caller
/// still consumes the whole frame and never projects that optional message.
pub(super) fn attachment_response_exclusive(r: &Response) -> bool {
    r.kind == "attachment"
        && r.attachment.is_some()
        && r.dot.is_none()
        && r.completion.is_none()
        && r.secret_admin.is_none()
        && r.maintenance.is_none()
        && r.composition.is_none()
        && r.configuration_receipt.is_none()
        && r.management_receipt.is_none()
        && r.sanction_receipt.is_none()
        && r.report_receipt.is_none()
        && r.guess_receipt.is_none()
        && r.word_receipt.is_none()
        && r.tic_receipt.is_none()
        && r.community_receipt.is_none()
        && r.direct_target.is_none()
        && !r.clear_view
        && r.mention.is_empty()
        && r.profile.is_none()
        && r.center.is_none()
        && r.preferences.is_none()
        && !r.key_reminder
        && r.device_key.is_none()
        && r.protocol == 0
        && r.registration_mode.is_empty()
        && r.user.is_empty()
        && r.rooms.is_empty()
        && r.messages.is_empty()
        && r.online == 0
        && r.status.is_empty()
        && r.error.is_empty()
        && r.panel.is_none()
        && r.draft.is_none()
        && r.form.is_empty()
        && r.reply_to == 0
        && !r.replace
        && !r.at_latest
        && !r.draft_changed
        && r.capabilities.is_empty()
        && r.message.as_ref().is_none_or(|m| {
            r.attachment.as_ref().is_some_and(|a| {
                a.status == "committed"
                    && m.id == a.message_id
                    && m.id > 0
                    && m.kind == "attachment"
                    && m.room == r.room
                    && m.recalled == 0
                    && m.attachment.is_some()
                    && m.attachment == a.attachment
            })
        })
}
impl MioChatApp {
    pub(super) fn attachment_context_occupied(&self) -> bool {
        self.attachment_ui.recovery.is_some()
            || self.attachment_ui.preparing
            || self.attachment_ui.saving
            || self.attachment_ui.download.is_some()
            || self
                .attachment_ui
                .transfers
                .upload
                .as_ref()
                .is_some_and(|u| {
                    !matches!(
                        u.phase,
                        Phase::Committed | Phase::Cancelled | Phase::Rejected | Phase::Expired
                    )
                })
    }
    pub(super) fn attachment_binding(&self) -> Option<Binding> {
        if self.screen != Screen::Chat
            || self.must_reset
            || self
                .session
                .as_ref()
                .is_none_or(|s| s.generation() != self.generation)
        {
            return None;
        }
        Binding::new(
            self.session_origin.clone()?,
            self.user.clone(),
            self.room.clone(),
            self.generation,
        )
    }
    pub(super) fn attachment_set_capabilities(&mut self, capabilities: &[String]) {
        self.attachment_ui
            .recovery_origin(self.session_origin.as_ref());
        self.attachment_ui.transfers.supported =
            capabilities.iter().any(|v| v == attachments::CAPABILITY);
        self.custom_emoji.reference_supported =
            capabilities.iter().any(|v| v == "attachment-references-v1");
    }
    fn attachment_clear_image(&mut self, cx: &mut Context<Self>) {
        self.attachment_ui.image_message = None;
        if let Some(image) = self.attachment_ui.image.take() {
            image.remove_asset(cx);
        }
    }
    fn attachment_asset_current(&self, asset: &MessageAsset) -> bool {
        asset.current(
            self.attachment_binding().as_ref(),
            self.attachment_ui.asset_revision,
            &self.messages,
        )
    }
    fn request_attachment_fetch_if_current(
        &mut self,
        asset: &MessageAsset,
        save: bool,
        cx: &mut Context<Self>,
    ) {
        if self.attachment_asset_current(asset) {
            self.request_attachment_fetch(asset.message_id, asset.info.clone(), save, cx);
        }
    }
    /// Parent calls this before projecting a recall/metadata replacement in
    /// merge_message. It also invalidates the final in-flight download chunk.
    pub(super) fn attachment_message_changed(&mut self, message: &Message, cx: &mut Context<Self>) {
        let changed = [
            self.attachment_ui.image_message.as_ref(),
            self.attachment_ui.download_message.as_ref(),
            self.attachment_ui.save_message.as_ref(),
            self.attachment_ui.saved_message.as_ref(),
        ]
        .into_iter()
        .flatten()
        .any(|a| a.changed_by(message));
        if changed {
            self.attachment_clear_message_assets(cx);
            self.status =
                "附件已变化或不可见，预览/在途读取已清除；可能已另存的文件不会从磁盘自动删除。"
                    .into();
        }
    }
    /// Parent calls this on permission/visibility reset or a cleared/replaced
    /// message view. Local unsent upload previews are not message assets.
    pub(super) fn attachment_clear_message_assets(&mut self, cx: &mut Context<Self>) {
        self.attachment_ui.clear_message_assets();
        self.attachment_release_assets(cx);
        cx.notify();
    }
    /// Both pointer and keyboard actions must present the captured immutable
    /// upload ticket. Old A controls cannot operate on replacement upload B.
    pub(super) fn attachment_action_if_current(
        &mut self,
        ticket: &ActionTicket,
        cx: &mut Context<Self>,
    ) {
        let Some(binding) = self.attachment_binding() else {
            return;
        };
        if !self.ordinary_chat_context_without_attachment()
            || !self.attachment_ui.transfers.admits_action(ticket, &binding)
        {
            return;
        }
        match ticket.action() {
            LocalAction::Operation(op) => self.attachment_operation(op, cx),
            LocalAction::Resume => {
                if self.attachment_ui.transfers.resume(&binding)
                    && let Some(op) = self.attachment_ui.transfers.next_operation(&binding)
                {
                    self.attachment_operation(op, cx);
                }
            }
            LocalAction::Discard => {
                if self.attachment_ui.transfers.discard_preview() {
                    self.attachment_clear_image(cx);
                }
            }
            LocalAction::ForgetUnknown => {
                if self.attachment_ui.transfers.forget_unknown_local() {
                    self.attachment_clear_image(cx);
                }
            }
        }
        cx.notify();
    }
    fn attachment_ticket_button(
        &self,
        id: &'static str,
        label: &'static str,
        action: LocalAction,
        cx: &mut Context<Self>,
    ) -> impl IntoElement {
        let ticket = self
            .attachment_binding()
            .and_then(|b| self.attachment_ui.transfers.action_ticket(action, &b));
        Self::button(id, label).on_click(cx.listener(move |app, _, _, cx| {
            if let Some(ticket) = ticket.as_ref() {
                app.attachment_action_if_current(ticket, cx);
            }
        }))
    }
    pub(super) fn attachment_invalidate(
        &mut self,
        credentials_changed: bool,
        cx: &mut Context<Self>,
    ) {
        self.attachment_ui.freeze(credentials_changed);
        self.custom_emoji.freeze(credentials_changed);
        self.attachment_release_assets(cx);
    }
    pub(super) fn attachment_release_assets(&mut self, cx: &mut Context<Self>) {
        if let Some(image) = self.attachment_ui.retired_image.take() {
            image.remove_asset(cx);
        }
    }
    /// Call each poll, and invalidate explicitly before any credential transition.
    pub(super) fn attachment_tick(&mut self, cx: &mut Context<Self>) {
        self.attachment_release_assets(cx);
        let now = self.attachment_binding();
        if self
            .attachment_ui
            .recovery
            .as_ref()
            .is_some_and(|r| now.as_ref().is_some_and(|b| !r.same_owner_origin(b)))
        {
            self.attachment_ui.recovery = None;
            self.attachment_ui.recovery_pending = None;
            self.attachment_ui.recovery_key = None;
        }
        if self
            .attachment_ui
            .recovery_pending
            .as_ref()
            .is_some_and(|(_, at)| at.elapsed() > Duration::from_secs(20))
            && let Some((id, _)) = self.attachment_ui.recovery_pending.take()
            && let Some(r) = self.attachment_ui.recovery.as_mut()
        {
            r.transport_uncertain(&id);
            self.attachment_ui.recovery_key = None;
            cx.notify();
        }
        if self.attachment_ui.context != now {
            let force = self
                .attachment_ui
                .context
                .as_ref()
                .zip(now.as_ref())
                .is_some_and(|(a, b)| a.owner != b.owner || a.origin != b.origin);
            self.attachment_invalidate(force, cx);
            self.attachment_ui.context = now;
        }
        let invalid_asset = [
            self.attachment_ui.image_message.as_ref(),
            self.attachment_ui.download_message.as_ref(),
            self.attachment_ui.save_message.as_ref(),
            self.attachment_ui.saved_message.as_ref(),
        ]
        .into_iter()
        .flatten()
        .any(|a| !self.attachment_asset_current(a));
        if invalid_asset {
            self.attachment_clear_message_assets(cx);
        }
        if self
            .attachment_ui
            .pending_since
            .as_ref()
            .is_some_and(|(_, at)| at.elapsed() > Duration::from_secs(20))
            && let Some((id, _)) = self.attachment_ui.pending_since.take()
        {
            self.attachment_ui.transfers.transport_uncertain(&id);
            cx.notify();
        }
        if self
            .attachment_ui
            .download_started
            .is_some_and(|at| at.elapsed() > Duration::from_secs(20))
        {
            self.attachment_ui.download = None;
            self.attachment_ui.download_started = None;
            self.error = Some("附件下载超时；没有保存或显示文件，请明确重新请求。".into());
            cx.notify();
        }
        // Server allows 32 attachment frames/s; schedule after ACK instead of
        // creating an unbounded local burst on a fast connection.
        if self
            .attachment_ui
            .next_chunk_at
            .is_some_and(|at| Instant::now() >= at)
        {
            self.attachment_ui.next_chunk_at = None;
            if let Some(binding) = self.attachment_binding()
                && let Some(op) = self.attachment_ui.transfers.next_operation(&binding)
            {
                self.attachment_operation(op, cx);
            }
        }
        if self
            .attachment_ui
            .next_fetch_at
            .is_some_and(|at| Instant::now() >= at)
        {
            self.attachment_ui.next_fetch_at = None;
            if self.attachment_ui.download.is_some() {
                self.queue_attachment_fetch(cx);
            }
        }
    }
    fn attachment_can_choose(&self) -> bool {
        self.attachment_ui.recovery.is_none()
            && !self.custom_emoji_context_occupied()
            && self.ordinary_chat_context_without_attachment()
            && self.attachment_binding().is_some()
            && !self.persistence.quarantined()
            && (!self.persistence_enabled || self.persistence.ready())
            && self.attachment_ui.retired_image.is_none()
            && !self.attachment_ui.preparing
            && !self.attachment_ui.saving
            && self.attachment_ui.download.is_none()
    }
    fn attachment_selection(
        &mut self,
        purpose: Purpose,
        cx: &mut Context<Self>,
    ) -> Option<Selection> {
        if !self.attachment_can_choose() {
            self.error = Some("请先完成当前操作，再选择附件。".into());
            cx.notify();
            return None;
        }
        let binding = self.attachment_binding()?;
        match self
            .attachment_ui
            .transfers
            .begin_selection(binding.clone(), purpose)
        {
            Ok(ticket) => {
                self.attachment_ui.preparing = true;
                self.attachment_ui.context = Some(binding);
                self.attachment_clear_image(cx);
                Some(ticket)
            }
            Err(e) => {
                self.error = Some(e.into());
                cx.notify();
                None
            }
        }
    }
    pub(super) fn choose_attachment(&mut self, purpose: Purpose, cx: &mut Context<Self>) {
        let Some(ticket) = self.attachment_selection(purpose, cx) else {
            return;
        };
        let selection = cx.prompt_for_paths(gpui::PathPromptOptions {
            files: true,
            directories: false,
            multiple: false,
            prompt: Some("选择附件；预览确认前不会上传".into()),
        });
        cx.spawn(async move |this, cx| {
            let selected = selection.await;
            let path = match selected {
                Ok(Ok(Some(paths))) if paths.len() == 1 => Some(paths[0].clone()),
                _ => None,
            };
            let Some(path) = path else {
                let _ = this.update(cx, |app, cx| {
                    app.attachment_ui.preparing = false;
                    if let Some(binding) = app.attachment_binding() {
                        app.attachment_ui
                            .transfers
                            .cancel_selection_if_current(&ticket, &binding);
                    }
                    cx.notify();
                });
                return;
            };
            let current = this
                .update(cx, |app, _| {
                    app.ordinary_chat_context_without_attachment()
                        && app.attachment_binding().is_some_and(|b| {
                            app.attachment_ui.transfers.selection_matches(&ticket, &b)
                        })
                })
                .unwrap_or(false);
            if !current {
                let _ = this.update(cx, |app, cx| {
                    app.attachment_ui.preparing = false;
                    cx.notify();
                });
                return;
            }
            let result = cx
                .background_executor()
                .spawn(async move { attachments::read_selected_file(&path) })
                .await;
            let _ = this.update(cx, |app, cx| {
                app.finish_attachment_selection(&ticket, result, cx)
            });
        })
        .detach();
    }
    /// Clipboard image format+encoded bytes come directly from GPUI 0.3.7;
    /// paths/text/URLs are never interpreted as attachment sources.
    pub(super) fn paste_attachment_image(&mut self, purpose: Purpose, cx: &mut Context<Self>) {
        let Some(ticket) = self.attachment_selection(purpose, cx) else {
            return;
        };
        let image = cx.read_from_clipboard().and_then(|item| {
            item.entries.into_iter().find_map(|e| match e {
                gpui::ClipboardEntry::Image(image) => Some(image),
                _ => None,
            })
        });
        let Some(image) = image else {
            self.finish_attachment_selection(
                &ticket,
                Err("剪贴板没有受支持的图片；文本和路径不会上传"),
                cx,
            );
            return;
        };
        let filename = match image.format {
            gpui::ImageFormat::Png => "clipboard.png",
            gpui::ImageFormat::Jpeg => "clipboard.jpg",
            gpui::ImageFormat::Gif => "clipboard.gif",
            _ => {
                self.finish_attachment_selection(
                    &ticket,
                    Err("剪贴板图片仅支持 PNG、JPEG、GIF"),
                    cx,
                );
                return;
            }
        };
        if image.bytes.len() > attachments::ITEM_LIMIT {
            self.finish_attachment_selection(&ticket, Err("剪贴板图片超过 20,000,000 字节"), cx);
            return;
        }
        cx.spawn(async move |this, cx| {
            let prepared = cx
                .background_executor()
                .spawn(async move { Prepared::from_bytes(filename.into(), image.bytes) })
                .await;
            let _ = this.update(cx, |app, cx| {
                app.finish_attachment_selection(&ticket, prepared, cx)
            });
        })
        .detach();
    }
    fn finish_attachment_selection(
        &mut self,
        ticket: &Selection,
        result: Result<Prepared, &'static str>,
        cx: &mut Context<Self>,
    ) {
        self.attachment_ui.preparing = false;
        let Some(binding) = self.attachment_binding() else {
            return;
        };
        if !self
            .attachment_ui
            .transfers
            .selection_matches(ticket, &binding)
        {
            return;
        }
        if !self.ordinary_chat_context_without_attachment() || self.persistence.quarantined() {
            self.attachment_ui
                .transfers
                .cancel_selection_if_current(ticket, &binding);
            cx.notify();
            return;
        }
        match result {
            Ok(source) => {
                let key = format!(
                    "att_{:x}_{:x}_{:x}",
                    std::time::SystemTime::now()
                        .duration_since(std::time::UNIX_EPOCH)
                        .map(|v| v.as_nanos())
                        .unwrap_or(0),
                    self.generation,
                    self.request_counter
                );
                self.request_counter = self.request_counter.wrapping_add(1);
                if let Err(e) = self
                    .attachment_ui
                    .transfers
                    .finish_selection(ticket, &binding, source, key)
                {
                    self.error = Some(e.into());
                } else {
                    let image = self
                        .attachment_ui
                        .transfers
                        .upload
                        .as_ref()
                        .and_then(|u| u.source())
                        .and_then(|s| image_from_checked_bytes(&s.info, s.bytes().to_vec()));
                    self.attachment_clear_image(cx);
                    self.attachment_ui.image = image;
                    self.error = None;
                }
            }
            Err(e) => {
                self.attachment_ui.transfers.cancel_selection();
                self.error = Some(e.into());
            }
        }
        cx.notify();
    }
    pub(super) fn attachment_operation(&mut self, op: Operation, cx: &mut Context<Self>) {
        let Some(binding) = self.attachment_binding() else {
            return;
        };
        let id = self.next_request_id("attachment");
        match self.attachment_ui.transfers.begin(op, id.clone(), &binding) {
            Ok(data) => {
                let mut req = Request::simple("attachment", &id);
                req.room = &binding.room;
                req.attachment = Some(&data);
                self.attachment_ui.pending_since = Some((id.clone(), Instant::now()));
                if self.send_request(&req).is_err() {
                    self.attachment_ui.transfers.transport_uncertain(&id);
                    self.attachment_ui.pending_since = None;
                }
            }
            Err(e) => self.error = Some(e.into()),
        }
        cx.notify();
    }
    /// Return true to consume the ENTIRE response. Never project incidental
    /// messages/navigation from an attachment response, valid or malformed.
    pub(super) fn attachment_response(
        &mut self,
        response: &Response,
        cx: &mut Context<Self>,
    ) -> bool {
        if self.attachment_recovery_response(response, cx) {
            return true;
        }
        if self.custom_emoji_response(response, cx) {
            return true;
        }
        let binding = self.attachment_binding();
        let relevant = attachment_claims_response(response)
            || binding.as_ref().is_some_and(|b| {
                self.attachment_ui
                    .transfers
                    .pending_matches(&response.request_id, b)
            })
            || self
                .attachment_ui
                .download
                .as_ref()
                .is_some_and(|d| d.pending_matches(&response.request_id));
        if !relevant {
            return false;
        }
        if !attachment_response_exclusive(response) {
            return true;
        }
        let Some(binding) = binding else {
            return true;
        };
        if self
            .attachment_ui
            .download
            .as_ref()
            .is_some_and(|d| d.pending_matches(&response.request_id))
            && !self
                .attachment_ui
                .download_message
                .as_ref()
                .is_some_and(|a| self.attachment_asset_current(a))
        {
            self.attachment_clear_message_assets(cx);
            return true;
        }
        if let Some(download) = self.attachment_ui.download.as_mut()
            && download.pending_matches(&response.request_id)
        {
            let result = if response.kind == "attachment" && response.room == binding.room {
                response
                    .attachment
                    .as_ref()
                    .ok_or("下载缺少回执")
                    .and_then(|r| download.accept(&response.request_id, &binding, r))
            } else {
                Err("下载响应不匹配")
            };
            match result {
                Ok(true) => {
                    self.attachment_ui.download_started = None;
                    if download.complete {
                        let info = download.info.clone();
                        let asset = self.attachment_ui.download_message.take();
                        let bytes = download.take_bytes().unwrap_or_default();
                        self.attachment_ui.download = None;
                        if self.attachment_ui.download_save {
                            if let Some(asset) = asset {
                                self.save_attachment_bytes(asset, bytes, cx);
                            }
                        } else {
                            self.attachment_clear_image(cx);
                            self.attachment_ui.image = image_from_checked_bytes(&info, bytes);
                            self.attachment_ui.image_message = asset;
                            self.status = "附件已按当前权限读取；预览不执行文件。".into();
                        }
                    } else {
                        self.attachment_ui.next_fetch_at =
                            Some(Instant::now() + Duration::from_millis(35));
                    }
                }
                Ok(false) => {}
                Err(e) => {
                    self.attachment_ui.download = None;
                    self.attachment_ui.download_started = None;
                    self.error = Some(e.into());
                }
            }
            cx.notify();
            return true;
        }
        if !self
            .attachment_ui
            .transfers
            .pending_matches(&response.request_id, &binding)
        {
            return true;
        }
        if response.kind != "attachment"
            || response.room != binding.room
            || response.attachment.is_none()
        {
            self.attachment_ui
                .transfers
                .transport_uncertain(&response.request_id);
            self.attachment_ui.pending_since = None;
            cx.notify();
            return true;
        }
        let accepted = self.attachment_ui.transfers.accept(
            &response.request_id,
            &binding,
            response.attachment.as_ref().unwrap(),
        );
        if accepted {
            self.remember_custom_emoji_upload(response.attachment.as_ref().unwrap());
            self.attachment_ui.pending_since = None;
            if self
                .attachment_ui
                .transfers
                .upload
                .as_ref()
                .is_some_and(|u| u.terminal())
            {
                self.attachment_clear_image(cx);
            }
            if self
                .attachment_ui
                .transfers
                .next_operation(&binding)
                .is_some()
            {
                self.attachment_ui.next_chunk_at = Some(Instant::now() + Duration::from_millis(35));
            }
        }
        cx.notify();
        true
    }
    pub(super) fn request_attachment_fetch(
        &mut self,
        message_id: i64,
        info: Info,
        save: bool,
        cx: &mut Context<Self>,
    ) {
        if self.custom_emoji_context_occupied()
            || !self.ordinary_chat_context_without_attachment()
            || self.persistence.quarantined()
            || self.attachment_ui.preparing
            || self.attachment_ui.retired_image.is_some()
            || !self.attachment_ui.transfers.supported
            || self.attachment_ui.download.is_some()
            || self.attachment_ui.saving
            || self
                .attachment_ui
                .transfers
                .upload
                .as_ref()
                .is_some_and(|u| !u.terminal())
        {
            return;
        }
        let Some(binding) = self.attachment_binding() else {
            return;
        };
        // Current visible metadata only; never fetch a guessed opaque ID or URL.
        if !self
            .messages
            .iter()
            .any(|m| m.id == message_id && m.recalled == 0 && m.attachment.as_ref() == Some(&info))
        {
            return;
        }
        if save {
            let asset = MessageAsset {
                binding: binding.clone(),
                message_id,
                info: info.clone(),
                revision: self.attachment_ui.asset_revision,
            };
            self.attachment_ui.save_message = Some(asset.clone());
            self.attachment_ui.saving = true;
            let prompt = cx.prompt_for_new_path(std::path::Path::new("."), Some(&info.filename));
            cx.spawn(async move |this, cx| {
                let selected = prompt.await;
                let _ = this.update(cx, |app, cx| {
                    app.attachment_ui.saving = false;
                    if !app.attachment_asset_current(&asset)
                        || app.attachment_ui.save_message.as_ref() != Some(&asset)
                    {
                        return;
                    }
                    if let Ok(Ok(Some(path))) = selected {
                        app.attachment_ui.save_path = Some(path);
                        app.start_attachment_fetch(binding, message_id, info, true, cx);
                    } else {
                        app.attachment_ui.save_message = None;
                    }
                    cx.notify();
                });
            })
            .detach();
        } else {
            self.start_attachment_fetch(binding, message_id, info, false, cx);
        }
    }
    fn start_attachment_fetch(
        &mut self,
        binding: Binding,
        message_id: i64,
        info: Info,
        save: bool,
        cx: &mut Context<Self>,
    ) {
        let asset = MessageAsset {
            binding: binding.clone(),
            message_id,
            info: info.clone(),
            revision: self.attachment_ui.asset_revision,
        };
        if !self.attachment_asset_current(&asset)
            || (save && self.attachment_ui.save_message.as_ref() != Some(&asset))
            || self.attachment_ui.preparing
            || self.attachment_ui.download.is_some()
            || self
                .attachment_ui
                .transfers
                .upload
                .as_ref()
                .is_some_and(|u| !u.terminal())
        {
            self.attachment_ui.save_path = None;
            return;
        }
        match Download::new(binding, message_id, info) {
            Ok(d) => {
                self.attachment_ui.download = Some(d);
                self.attachment_ui.download_message = Some(asset);
                self.attachment_ui.download_save = save;
                self.queue_attachment_fetch(cx);
            }
            Err(e) => self.error = Some(e.into()),
        }
        cx.notify();
    }
    fn queue_attachment_fetch(&mut self, cx: &mut Context<Self>) {
        let Some(binding) = self.attachment_binding() else {
            return;
        };
        let id = self.next_request_id("attachment-fetch");
        let data = self
            .attachment_ui
            .download
            .as_mut()
            .and_then(|d| d.begin(id.clone(), &binding));
        if let Some(data) = data {
            let mut req = Request::simple("attachment", &id);
            req.room = &binding.room;
            req.attachment = Some(&data);
            if self.send_request(&req).is_ok() {
                self.attachment_ui.download_started = Some(Instant::now());
            } else {
                self.attachment_ui.download = None;
                self.error = Some("下载请求未完成；没有保存文件。".into());
            }
        }
        cx.notify();
    }
    fn save_attachment_bytes(
        &mut self,
        asset: MessageAsset,
        bytes: Vec<u8>,
        cx: &mut Context<Self>,
    ) {
        let Some(path) = self.attachment_ui.save_path.take() else {
            return;
        };
        if !self.attachment_asset_current(&asset)
            || self.attachment_ui.save_message.as_ref() != Some(&asset)
        {
            return;
        }
        let cancel = Arc::new(std::sync::atomic::AtomicBool::new(false));
        self.attachment_ui.save_cancel = Some(cancel.clone());
        self.attachment_ui.saving = true;
        cx.spawn(async move|this,cx|{
            let result=cx.background_executor().spawn(async move{write_cancellable_download(&path,&bytes,&cancel)}).await;
            let _=this.update(cx,|app,cx|{
                app.attachment_ui.saving=false;
                if app.attachment_asset_current(&asset)&&app.attachment_ui.save_message.as_ref()==Some(&asset){
                    app.attachment_ui.save_cancel=None;
                    match result{Ok(())=>{app.attachment_ui.saved_message=Some(asset);app.attachment_ui.save_message=None;app.status="已保存到你选择的新文件；没有自动打开或执行。此文件不会因消息撤回自动删除。".into();},Err(e)=>app.error=Some(e.into())}
                }cx.notify();
            });
        }).detach();
    }
    pub(super) fn attachment_controls(&self, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .flex()
            .gap(px(8.))
            .flex_wrap()
            .child(Self::button("attachment-choose", "附件").on_click(
                cx.listener(|app, _, _, cx| app.choose_attachment(Purpose::Attachment, cx)),
            ))
            .child(Self::button("attachment-paste", "粘贴图片").on_click(
                cx.listener(|app, _, _, cx| app.paste_attachment_image(Purpose::Attachment, cx)),
            ))
            .child(Self::button("attachment-emoji", "自定义图片表情").on_click(
                cx.listener(|app, _, _, cx| app.choose_attachment(Purpose::CustomEmoji, cx)),
            ))
            .when(!self.attachment_ui.transfers.supported, |d| {
                d.child("此服务器不支持附件")
            })
    }
    fn attachment_recovery_action(&mut self, ticket: &RecoveryTicket, cx: &mut Context<Self>) {
        let Some(binding) = self.attachment_binding() else {
            return;
        };
        if !self.ordinary_chat_context_without_attachment()
            || !self
                .attachment_ui
                .recovery
                .as_ref()
                .is_some_and(|r| r.admits(ticket, &binding))
        {
            return;
        }
        self.attachment_ui.recovery_key = None;
        match ticket.action() {
            RecoveryAction::Forget => {
                self.attachment_ui.recovery = None;
                self.attachment_ui.transfers.notice =
                    Some("仅清除了本地记录；不代表服务器取消或撤回，不要据此重复发送。".into());
            }
            RecoveryAction::Status => {
                if !self.attachment_ui.transfers.supported {
                    return;
                }
                let id = self.next_request_id("attachment-recovery");
                let result = self.attachment_ui.recovery.as_mut().unwrap().begin_status(
                    ticket,
                    id.clone(),
                    &binding,
                );
                match result {
                    Ok(data) => {
                        let mut req = Request::simple("attachment", &id);
                        req.room = &binding.room;
                        req.attachment = Some(&data);
                        self.attachment_ui.recovery_pending = Some((id.clone(), Instant::now()));
                        if self.send_request(&req).is_err() {
                            self.attachment_ui
                                .recovery
                                .as_mut()
                                .unwrap()
                                .transport_uncertain(&id);
                            self.attachment_ui.recovery_pending = None;
                        }
                    }
                    Err(e) => self.error = Some(e.into()),
                }
            }
        }
        cx.notify();
    }
    fn attachment_recovery_response(
        &mut self,
        response: &Response,
        cx: &mut Context<Self>,
    ) -> bool {
        if !response.request_id.starts_with("attachment-recovery:") {
            return false;
        }
        // Claim even expired/error IDs before any global response effects.
        if !attachment_response_exclusive(response) {
            return true;
        }
        let Some(binding) = self.attachment_binding() else {
            return true;
        };
        if response.room != binding.room {
            return true;
        }
        if let Some(r) = self.attachment_ui.recovery.as_mut()
            && let Some(receipt) = response.attachment.as_ref()
            && r.accept(&response.request_id, &binding, receipt)
        {
            let verified = r.take_verified_upload();
            self.attachment_ui.recovery_pending = None;
            self.attachment_ui.recovery_key = None;
            if let Some(upload) = verified {
                self.attachment_ui.recovery = None;
                self.attachment_ui.transfers.upload = Some(upload);
            }
            cx.notify();
        }
        true
    }
    fn render_attachment_recovery(&self, cx: &mut Context<Self>) -> Div {
        let Some(binding) = self.attachment_binding() else {
            return div();
        };
        let Some(r) = self
            .attachment_ui
            .recovery
            .as_ref()
            .filter(|r| r.same_owner_origin(&binding))
        else {
            return div();
        };
        let status = r
            .ticket(RecoveryAction::Status, &binding)
            .filter(|_| self.attachment_ui.transfers.supported);
        let forget = r.ticket(RecoveryAction::Forget, &binding);
        let selected = self
            .attachment_ui
            .recovery_key
            .as_ref()
            .filter(|t| r.admits(t, &binding))
            .map(|t| t.action());
        div()
            .flex()
            .flex_col()
            .gap(px(6.))
            .child(r.notice())
            .child("仅原目标可查询。Tab 选择操作，Enter 执行明确选中项；Escape 不取消服务器操作。")
            .when_some(status, |d, ticket| {
                d.child(
                    Self::button(
                        "attachment-recovery-status",
                        if selected == Some(RecoveryAction::Status) {
                            "▶ 查询原上传"
                        } else {
                            "查询原上传"
                        },
                    )
                    .on_click(
                        cx.listener(move |app, _, _, cx| {
                            app.attachment_recovery_action(&ticket, cx)
                        }),
                    ),
                )
            })
            .when_some(forget, |d, ticket| {
                d.child(
                    Self::button(
                        "attachment-recovery-forget",
                        if selected == Some(RecoveryAction::Forget) {
                            "▶ 仅清本地记录"
                        } else {
                            "仅清本地记录"
                        },
                    )
                    .on_click(
                        cx.listener(move |app, _, _, cx| {
                            app.attachment_recovery_action(&ticket, cx)
                        }),
                    ),
                )
            })
    }
    /// Install before attachment_key; no default action is armed.
    pub(super) fn attachment_recovery_key(
        &mut self,
        event: &KeyDownEvent,
        cx: &mut Context<Self>,
    ) -> bool {
        if !self.ordinary_chat_context_without_attachment() {
            return false;
        }
        let Some(binding) = self.attachment_binding() else {
            return false;
        };
        let Some(r) = self
            .attachment_ui
            .recovery
            .as_ref()
            .filter(|r| r.same_owner_origin(&binding))
        else {
            return false;
        };
        let key = event.keystroke.key.as_str();
        let m = &event.keystroke.modifiers;
        if !matches!(key, "tab" | "enter" | "escape")
            || m.control
            || m.alt
            || m.platform
            || m.function
        {
            return false;
        }
        if event.is_held || event.prefer_character_input || self.marked_range.is_some() {
            return true;
        }
        let actions: Vec<_> = [RecoveryAction::Status, RecoveryAction::Forget]
            .into_iter()
            .filter(|a| *a != RecoveryAction::Status || self.attachment_ui.transfers.supported)
            .filter_map(|a| r.ticket(a, &binding))
            .collect();
        let selected = self
            .attachment_ui
            .recovery_key
            .as_ref()
            .filter(|t| r.admits(t, &binding))
            .map(|t| t.action());
        if key == "tab" && !actions.is_empty() {
            let current = actions.iter().position(|t| Some(t.action()) == selected);
            let index = if m.shift {
                current.map_or(actions.len() - 1, |i| {
                    (i + actions.len() - 1) % actions.len()
                })
            } else {
                current.map_or(0, |i| (i + 1) % actions.len())
            };
            self.attachment_ui.recovery_key = Some(actions[index].clone());
        } else if key == "enter"
            && let Some(ticket) = self.attachment_ui.recovery_key.clone()
        {
            self.attachment_recovery_action(&ticket, cx);
        } else if key == "escape" {
            self.attachment_ui.recovery_key = None;
        }
        cx.notify();
        true
    }
    pub(super) fn render_attachment_preview(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let binding = self.attachment_binding();
        let upload = self.attachment_ui.transfers.upload.as_ref().filter(|u| {
            binding
                .as_ref()
                .is_some_and(|b| u.binding.same_authority(b))
        });
        div()
            .flex()
            .flex_col()
            .gap(px(8.))
            .p(px(10.))
            .child(self.render_attachment_recovery(cx))
            .when_some(self.attachment_ui.transfers.notice.clone(), |d, n| {
                d.child(n)
            })
            .when_some(self.attachment_ui.image.clone(), |d, image| {
                d.child(
                    gpui::img(image)
                        .w(px(280.))
                        .h(px(180.))
                        .object_fit(gpui::ObjectFit::Contain),
                )
            })
            .when_some(upload, |d, u| {
                let phase = u.phase;
                let pending = u.pending();
                d.child(format!(
                    "目标：{}  ·  账户：{}",
                    u.binding.room, u.binding.owner
                ))
                .child(format!(
                    "{} · {} · {} 字节 · 已接收 {}",
                    u.info.filename, u.info.mime, u.info.size, u.accepted
                ))
                .child(format!("SHA-256：{}", u.info.sha256))
                .child(attachments::DISCLOSURE)
                .child(u.notice.clone())
                .when_some(u.quota.clone(), |d, q| {
                    d.child(format!(
                        "服务器当日额度：已用 {}，预留 {}，剩余 {} 字节；重置 Unix 时间 {}",
                        q.used,
                        q.reserved,
                        q.remaining(),
                        q.reset_at
                    ))
                })
                .when(!pending && phase == Phase::Preview, |d| {
                    d.child(self.attachment_ticket_button(
                        "attachment-confirm-intent",
                        "确认目标，开始上传（稍后确认发布）",
                        LocalAction::Operation(Operation::Intent),
                        cx,
                    ))
                })
                .when(!pending && phase == Phase::ReadyToCommit, |d| {
                    d.child(self.attachment_ticket_button(
                        "attachment-confirm-commit",
                        "确认发布到上述目标",
                        LocalAction::Operation(Operation::Commit),
                        cx,
                    ))
                })
                .when(
                    !pending && matches!(phase, Phase::Unknown | Phase::Paused),
                    |d| {
                        d.child(self.attachment_ticket_button(
                            "attachment-status",
                            "查询原上传状态",
                            LocalAction::Operation(Operation::Status),
                            cx,
                        ))
                    },
                )
                .when(
                    !pending && binding.as_ref().is_some_and(|b| u.can_resume(b)),
                    |d| {
                        d.child(self.attachment_ticket_button(
                            "attachment-resume",
                            "继续原上传",
                            LocalAction::Resume,
                            cx,
                        ))
                    },
                )
                .when(
                    !pending && !u.upload_id.is_empty() && !u.terminal() && phase != Phase::Preview,
                    |d| {
                        d.child(self.attachment_ticket_button(
                            "attachment-cancel",
                            "取消未发布上传（不能撤回已发布）",
                            LocalAction::Operation(Operation::Cancel),
                            cx,
                        ))
                    },
                )
                .when(!pending && (phase == Phase::Preview || u.terminal()), |d| {
                    d.child(self.attachment_ticket_button(
                        "attachment-dismiss",
                        "清除本地预览",
                        LocalAction::Discard,
                        cx,
                    ))
                })
                .when(!pending && phase == Phase::Unknown, |d| {
                    d.child(self.attachment_ticket_button(
                        "attachment-forget-unknown",
                        "仅清本地记录（不代表取消/回滚，不自动重试）",
                        LocalAction::ForgetUnknown,
                        cx,
                    ))
                })
                .when(pending, |d| d.child("等待服务器确认…"))
            })
    }
    fn attachment_message_button(id: String, label: &'static str) -> Stateful<Div> {
        div()
            .id(id)
            .role(gpui::Role::Button)
            .aria_label(label)
            .p(px(8.))
            .rounded(px(6.))
            .bg(rgb(0x292d32))
            .text_color(rgb(0xfdfcf9))
            .cursor_pointer()
            .child(label)
    }
    pub(super) fn render_message_attachment(
        &self,
        message: &Message,
        cx: &mut Context<Self>,
    ) -> Div {
        let id = message.id;
        div().flex().flex_col().gap(px(6.)).when_some(
            message.attachment.clone().filter(|_| message.recalled == 0),
            |d, info| {
                let asset = self.attachment_binding().map(|binding| MessageAsset {
                    binding,
                    message_id: id,
                    info: info.clone(),
                    revision: self.attachment_ui.asset_revision,
                });
                let preview = asset.clone();
                let save = asset;
                d.child(format!(
                    "附件：{} · {} 字节 · ID {}",
                    info.filename, info.size, info.id
                ))
                .when(info.mime.starts_with("image/"), |d| {
                    d.child(
                        Self::attachment_message_button(
                            format!("attachment-view-{id}"),
                            "按当前权限加载图片",
                        )
                        .on_click(cx.listener(move |app, _, _, cx| {
                            if let Some(asset) = preview.as_ref() {
                                app.request_attachment_fetch_if_current(asset, false, cx);
                            }
                        })),
                    )
                })
                .child(
                    Self::attachment_message_button(
                        format!("attachment-save-{id}"),
                        "下载并另存为…",
                    )
                    .on_click(cx.listener(move |app, _, _, cx| {
                        if let Some(asset) = save.as_ref() {
                            app.request_attachment_fetch_if_current(asset, true, cx);
                        }
                    })),
                )
            },
        )
    }
}
fn image_from_checked_bytes(info: &Info, bytes: Vec<u8>) -> Option<Arc<gpui::Image>> {
    if attachments::image_budget(&info.mime, &bytes).is_err() {
        return None;
    }
    let format = match info.mime.as_str() {
        "image/png" => gpui::ImageFormat::Png,
        "image/jpeg" => gpui::ImageFormat::Jpeg,
        "image/gif" => gpui::ImageFormat::Gif,
        _ => return None,
    };
    Some(Arc::new(gpui::Image::from_bytes(format, bytes)))
}
#[cfg(test)]
fn write_new_download(path: &std::path::Path, bytes: &[u8]) -> Result<(), &'static str> {
    write_cancellable_download(path, bytes, &std::sync::atomic::AtomicBool::new(false))
}
fn write_cancellable_download(
    path: &std::path::Path,
    bytes: &[u8],
    cancel: &std::sync::atomic::AtomicBool,
) -> Result<(), &'static str> {
    use std::io::Write;
    if bytes.len() > attachments::ITEM_LIMIT || cancel.load(std::sync::atomic::Ordering::Acquire) {
        return Err("保存已取消");
    }
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(path)
        .map_err(|_| "无法创建新文件（已有文件不会覆盖）")?;
    for chunk in bytes.chunks(attachments::CHUNK_BYTES) {
        if cancel.load(std::sync::atomic::Ordering::Acquire) {
            let _ = file.set_len(0);
            return Err("范围变化，保存已取消；可能留下空文件");
        }
        file.write_all(chunk)
            .map_err(|_| "保存失败；目标可能是不完整文件，请检查")?;
    }
    if cancel.load(std::sync::atomic::Ordering::Acquire) {
        let _ = file.set_len(0);
        return Err("保存已取消；可能留下空文件");
    }
    file.sync_all()
        .map_err(|_| "保存失败；目标可能是不完整文件，请检查")
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn stale_attachment_errors_are_claimed_and_mixed_acks_rejected() {
        for id in ["attachment:1:4", "attachment-fetch:1:9"] {
            let r = Response {
                kind: "error".into(),
                request_id: id.into(),
                error: "old".into(),
                replace: true,
                ..Default::default()
            };
            assert!(attachment_claims_response(&r));
            assert!(!attachment_response_exclusive(&r));
        }
        let mut r = Response {
            kind: "attachment".into(),
            request_id: "attachment:1:4".into(),
            room: "lobby".into(),
            attachment: Some(attachments::Receipt::default()),
            ..Default::default()
        };
        assert!(attachment_response_exclusive(&r));
        r.replace = true;
        assert!(!attachment_response_exclusive(&r));
        r.replace = false;
        r.profile = Some(Default::default());
        assert!(!attachment_response_exclusive(&r));
        r.profile = None;
        r.community_receipt = Some(miochat_gpui::community_mutation::CommunityReceipt {
            command: "/poll".into(),
            object_id: 4,
            room: "lobby".into(),
            origin_scope: "lobby".into(),
            committed: true,
        });
        assert!(!attachment_response_exclusive(&r));
        r.community_receipt = None;
        r.center = Some(Default::default());
        assert!(!attachment_response_exclusive(&r));
        r.center = None;
        r.preferences = Some(Default::default());
        assert!(!attachment_response_exclusive(&r));
        r.preferences = None;
        r.completion = Some(miochat_gpui::completion::Receipt {
            kind: miochat_gpui::completion::Kind::User,
            prefix: "a".into(),
            choices: vec![],
        });
        assert!(!attachment_response_exclusive(&r));
    }
    #[test]
    fn no_context_freeze_discards_local_preview_and_revokes_worker() {
        let binding = Binding::new(
            miochat_gpui::composition::Origin::synthetic("test", 1),
            "owner".into(),
            "lobby".into(),
            1,
        )
        .unwrap();
        let mut state = State::default();
        state.transfers.supported = true;
        let ticket = state
            .transfers
            .begin_selection(binding.clone(), Purpose::Attachment)
            .unwrap();
        state
            .transfers
            .finish_selection(
                &ticket,
                &binding,
                Prepared::from_bytes("a.txt".into(), b"secret".to_vec()).unwrap(),
                "intent_test_123".into(),
            )
            .unwrap();
        let cancel = Arc::new(std::sync::atomic::AtomicBool::new(false));
        state.save_cancel = Some(cancel.clone());
        state.freeze(true);
        assert!(state.transfers.upload.is_none());
        assert!(cancel.load(std::sync::atomic::Ordering::Acquire));
        assert!(state.pending_since.is_none());
    }
    fn asset_fixture() -> (Binding, Info, MessageAsset, Message) {
        let binding = Binding::new(
            miochat_gpui::composition::Origin::synthetic("server", 1),
            "owner".into(),
            "lobby".into(),
            7,
        )
        .unwrap();
        let info = Info {
            id: "a".repeat(32),
            filename: "image.png".into(),
            mime: "image/png".into(),
            size: 3,
            sha256: "a".repeat(64),
            width: 1,
            height: 1,
            frames: 1,
            metadata_policy: "preserve_original".into(),
        };
        let asset = MessageAsset {
            binding: binding.clone(),
            message_id: 4,
            info: info.clone(),
            revision: 0,
        };
        let message = Message {
            id: 4,
            room: "lobby".into(),
            attachment: Some(info.clone()),
            ..Default::default()
        };
        (binding, info, asset, message)
    }
    #[test]
    fn recall_metadata_and_same_generation_scope_roundtrip_invalidate_download_tickets() {
        let (binding, _, asset, mut message) = asset_fixture();
        assert!(asset.current(Some(&binding), 0, &[message.clone()]));
        message.recalled = 1;
        assert!(asset.changed_by(&message));
        assert!(!asset.current(Some(&binding), 0, &[message.clone()]));
        message.recalled = 0;
        message.attachment.as_mut().unwrap().sha256 = "b".repeat(64);
        assert!(asset.changed_by(&message));
        let (_, _, _, original) = asset_fixture();
        assert!(!asset.current(Some(&binding), 1, &[original]));
    }
    #[test]
    fn message_only_clear_drops_final_chunk_and_save_but_preserves_upload_source() {
        let (binding, info, asset, _) = asset_fixture();
        let mut state = State::default();
        state.transfers.supported = true;
        let selection = state
            .transfers
            .begin_selection(binding.clone(), Purpose::Attachment)
            .unwrap();
        state
            .transfers
            .finish_selection(
                &selection,
                &binding,
                Prepared::from_bytes("draft.txt".into(), b"local".to_vec()).unwrap(),
                "local_intent_123".into(),
            )
            .unwrap();
        let mut download = Download::new(binding.clone(), 4, info).unwrap();
        download.begin("last-fetch".into(), &binding);
        state.download = Some(download);
        state.download_message = Some(asset.clone());
        state.save_message = Some(asset);
        let cancel = Arc::new(std::sync::atomic::AtomicBool::new(false));
        state.save_cancel = Some(cancel.clone());
        state.clear_message_assets();
        assert!(state.download.is_none());
        assert!(state.download_message.is_none());
        assert!(state.save_message.is_none());
        assert!(cancel.load(std::sync::atomic::Ordering::Acquire));
        assert_eq!(state.asset_revision, 1);
        assert_eq!(
            state
                .transfers
                .upload
                .as_ref()
                .unwrap()
                .source()
                .unwrap()
                .bytes(),
            b"local"
        );
    }
    #[test]
    fn only_transport_sealed_identity_survives_login_and_credentials_destroy_it() {
        let (binding, _, _, _) = asset_fixture();
        let mut state = State::default();
        state.transfers.supported = true;
        let t = state
            .transfers
            .begin_selection(binding.clone(), Purpose::Attachment)
            .unwrap();
        state
            .transfers
            .finish_selection(
                &t,
                &binding,
                Prepared::from_bytes("private-name.txt".into(), b"secret".to_vec()).unwrap(),
                "transport_test_123".into(),
            )
            .unwrap();
        state
            .transfers
            .begin(Operation::Intent, "intent".into(), &binding)
            .unwrap();
        state.capture_transport_recovery(false);
        assert!(state.transfers.upload.is_none());
        assert!(state.recovery.is_some());
        state.screen_changed(Screen::Connect);
        state.begin_session();
        state.screen_changed(Screen::Trust);
        state.screen_changed(Screen::Auth);
        state.screen_changed(Screen::Chat);
        assert!(state.recovery.is_some());
        assert!(
            !state
                .recovery
                .as_ref()
                .unwrap()
                .notice()
                .contains("private-name")
        );
        state.screen_changed(Screen::Password);
        assert!(state.recovery.is_none());
        assert!(state.transfers.supported);
    }
    #[test]
    fn save_never_overwrites() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("out.txt");
        write_new_download(&p, b"first").unwrap();
        assert!(write_new_download(&p, b"second").is_err());
        assert_eq!(std::fs::read(p).unwrap(), b"first");
    }
}
