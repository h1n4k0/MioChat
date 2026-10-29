//! The multiline editor uses the same shaped glyph positions for painting,
//! mouse selection, vertical navigation and the platform IME callbacks.
use super::*;
use gpui::{MouseButton, ShapedLine, TextRun, point};

const LINE: f32 = 22.;
fn line_spans(text: &str) -> Vec<Range<usize>> {
    let mut start = 0;
    text.split('\n')
        .map(|line| {
            let range = start..start + line.len();
            start += line.len() + 1;
            range
        })
        .collect()
}
#[derive(Clone)]
struct Line {
    start: usize,
    shaped: ShapedLine,
    origin: Point<Pixels>,
}
#[derive(Clone)]
pub(super) struct TextLayout {
    text: String,
    lines: Vec<Line>,
}
impl TextLayout {
    fn row_for_byte(&self, byte: usize) -> usize {
        self.lines
            .iter()
            .rposition(|l| l.start <= byte)
            .unwrap_or(0)
    }
    fn byte_for_point(&self, p: Point<Pixels>) -> usize {
        let i = self
            .lines
            .iter()
            .rposition(|l| p.y >= l.origin.y)
            .unwrap_or(0);
        let l = &self.lines[i];
        let x = p.x - l.origin.x;
        let local = if x <= px(0.) {
            0
        } else {
            l.shaped.index_for_x(x).unwrap_or(l.shaped.len())
        };
        l.start + local
    }
    fn caret(&self, byte: usize) -> Bounds<Pixels> {
        let l = &self.lines[self.row_for_byte(byte)];
        let local = byte.saturating_sub(l.start).min(l.shaped.len());
        Bounds::new(
            point(l.origin.x + l.shaped.x_for_index(local), l.origin.y),
            size(px(2.), px(LINE)),
        )
    }
}
fn drag_selection(text: &str, anchor: usize, at: usize) -> (Range<usize>, (usize, usize)) {
    let anchor = editing::valid_selection(text, anchor..anchor).start;
    let at = editing::valid_selection(text, at..at).start;
    (anchor.min(at)..anchor.max(at), (anchor, at))
}
impl MioChatApp {
    pub(super) fn composition_caret(&self) -> usize {
        self.composition_cursor
            .filter(|(anchor, caret)| {
                self.selection == ((*anchor).min(*caret)..(*anchor).max(*caret))
            })
            .map_or(self.selection.end, |(_, caret)| caret)
    }
    pub(super) fn move_composition_selection(&mut self, position: usize, shift: bool) {
        let anchor = if shift {
            self.composition_cursor
                .filter(|(a, c)| self.selection == ((*a).min(*c)..(*a).max(*c)))
                .map_or(self.selection.start, |(a, _)| a)
        } else {
            position
        };
        self.composition_cursor = Some((anchor, position));
        self.selection = anchor.min(position)..anchor.max(position);
        self.composition_reveal = true;
    }
    fn composition_layout(&self) -> Option<&TextLayout> {
        self.composition_layout.as_ref().filter(|l| {
            self.composition_input_active()
                && self.field == Field::CompositionBody
                && self.composition.as_ref().is_some_and(|e| e.body == l.text)
        })
    }
    pub(super) fn composition_ime_bounds(&self, range: Range<usize>) -> Option<Bounds<Pixels>> {
        let l = self.composition_layout()?;
        Some(l.caret(editing::byte_offset(&l.text, range.start)))
    }
    pub(super) fn composition_ime_index(&self, p: Point<Pixels>) -> Option<usize> {
        let l = self.composition_layout()?;
        Some(editing::utf16_offset(&l.text, l.byte_for_point(p)))
    }
    pub(super) fn composition_move(&mut self, key: &str, shift: bool, command: bool) -> bool {
        let Some(l) = self.composition_layout() else {
            return false;
        };
        let at = self.composition_caret().min(l.text.len());
        let row = l.row_for_byte(at);
        let line = &l.lines[row];
        let position = match key {
            "home" if command => 0,
            "end" if command => l.text.len(),
            "home" => line.start,
            "end" => line.start + line.shaped.len(),
            "up" | "down" => {
                let next = if key == "up" {
                    row.saturating_sub(1)
                } else {
                    (row + 1).min(l.lines.len() - 1)
                };
                let target = &l.lines[next];
                let x = line
                    .shaped
                    .x_for_index(at.saturating_sub(line.start).min(line.shaped.len()));
                target.start + target.shaped.index_for_x(x).unwrap_or(target.shaped.len())
            }
            _ => return false,
        };
        self.move_composition_selection(position, shift);
        true
    }
    pub(super) fn composition_text(&self, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .id("composition-body-input")
            .h(px(260.))
            .flex_shrink_0()
            .min_w(px(0.))
            .w_full()
            .overflow_y_scroll()
            .overflow_x_scroll()
            .track_scroll(&self.composition_scroll)
            .border_1()
            .border_color(self.preference_border())
            .bg(self.surface_color())
            .rounded(px(6.))
            .font_family("Consolas")
            .text_size(px(14.))
            .text_color(rgb(TEXT))
            .on_mouse_down(
                MouseButton::Left,
                cx.listener(|this, event: &gpui::MouseDownEvent, window, cx| {
                    if !this.composition_visible() || this.marked_range.is_some() {
                        return;
                    }
                    this.field = Field::CompositionBody;
                    if let Some(layout) = this.composition_layout() {
                        let at = layout.byte_for_point(event.position);
                        let anchor = if event.modifiers.shift {
                            this.selection.start
                        } else {
                            at
                        };
                        let (selection, cursor) = drag_selection(&layout.text, anchor, at);
                        this.composition_drag = Some(anchor);
                        this.composition_cursor = Some(cursor);
                        this.selection = selection;
                        this.focus.focus(window, cx);
                        cx.notify();
                        cx.stop_propagation();
                    }
                }),
            )
            .on_mouse_move(cx.listener(|this, event: &gpui::MouseMoveEvent, _, cx| {
                if let (Some(anchor), Some(layout)) =
                    (this.composition_drag, this.composition_layout())
                {
                    let at = layout.byte_for_point(event.position);
                    let (selection, cursor) = drag_selection(&layout.text, anchor, at);
                    this.composition_cursor = Some(cursor);
                    this.selection = selection;
                    cx.notify();
                }
            }))
            .on_mouse_up(
                MouseButton::Left,
                cx.listener(|this, _, _, _| this.composition_drag = None),
            )
            .on_mouse_up_out(
                MouseButton::Left,
                cx.listener(|this, _, _, _| this.composition_drag = None),
            )
            .child(TextElement {
                entity: cx.entity(),
            })
    }
}
struct TextElement {
    entity: Entity<MioChatApp>,
}
impl IntoElement for TextElement {
    type Element = Self;
    fn into_element(self) -> Self {
        self
    }
}
impl Element for TextElement {
    type RequestLayoutState = Vec<(usize, ShapedLine)>;
    type PrepaintState = ();
    fn id(&self) -> Option<ElementId> {
        None
    }
    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }
    fn request_layout(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        window: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, Self::RequestLayoutState) {
        let text = self
            .entity
            .read(cx)
            .composition
            .as_ref()
            .map_or("", |e| &e.body);
        let style = window.text_style();
        let font_size = style.font_size.to_pixels(window.rem_size());
        let mut width = px(500.);
        let lines: Vec<_> = line_spans(text)
            .into_iter()
            .map(|span| {
                let part = &text[span.clone()];
                let run = TextRun {
                    len: part.len(),
                    font: style.font(),
                    color: style.color,
                    background_color: None,
                    underline: None,
                    strikethrough: None,
                };
                let line = window.text_system().shape_line(
                    part.to_owned().into(),
                    font_size,
                    &[run],
                    None,
                );
                width = width.max(line.width + px(24.));
                (span.start, line)
            })
            .collect();
        let mut layout = Style::default();
        layout.size.width = width.into();
        layout.size.height = px(LINE * lines.len() as f32 + 16.).into();
        (window.request_layout(layout, [], cx), lines)
    }
    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        lines: &mut Self::RequestLayoutState,
        window: &mut Window,
        cx: &mut App,
    ) {
        self.entity.update(cx, |this, cx| {
            let Some(e) = this.composition.as_ref() else {
                return;
            };
            let layout = TextLayout {
                text: e.body.clone(),
                lines: lines
                    .iter()
                    .enumerate()
                    .map(|(i, (start, shaped))| Line {
                        start: *start,
                        shaped: shaped.clone(),
                        origin: point(
                            bounds.left() + px(8.),
                            bounds.top() + px(8. + LINE * i as f32),
                        ),
                    })
                    .collect(),
            };
            if this.composition_reveal
                && this.field == Field::CompositionBody
                && this.focus.is_focused(window)
            {
                let caret = layout.caret(this.composition_caret().min(layout.text.len()));
                let viewport = this.composition_scroll.bounds();
                let mut offset = this.composition_scroll.offset();
                if caret.top() < viewport.top() {
                    offset.y += viewport.top() - caret.top();
                }
                if caret.bottom() > viewport.bottom() {
                    offset.y -= caret.bottom() - viewport.bottom();
                }
                if caret.left() < viewport.left() {
                    offset.x += viewport.left() - caret.left();
                }
                if caret.right() > viewport.right() {
                    offset.x -= caret.right() - viewport.right();
                }
                this.composition_scroll.set_offset(offset);
                this.composition_reveal = false;
                cx.notify();
            }
            this.composition_layout = Some(layout);
        });
    }
    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut Self::RequestLayoutState,
        _: &mut (),
        window: &mut Window,
        cx: &mut App,
    ) {
        let this = self.entity.read(cx);
        let Some(layout) = this.composition_layout.clone() else {
            return;
        };
        let active = this.composition_input_active() && this.field == Field::CompositionBody;
        let focus = this.focus.clone();
        let selection = editing::valid_selection(&layout.text, this.selection.clone());
        let marked_range = this.marked_range.clone();
        let caret = this.composition_caret().min(layout.text.len());
        if active {
            window.handle_input(
                &focus,
                ElementInputHandler::new(bounds, self.entity.clone()),
                cx,
            );
        }
        for line in &layout.lines {
            let end = line.start + line.shaped.len();
            if active && selection.start <= end && selection.end > line.start {
                let left = selection
                    .start
                    .saturating_sub(line.start)
                    .min(line.shaped.len());
                let right = selection
                    .end
                    .saturating_sub(line.start)
                    .min(line.shaped.len());
                let width =
                    (line.shaped.x_for_index(right) - line.shaped.x_for_index(left)).max(px(2.));
                window.paint_quad(gpui::fill(
                    Bounds::new(
                        point(line.origin.x + line.shaped.x_for_index(left), line.origin.y),
                        size(width, px(LINE)),
                    ),
                    gpui::rgba(0x7ca9d04f),
                ));
            }
            let _ = line.shaped.paint(
                line.origin,
                px(LINE),
                gpui::TextAlign::Left,
                None,
                window,
                cx,
            );
            if active
                && let Some(marked) = marked_range.as_ref()
                && marked.start <= end
                && marked.end > line.start
            {
                let left = marked
                    .start
                    .saturating_sub(line.start)
                    .min(line.shaped.len());
                let right = marked.end.saturating_sub(line.start).min(line.shaped.len());
                window.paint_quad(gpui::fill(
                    Bounds::new(
                        point(
                            line.origin.x + line.shaped.x_for_index(left),
                            line.origin.y + px(LINE - 2.),
                        ),
                        size(
                            (line.shaped.x_for_index(right) - line.shaped.x_for_index(left))
                                .max(px(1.)),
                            px(1.),
                        ),
                    ),
                    rgb(ACCENT),
                ));
            }
        }
        if active && focus.is_focused(window) {
            window.paint_quad(gpui::fill(layout.caret(caret), rgb(ACCENT)));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn actual_multiline_shape_spans_preserve_160_columns_utf8_empty_and_final_lines() {
        let text = format!("{}\n中文😀\n\n", "-".repeat(160));
        let spans = line_spans(&text);
        assert_eq!(spans.len(), 4);
        assert_eq!(spans[0], 0..160);
        assert_eq!(&text[spans[1].clone()], "中文😀");
        assert!(spans[2].is_empty() && spans[3].is_empty());
        for span in spans {
            assert!(text.is_char_boundary(span.start) && text.is_char_boundary(span.end));
        }
        assert_eq!(line_spans("").len(), 1);
        assert_eq!(line_spans("")[0], 0..0);
    }
    #[test]
    fn actual_reverse_drag_helper_tracks_utf8_caret_and_anchor() {
        let text = "A中文😀Z";
        let (range, (anchor, caret)) = drag_selection(text, text.len(), 1);
        assert_eq!(range, 1..text.len());
        assert_eq!(anchor, text.len());
        assert_eq!(caret, 1);
        assert!(anchor > caret);
        let (range, (_, caret)) = drag_selection(text, anchor, 5);
        assert_eq!(caret, 4);
        assert_eq!(&text[range], "文😀Z");
        let (range, (anchor, caret)) = drag_selection(text, 1, text.len());
        assert_eq!(range, 1..text.len());
        assert!(anchor < caret);
    }
}
