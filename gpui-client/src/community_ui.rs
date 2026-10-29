use super::*;

impl MioChatApp {
    pub(super) fn activate_community_action(
        &mut self,
        command: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        use miochat_gpui::{
            community_mutation::{CommunityAction, CommunityIntent},
            direct_message::FormPurpose,
        };
        let local = command.strip_prefix("local:community:");
        let Some(action) = CommunityAction::parse(local.unwrap_or(command)) else {
            return false;
        };
        if self.screen != Screen::Chat || self.direct_form.is_some() || self.pending_send.is_some()
        {
            return true;
        }
        // Menu entries first show the actual server-owned target channel/object.
        if local.is_none() && action.creates() {
            self.run_command(
                if action == CommunityAction::Poll {
                    "/results"
                } else {
                    "/events"
                }
                .into(),
                true,
                cx,
            );
            return true;
        }
        let Some(panel) = self.feature_panel.as_ref().filter(|p| p.scope == self.room) else {
            return true;
        };
        let Some(view) = panel.community.as_ref() else {
            self.run_command(
                match action {
                    CommunityAction::Vote => "/results",
                    CommunityAction::Rsvp | CommunityAction::Unrsvp => "/events",
                    _ => "/participants",
                }
                .into(),
                true,
                cx,
            );
            return true;
        };
        let mut intent = if action.creates() {
            CommunityIntent {
                action,
                object_id: 0,
                choice: 0,
                room: view.room.clone(),
            }
        } else {
            let Some(mut i) = CommunityIntent::from_command(command) else {
                return true;
            };
            let Some(detail) = view.detail.as_ref().filter(|d| d.card.id == i.object_id) else {
                return true;
            };
            i.room = detail.card.room.clone();
            i
        };
        if intent.room.is_empty() {
            return true;
        }
        // Copy the typed room now. Later same-DM navigation cannot rewrite it.
        intent.action = action;
        self.open_direct_form(cx);
        if let Some(form) = self.direct_form.as_mut() {
            form.purpose = FormPurpose::Community(intent);
            self.field = direct_field(form.initial_slot());
            self.reset_selection();
        }
        self.focus.focus(window, cx);
        cx.notify();
        true
    }
    pub(super) fn insert_daily_from_panel(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.screen != Screen::Chat || self.direct_form.is_some() {
            return;
        }
        let Some(panel) = self.feature_panel.as_ref() else {
            return;
        };
        let Some(view) = panel.community.as_ref() else {
            return;
        };
        if miochat_gpui::community::insert_daily_topic(
            &mut self.composer,
            &mut self.selection,
            &view.daily_topic,
            self.marked_range.is_some(),
            &panel.scope,
            &self.room,
        ) {
            self.begin_feature_navigation();
            self.observe_persistence();
            self.focus.focus(window, cx);
        } else {
            self.error =
                Some("话题未插入：请完成输入法组合、核对原会话及草稿长度。原草稿保留。".into());
        }
        cx.notify();
    }
    pub(super) fn community_content(&self, panel: &Panel, cx: &mut Context<Self>) -> Div {
        let mut content = div().flex().flex_col().gap(px(10.));
        let Some(view) = &panel.community else {
            return content;
        };
        if !view.room.is_empty() {
            content = content.child(
                div()
                    .text_size(px(12.))
                    .child(format!("频道 #{}", view.room)),
            );
        }
        if !view.daily_topic.is_empty() {
            content = content
                .child(div().text_size(px(12.)).child(view.daily_date.clone()))
                .child(div().text_size(px(16.)).child(view.daily_topic.clone()));
        }
        if view.detail.is_none() && view.daily_topic.is_empty() && view.objects.is_empty() {
            content = content.child("这个频道暂时没有此类内容。");
        }
        for object in &view.objects {
            let Some(original) =
                self.feature_action_original(panel, &format!("/object {}", object.id))
            else {
                continue;
            };
            let revision = self.feature_revision;
            let id = object.id;
            content = content.child(
                div()
                    .id(gpui::SharedString::from(format!("community-{id}")))
                    .role(gpui::Role::Button)
                    .aria_label(format!("打开 {}", object.title))
                    .anchor_scroll(self.feature_anchor_for(&feature_menu::Item::Action(original)))
                    .focusable()
                    .p(px(10.))
                    .rounded(px(8.))
                    .bg(self.canvas_color())
                    .when(self.feature_original_selected(original), |d| {
                        d.border_1().border_color(rgb(0x899bb0))
                    })
                    .cursor_pointer()
                    .child(
                        div()
                            .font_weight(gpui::FontWeight::MEDIUM)
                            .child(format!("#{} {}", id, object.title)),
                    )
                    .child(div().text_size(px(12.)).child(format!(
                        "{} · @{}{}{}",
                        object.kind,
                        object.owner,
                        if object.closed { " · 已结束" } else { "" },
                        if object.unavailable {
                            " · 状态过大"
                        } else {
                            ""
                        }
                    )))
                    .on_click(cx.listener(move |this, _, window, cx| {
                        this.activate_visible_feature(revision, original, window, cx)
                    })),
            );
        }
        if let Some(detail) = &view.detail {
            let card = &detail.card;
            let id = card.id;
            content = content
                .child(
                    div()
                        .font_weight(gpui::FontWeight::SEMIBOLD)
                        .child(card.title.clone()),
                )
                .child(div().text_size(px(12.)).child(format!(
                    "#{} · {} · @{} · {}",
                    id,
                    card.kind,
                    card.owner,
                    if card.closed {
                        "已结束"
                    } else {
                        "进行中"
                    }
                )));
            if card.unavailable {
                return content.child("内容状态超过安全显示限制；数据未删除。");
            }
            if !detail.body.is_empty() {
                content = if card.kind == "art" {
                    content.child(community_ascii_block(&detail.body))
                } else {
                    content.child(div().child(detail.body.clone()))
                };
            }
            if !detail.when_text.is_empty() {
                content = content
                    .child(format!("北京时间：{}", detail.when_text))
                    .child(detail.description.clone());
            }
            content = content.children(detail.options.iter().map(|option| {
                div()
                    .flex()
                    .justify_between()
                    .p(px(8.))
                    .rounded(px(6.))
                    .bg(self.canvas_color())
                    .child(format!("{}. {}", option.index, option.text))
                    .child(format!("{} 票", option.votes))
            }));
            if let Some(game) = &detail.game {
                if !game.cells.is_empty() {
                    content = content.child(format!(
                        "X @{} · O @{} · 轮到 @{}{}",
                        game.x,
                        game.o,
                        game.turn,
                        if game.accepted { "" } else { " · 待接受" }
                    ));
                    content = content.child(self.tic_board(detail, cx));
                    if !game.winner.is_empty() {
                        content = content.child(format!("结果：{}", game.winner));
                    }
                } else if card.kind == "guess" {
                    content = content.child(format!(
                        "数字范围1–{} · 每次猜测给出偏大或偏小提示",
                        game.max
                    ));
                } else {
                    content = content.child(format!(
                        "范围上限 {} · 失误 {} · 已猜：{}",
                        game.max,
                        game.wrong,
                        game.guesses.join("、")
                    ));
                }
                if card.kind == "word" {
                    content = content.child(format!("进度：{}", game.masked));
                }
                if card.closed && !game.secret.is_empty() {
                    content = content.child(format!("已结束 · 答案：{}", game.secret));
                }
            }
            content = content
                .child(div().text_size(px(12.)).child(format!(
                    "参与者 {} 人（用户名顺序，每页100人）",
                    detail.participant_count
                )))
                .children(
                    detail
                        .participants
                        .iter()
                        .map(|name| div().text_size(px(12.)).child(format!("@{name}"))),
                );
            content = content.children(detail.comments.iter().map(|comment| {
                div()
                    .p(px(8.))
                    .rounded(px(6.))
                    .bg(self.canvas_color())
                    .child(div().text_size(px(12.)).child(format!("@{}", comment.user)))
                    .child(comment.body.clone())
            }));
        }
        content
    }
}

