//! Native UI bridge. Persistence owns versions and buffer conflict decisions;
//! this module translates typed requests and the actual laid-out scroll handle.
use super::*;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum RestoreStep {
    Wait,
    Apply,
    Finish,
}
pub(super) fn local_view_poll_plan(screen: Screen, persistence_enabled: bool) -> (bool, bool) {
    (
        screen == Screen::Chat,
        screen == Screen::Chat && persistence_enabled,
    )
}
pub(super) fn local_restore_step(
    screen: Screen,
    layout_revision: u64,
    message_revision: u64,
    layout_epoch: u64,
    pending_epoch: Option<Option<u64>>,
) -> RestoreStep {
    if screen != Screen::Chat || layout_revision != message_revision {
        return RestoreStep::Wait;
    }
    match pending_epoch {
        None => RestoreStep::Wait,
        Some(None) => RestoreStep::Apply,
        Some(Some(applied)) if layout_epoch > applied => RestoreStep::Finish,
        Some(Some(_)) => RestoreStep::Wait,
    }
}

impl MioChatApp {
    pub(super) fn poll_local_view(&mut self, cx: &mut Context<Self>) {
        self.attachment_tick(cx);
        self.poll_custom_emoji(cx);
        self.dot_tick();
        self.poll_emoji();
        let (restore, persist) = local_view_poll_plan(self.screen, self.persistence_enabled);
        if restore {
            self.update_restored_scroll(cx);
            self.poll_reading_move(cx);
        }
        if persist {
            self.poll_persistence(cx);
        }
    }
    pub(super) fn timeline_tools(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let cleared = self.timeline_cleared;
        div()
            .flex()
            .justify_center()
            .gap(px(12.))
            .child(
                div()
                    .id("older")
                    .role(gpui::Role::Button)
                    .aria_label(if cleared {
                        "恢复历史记录"
                    } else {
                        "加载更早消息"
                    })
                    .px(px(12.))
                    .py(px(5.))
                    .rounded(px(6.))
                    .text_size(px(12.))
                    .text_color(rgb(MUTED))
                    .cursor_pointer()
                    .hover(|style| style.bg(rgb(0xe9e6df)))
                    .child(if cleared {
                        "恢复历史记录"
                    } else {
                        "加载更早消息"
                    })
                    .on_click(cx.listener(move |this, _, _, cx| {
                        this.run_command(
                            if cleared { "/history" } else { "/older" }.into(),
                            true,
                            cx,
                        );
                    })),
            )
            .child(
                Self::button("clear-view", "清屏").on_click(cx.listener(|this, _, _, cx| {
                    this.run_command("/clear".into(), true, cx);
                })),
            )
            .when(
                self.persistence_enabled && (!self.feed_latest || self.view_unavailable),
                |d| {
                    d.child(Self::button("view-newer", "较新消息").on_click(cx.listener(
                        |this, _, _, cx| {
                            let position = this.reading_position();
                            if let Err(error) = this.request_view(position, true) {
                                this.error = Some(format!("{error:#}"));
                            }
                            cx.notify();
                        },
                    )))
                    .child(
                        Self::button("view-latest", "返回最新").on_click(cx.listener(
                            |this, _, _, cx| {
                                if let Err(error) = this.request_view(Position::default(), false) {
                                    this.error = Some(format!("{error:#}"));
                                }
                                cx.notify();
                            },
                        )),
                    )
                },
            )
    }
    pub(super) fn persistence_now(&self) -> u64 {
        self.persistence_clock
            .elapsed()
            .as_millis()
            .min(u64::MAX as u128) as u64
    }
    pub(super) fn sync_draft_flags(&mut self) {
        self.draft_version = self.persistence.version();
        self.draft_conflict = self.persistence.conflicted();
        self.draft_backup_id = self.persistence.backup_id().into();
    }
    pub(super) fn apply_snapshot(&mut self, snapshot: Snapshot) {
        if miochat_gpui::word::reserved_start(&snapshot.body) {
            self.error = Some(miochat_gpui::word::QUARANTINE_NOTICE.into());
            return;
        }

        if self.composer != snapshot.body {
            self.cancel_completion();
            self.composer = snapshot.body;
            self.reset_selection();
        }
        self.reply_to = snapshot.reply_to;
    }
    pub(super) fn activate_persistence(&mut self) {
        if !self.persistence_enabled {
            return;
        }
        // Never share an in-memory draft between accounts or servers.
        let owner = format!(
            "{}:{}\0{}\0{}",
            self.spec.host, self.spec.port, self.spec.ssh_user, self.user
        );
        let snapshot = self
            .persistence
            .activate(&owner, &self.room, self.persistence_now());
        self.apply_snapshot(snapshot);
        self.sync_draft_flags();
        self.pending_view = None;
        self.restoring_view = None;
        self.view_unavailable = false;
        self.queued_room = None;
    }
    pub(super) fn reading_position(&self) -> Position {
        let mut position = self
            .persistence
            .local()
            .map(|s| s.position.clone())
            .unwrap_or_default();
        if self.screen != Screen::Chat
            || self.timeline_cleared
            || self.pending_view.is_some()
            || self.restoring_view.is_some()
            || self.view_unavailable
            || self.layout_revision != self.message_revision
        {
            return position;
        }
        let at_bottom =
            (self.timeline_scroll.offset().y + self.timeline_scroll.max_offset().y).abs() <= px(2.);
        if self.feed_latest && at_bottom {
            position.anchor = self.messages.last().map_or(0, |m| m.id);
            position.offset = 0;
        } else {
            // The timeline header is child0; message bubbles start at child1.
            let (child, clipped) = self.timeline_scroll.logical_scroll_top();
            if let Some(message) = self.messages.get(child.saturating_sub(1)) {
                position.anchor = message.id;
                let pixels: f32 = (-clipped).max(px(0.)).into();
                position.offset = (pixels / 23.).floor() as i32 + 1;
            }
        }
        if self.foreground && !self.messages.is_empty() {
            let bottom = self.timeline_scroll.bottom_item().saturating_sub(1);
            if let Some(message) = self.messages.get(bottom.min(self.messages.len() - 1)) {
                position.last_read = position.last_read.max(message.id);
            }
        }
        position
    }
    pub(super) fn observe_persistence(&mut self) {
        if !self.persistence_enabled
            || self.persistence.quarantined()
            || miochat_gpui::word::reserved_start(&self.composer)
            || self.room.is_empty()
            || self.marked_range.is_some()
        {
            return;
        }
        let snapshot = Snapshot {
            body: self.composer.clone(),
            reply_to: self.reply_to,
            position: self.reading_position(),
        };
        self.persistence.observe(snapshot, self.persistence_now());
    }
    pub(super) fn queue_persistence(&mut self, op: Operation) -> anyhow::Result<()> {
        if !op.safe_body() {
            anyhow::bail!("{}", miochat_gpui::word::RESERVED_NOTICE);
        }

        let id = self.next_request_id("position");
        if !self
            .persistence
            .begin(id.clone(), op.clone(), self.persistence_now())
        {
            anyhow::bail!("草稿操作尚未准备好");
        }
        if let Err(error) = self
            .session
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("not connected"))?
            .send_persistence(&op, &id)
        {
            self.persistence.fail(&id, self.persistence_now());
            return Err(error);
        }
        Ok(())
    }
    pub(super) fn restore_position(&mut self, position: Position) {
        self.view_unavailable = false;
        if position.offset > 0 || !self.feed_latest {
            if let Err(error) = self.request_view(position, false) {
                self.view_unavailable = true;
                self.error = Some(format!(
                    "无法恢复阅读位置：{error:#}。可返回最新消息，原位置不会被空值覆盖。"
                ));
            }
        } else {
            self.restoring_view = Some((position, None));
        }
    }
    pub(super) fn request_view(&mut self, position: Position, append: bool) -> anyhow::Result<()> {
        if self.pending_view.is_some() {
            anyhow::bail!("阅读窗口请求仍在处理中");
        }
        let id = self.next_request_id("view");
        let mut request = Request::simple(if append { "view_newer" } else { "view_window" }, &id);
        request.room = &self.room;
        request.position = Some(&position);
        request.before = self.messages.last().map_or(0, |m| m.id);
        self.send_request(&request)?;
        self.pending_view = Some(PendingView {
            request_id: id,
            room: self.room.clone(),
            position,
            append,
            started: self.persistence_now(),
        });
        Ok(())
    }
    pub(super) fn handle_view(&mut self, response: Response, cx: &mut Context<Self>) {
        if self.pending_view.as_ref().is_none_or(|p| {
            p.request_id != response.request_id || p.room != response.room || p.room != self.room
        }) {
            return;
        }
        self.reading_response(&response.request_id, &response.room, true);
        let pending = self.pending_view.take().unwrap();
        self.timeline_cleared = false;
        let position = pending.position;
        if !pending.append {
            self.attachment_clear_message_assets(cx);
            self.messages.clear();
            self.message_ids.clear();
        }
        self.notifications
            .history(&response.room, &response.messages, false);
        for message in response.messages {
            self.merge_message(message, cx);
        }
        self.feed_latest = response.at_latest;
        self.view_unavailable = false;
        self.restoring_view = Some((position, None));
        self.message_revision = self.message_revision.wrapping_add(1);
    }
    pub(super) fn update_restored_scroll(&mut self, cx: &mut Context<Self>) {
        match local_restore_step(
            self.screen,
            self.layout_revision,
            self.message_revision,
            self.layout_epoch,
            self.restoring_view.as_ref().map(|(_, epoch)| *epoch),
        ) {
            RestoreStep::Wait => return,
            RestoreStep::Finish => {
                self.restoring_view = None;
                return;
            }
            RestoreStep::Apply => {}
        }
        let (position, _) = self
            .restoring_view
            .clone()
            .expect("restore step requires pending position");
        if position.offset == 0 {
            self.timeline_scroll.scroll_to_bottom();
        } else if let Some(index) = self.messages.iter().position(|m| m.id == position.anchor) {
            if let Some(bounds) = self.timeline_scroll.bounds_for_item(index + 1) {
                let y = self.timeline_scroll.bounds().top()
                    - bounds.top()
                    - px((position.offset - 1) as f32 * 23.);
                self.timeline_scroll.set_offset(gpui::point(px(0.), y));
            } else {
                return;
            }
        } else {
            self.view_unavailable = true;
            self.restoring_view = None;
            self.error = Some("原阅读锚点不可用。可返回最新消息，原位置暂不覆盖。".into());
            return;
        }
        self.restoring_view = Some((position, Some(self.layout_epoch)));
        cx.notify();
    }
    pub(super) fn poll_persistence(&mut self, cx: &mut Context<Self>) {
        if !self.persistence_enabled
            || self.screen != Screen::Chat
            || self.room.is_empty()
            || self.session.is_none()
        {
            return;
        }
        let now = self.persistence_now();
        if self.persistence.expire(now) {
            self.error = Some("草稿确认超时；保留输入，重新读取版本后再保存。".into());
            cx.notify();
        }
        if self
            .pending_view
            .as_ref()
            .is_some_and(|p| now.saturating_sub(p.started) >= 10_000)
        {
            self.pending_view = None;
            self.view_unavailable = true;
            self.error = Some("读取历史窗口超时；原阅读位置已保留，可重试或返回最新。".into());
            cx.notify();
        }
        self.observe_persistence();
        if self
            .close_started
            .is_some_and(|at| now.saturating_sub(at) >= 10_000)
            || self.close_started.is_some() && self.persistence.conflicted()
        {
            self.close_started = None;
            self.set_feature_panel(Some(Panel{title:"草稿尚未确认保存".into(),
                lines:vec!["输入仍保留，部分请求可能仍在处理。不会自动覆盖其它设备的草稿；可返回编辑，或复制所有未确认输入后关闭。".into()],
                actions:vec![PanelAction{label:"返回编辑".into(),command:"local:cancel-close".into(),fill:false},
                    PanelAction{label:"复制所有未确认输入并关闭".into(),command:"local:copy-close".into(),fill:false}],..Panel::default()}));
            cx.notify();
        }
        if self.pending_send.is_none()
            && self.pending_room.is_none()
            && self.pending_password.is_none()
            && self.pending_key_bind.is_none()
            && self.marked_range.is_none()
        {
            let immediate = self.queued_room.is_some() || self.close_started.is_some();
            let op = if immediate
                && self.persistence.ready()
                && self.persistence.dirty()
                && !self.persistence.conflicted()
            {
                self.persistence.manual(false)
            } else {
                self.persistence.next(now)
            };
            if let Some(op) = op {
                if let Err(error) = self.queue_persistence(op) {
                    self.error = Some(format!("草稿请求失败：{error:#}"));
                }
                cx.notify();
            }
            if self.persistence.ready()
                && !self.persistence.dirty()
                && !self.persistence.pending()
                && !self.persistence.conflicted()
            {
                if self.composition.is_none()
                    && let Some(room) = self.queued_room.take()
                {
                    self.select_room_now(room, cx);
                }
                if self.composition.is_none()
                    && self.close_started.is_some()
                    && !self.persistence.has_unconfirmed_backups()
                {
                    self.close_ready = true;
                    if let Some(handle) = self.window_handle {
                        let _ = handle.update(cx, |_, window, _| window.remove_window());
                    }
                }
            }
        }
        self.sync_draft_flags();
    }
    pub(super) fn handle_persistent_draft(&mut self, response: Response) {
        let Some(draft) = response.draft else {
            return;
        };
        let ack = match response.kind.as_str() {
            "draft" => Ack::Loaded,
            "draft_saved" => Ack::Saved,
            "draft_conflict" => Ack::Conflict,
            "draft_suspended" => Ack::Suspended,
            _ => return,
        };
        let snapshot = Snapshot {
            body: draft.body,
            reply_to: draft.reply_to,
            position: draft.position,
        };
        let update = self.persistence.acknowledge(
            (&response.request_id, &draft.room),
            ack,
            draft.version,
            snapshot,
            draft.backup_id,
            self.persistence_now(),
        );
        if update.ignored {
            return;
        }
        if let Some(snapshot) = update.apply {
            let position = snapshot.position.clone();
            self.apply_snapshot(snapshot);
            if update.restore_position {
                self.restore_position(position);
            }
        }
        self.sync_draft_flags();
        self.status = if update.conflict {
            "草稿有冲突：请选择保留本地或读取另一设备版本"
        } else if ack == Ack::Saved {
            "草稿与阅读位置已保存"
        } else {
            "草稿版本已同步"
        }
        .into();
    }
    pub(super) fn request_close(&mut self, cx: &mut Context<Self>) -> bool {
        if self.composition.is_some() {
            return self.close_composition(composition_ui::CompositionAfter::Window, cx);
        }
        self.begin_feature_navigation();
        if self.close_ready
            || !self.persistence_enabled
            || self.room.is_empty()
            || self.must_reset
            || self.session.is_none()
        {
            return true;
        }
        self.observe_persistence();
        // Account forms can sit over an unsaved chat buffer. Closing a form
        // must still flush that buffer, and wait for in-flight account work.
        self.screen = Screen::Chat;
        self.password.clear();
        self.confirm.clear();
        self.invite.clear();
        self.identity_passphrase.clear();
        self.reset_selection();
        if self.persistence.ready()
            && !self.persistence.dirty()
            && !self.persistence.pending()
            && !self.persistence.has_unconfirmed_backups()
            && self.pending_send.is_none()
            && self.pending_key_bind.is_none()
            && self.pending_password.is_none()
        {
            return true;
        }
        self.close_started
            .get_or_insert_with(|| self.persistence_clock.elapsed().as_millis() as u64);
        self.status = "正在确认草稿保存，稍后关闭…".into();
        cx.notify();
        false
    }
}
