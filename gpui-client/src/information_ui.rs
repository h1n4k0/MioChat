use super::*;

impl MioChatApp {
    pub(super) fn information_content(&self, panel: &Panel) -> Div {
        let mut content = div().flex().flex_col().gap(px(10.));
        if !miochat_gpui::information::information_panel_payload(panel) {
            return content;
        }
        if let Some(stats) = &panel.statistics {
            content = content.child(self.statistics_content(stats));
        }
        if !panel.server_time.is_empty() {
            content = content.child(
                div()
                    .font_family("Consolas")
                    .text_size(px(14.))
                    .child(panel.server_time.clone()),
            );
        }
        content
    }
    pub(super) fn statistics_content(&self, stats: &miochat_gpui::information::Statistics) -> Div {
        let mut content = div().flex().flex_col().gap(px(10.));
        content = content.children(stats.rows().into_iter().map(|(label, value)| {
            div()
                .flex()
                .justify_between()
                .gap(px(8.))
                .p(px(8.))
                .rounded(px(6.))
                .bg(self.canvas_color())
                .child(div().text_size(px(12.)).child(label))
                .child(div().font_weight(gpui::FontWeight::MEDIUM).child(value))
        }));
        if !stats.storage_note.is_empty() {
            content = content.child(div().text_size(px(12.)).child(stats.storage_note.clone()));
        }
        content
    }
}