// Direct unwrapped text is measured at its intrinsic width inside this bounded
// scroll viewport. Explicit newlines remain newlines; no ellipsis or clipping
// transform removes the right-hand columns of a 160-character ASCII drawing.
fn community_ascii_block(body: &str) -> gpui::Stateful<Div> {
    div()
        .id("community-ascii-scroll")
        .w_full()
        .min_w(px(0.))
        .overflow_x_scroll()
        .font_family("Consolas")
        .whitespace_nowrap()
        .child(miochat_gpui::community::ascii_art_text(body))
}

#[cfg(test)]
mod ascii_style_tests {
    use super::*;
    #[test]
    fn actual_gallery_block_uses_nowrap_and_bounded_horizontal_scroll() {
        let body = format!("{}\n  {}", "-".repeat(160), "*".repeat(158));
        let mut block = community_ascii_block(&body);
        assert_eq!(block.style().overflow.x, Some(gpui::Overflow::Scroll));
        assert_eq!(
            block.text_style().white_space,
            Some(gpui::WhiteSpace::Nowrap)
        );
        assert!(block.text_style().text_overflow.is_none());
        assert_eq!(
            block.text_style().font_family.as_ref().unwrap().as_ref(),
            "Consolas"
        );
        assert!(block.style().size.width.is_some());
        assert!(block.style().min_size.width.is_some());
        assert_eq!(miochat_gpui::community::ascii_art_text(&body), body);
    }
}
