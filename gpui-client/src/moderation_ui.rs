use super::*;
use miochat_gpui::{direct_message::FormPurpose, moderation::ReadIntent};
impl MioChatApp {
    // User intent to open/replace an overlay or input workflow invalidates any
    // in-flight private read, including when the old panel was just a menu.
    pub(super) fn begin_feature_navigation(&mut self) {
        self.dot_ui.clear();
        self.attachment_ui.freeze(false);
        self.custom_emoji.freeze(false);
        self.cancel_completion();
        self.close_emoji();
        self.discard_secret_admin();
        if let Some(job) = self.maintenance.as_mut() {
            job.hide();
        }
        self.dismiss_feature_panel();
    }

    pub(super) fn activate_moderation_action(
        &mut self,
        command: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        if command == "local:revisions-next" {
            if self.screen == Screen::Chat
                && self.direct_form.is_none()
                && self.word_start.is_none()
                && self.pending_send.is_none()
            {
                self.run_command(command.into(), true, cx);
            }
            return true;
        }
        let Some(intent) = ReadIntent::parse(command) else {
            return false;
        };
        if self.screen != Screen::Chat
            || self.direct_form.is_some()
            || self.word_start.is_some()
            || self.pending_send.is_some()
        {
            return true;
        }
        if matches!(
            intent.command.as_str(),
            "/admin" | "/bans" | "/audit" | "/recalls" | "/reports" | "/errors" | "/health"
        ) || !intent.requested.is_empty()
            || matches!(intent.command.as_str(), "/ip" | "/logins")
        {
            self.run_command(command.trim().into(), true, cx);
            return true;
        }
        self.open_direct_form(cx);
        if let Some(f) = self.direct_form.as_mut() {
            f.purpose = FormPurpose::Moderation(intent);
            self.field = Field::DirectTarget;
            self.reset_selection();
        }
        self.focus.focus(window, cx);
        cx.notify();
        true
    }
    pub(super) fn moderation_content(&self, panel: &Panel) -> Div {
        let mut d = div().flex().flex_col().gap(px(10.));
        if !miochat_gpui::moderation::private_panel_payload(panel) {
            return d;
        }
        let Some(v) = panel.moderation.as_ref() else {
            return d;
        };
        d = d.child(div().text_size(px(12.)).child(v.explanation.clone()));
        if let Some(p) = &v.profile {
            for line in [
                p.avatar.clone(),
                format!("@{} · UID #{} · {}", p.name, p.uid, p.role),
                format!("状态：{}", p.status),
                format!("简介：{}", p.bio),
                format!("注册：{}", p.created),
                format!(
                    "最近登录：{} · {}",
                    p.last_login,
                    if p.online { "在线" } else { "离线" }
                ),
                format!("成就：{}", p.badges.join(" / ")),
                format!("我的私有备注：{}", p.note),
            ] {
                d = d.child(div().text_size(px(13.)).child(line));
            }
        }
        if let Some(health) = &v.health {
            d = d.child(self.statistics_content(health));
        }
        for r in &v.diagnostics {
            d = d.child(
                div()
                    .p(px(8.))
                    .rounded(px(8.))
                    .bg(self.raised_color())
                    .flex()
                    .flex_col()
                    .gap(px(4.))
                    .child(format!("#{} · {} · {}", r.id, r.time, r.kind))
                    .child(r.detail.clone()),
            );
        }
        for r in &v.logins {
            d = d.child(
                div()
                    .p(px(8.))
                    .rounded(px(8.))
                    .bg(self.raised_color())
                    .flex()
                    .flex_col()
                    .gap(px(4.))
                    .child(format!(
                        "{} · {}",
                        r.time,
                        if r.ok { "成功" } else { "失败" }
                    ))
                    .child(r.ip.clone())
                    .child(r.client.clone()),
            );
        }
        for r in &v.users {
            d = d.child(div().text_size(px(13.)).child(if r.note.is_empty() {
                format!("@{}", r.name)
            } else {
                format!("@{} · 我的备注：{}", r.name, r.note)
            }));
        }
        for r in &v.sanctions {
            d = d.child(
                div()
                    .p(px(8.))
                    .flex()
                    .flex_col()
                    .child(format!(
                        "#{} · {} {} · 频道 {}",
                        r.id, r.kind, r.target, r.room
                    ))
                    .child(format!("expires={} · 已撤销={}", r.expires, r.revoked))
                    .child(r.reason.clone()),
            );
        }
        for r in &v.audits {
            d = d.child(
                div()
                    .p(px(8.))
                    .flex()
                    .flex_col()
                    .child(format!("#{} · {} · @{}", r.id, r.time, r.actor))
                    .child(format!("{} · {}", r.action, r.target))
                    .child(r.detail.clone()),
            );
        }
        for r in &v.recalls {
            d = d.child(
                div()
                    .p(px(8.))
                    .flex()
                    .flex_col()
                    .child(format!("消息 #{} · @{} · #{}", r.id, r.sender, r.room))
                    .child(r.body.clone()),
            );
        }
        for r in &v.reports {
            d = d.child(
                div()
                    .p(px(8.))
                    .flex()
                    .flex_col()
                    .child(format!(
                        "举报 #{} · @{} · 消息 #{} · {}",
                        r.id, r.user, r.message_id, r.status
                    ))
                    .child(r.reason.clone()),
            );
        }
        if let Some(r) = &v.reported {
            d = d.child(
                div()
                    .p(px(8.))
                    .flex()
                    .flex_col()
                    .child(format!(
                        "举报 #{} · 消息 #{} · @{}",
                        r.report_id, r.message_id, r.sender
                    ))
                    .child(r.body.clone()),
            );
        }
        if let Some(p) = &v.revisions {
            d = d.child(format!(
                "消息 #{} · 修订 {} 之后 · 快照上界 {}",
                p.message_id, p.after, p.until
            ));
            for r in &p.rows {
                d = d.child(
                    div()
                        .p(px(8.))
                        .flex()
                        .flex_col()
                        .child(format!(
                            "修订 #{} · {} · @{} · {}",
                            r.id, r.time, r.actor, r.action
                        ))
                        .child(r.body.clone()),
                );
            }
            d = d.child(if p.next_after > 0 {
                "仍有修订，请点下一页。"
            } else {
                "已到此快照末尾；重新读取可查看后续修订。"
            });
        }
        if v.window > 0 {
            d = d.child(format!(
                "本次 {} 条 · 最多 {} 条{}",
                v.count(),
                v.window,
                if v.audited {
                    " · 查询审计已提交"
                } else {
                    ""
                }
            ));
        }
        d
    }
}
