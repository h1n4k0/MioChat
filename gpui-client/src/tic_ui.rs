use super::*;
use miochat_gpui::{
    community::ObjectDetail,
    direct_message::FormPurpose,
    tic::{TicAction, TicIntent},
};
impl MioChatApp {
    pub(super) fn open_tic_form(&mut self, intent: TicIntent, cx: &mut Context<Self>) {
        if self.screen != Screen::Chat || self.direct_form.is_some() || self.pending_send.is_some()
        {
            return;
        }
        self.open_direct_form(cx);
        if let Some(f) = self.direct_form.as_mut() {
            f.purpose = FormPurpose::Tic(intent);
            self.field = direct_field(f.initial_slot());
            self.reset_selection();
        }
        cx.notify();
    }
    pub(super) fn activate_tic_action(
        &mut self,
        command: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        let local = command == "local:ttt-challenge";
        if !local && command.split_whitespace().next() != Some("/ttt") {
            return false;
        }
        if self.screen != Screen::Chat || self.direct_form.is_some() || self.pending_send.is_some()
        {
            return true;
        }
        if !local && command.split_whitespace().count() == 1 {
            self.run_command("/participants".into(), true, cx);
            return true;
        }
        let Some(panel) = self.feature_panel.as_ref().filter(|p| p.scope == self.room) else {
            return true;
        };
        let Some(view) = panel.community.as_ref() else {
            return true;
        };
        let intent = if local {
            TicIntent {
                action: TicAction::Challenge,
                object_id: 0,
                cell: 0,
                room: view.room.clone(),
                revision: None,
                requested: String::new(),
            }
        } else {
            let Some(mut i) = TicIntent::from_command(command) else {
                return true;
            };
            let Some(d) = view
                .detail
                .as_ref()
                .filter(|d| d.card.id == i.object_id && d.card.kind == "ttt" && !d.card.closed)
            else {
                return true;
            };
            if i.action == TicAction::Move {
                let Some(bound) = miochat_gpui::tic::board_move(d, i.cell as usize) else {
                    return true;
                };
                bound
            } else {
                let Some(game) = d.game.as_ref().filter(|g| g.can_accept) else {
                    return true;
                };
                i.room = d.card.room.clone();
                i.revision = Some(game.revision);
                i
            }
        };
        if intent.room.is_empty() {
            return true;
        }
        self.open_tic_form(intent, cx);
        self.focus.focus(window, cx);
        true
    }
    pub(super) fn tic_board(&self, detail: &ObjectDetail, cx: &mut Context<Self>) -> Div {
        let Some(game) = detail.game.as_ref().filter(|g| g.cells.len() == 9) else {
            return div().child("棋盘状态不可用，请刷新。");
        };
        div()
            .flex()
            .flex_col()
            .gap(px(5.))
            .children((0..3).map(|row| {
                div().flex().gap(px(5.)).children((0..3).map(|column| {
                    let index = row * 3 + column;
                    let cell = index + 1;
                    let text = if game.cells[index].is_empty() {
                        cell.to_string()
                    } else {
                        game.cells[index].clone()
                    };
                    let intent = miochat_gpui::tic::board_move(detail, cell);
                    let generation = self.generation;
                    let owner = self.user.clone();
                    div()
                        .id(gpui::SharedString::from(format!("tic-cell-{cell}")))
                        .role(gpui::Role::Button)
                        .aria_label(format!("井字棋格子 {cell}: {text}"))
                        .focusable()
                        .w(px(82.))
                        .h(px(68.))
                        .flex()
                        .items_center()
                        .justify_center()
                        .rounded(px(8.))
                        .bg(self.canvas_color())
                        .border_1()
                        .border_color(self.preference_border())
                        .text_size(px(24.))
                        .when(intent.is_some(), |d| d.cursor_pointer())
                        .when(intent.is_none(), |d| d.text_color(rgb(MUTED)))
                        .child(text)
                        .on_click(cx.listener(move |this, _, window, cx| {
                            if let Some(intent) = intent.clone() {
                                if this.generation != generation
                                    || this.user != owner
                                    || !this.feature_panel.as_ref().is_some_and(|p| {
                                        miochat_gpui::tic::board_move_still_current(
                                            &intent, p, &this.room,
                                        )
                                    })
                                {
                                    return;
                                }
                                this.open_tic_form(intent, cx);
                                this.focus.focus(window, cx);
                            }
                        }))
                }))
            }))
    }
}
