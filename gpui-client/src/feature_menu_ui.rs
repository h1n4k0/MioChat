use super::feature_menu::{Filter, Item, projected_items};
use super::*;
use gpui::{MouseButton, ShapedLine, TextRun, point};

fn activation_allowed(event: &KeyDownEvent, marked: bool, pending: bool) -> bool {
    let m = &event.keystroke.modifiers;
    event.keystroke.key == "enter"
        && !event.is_held
        && !event.prefer_character_input
        && !marked
        && !pending
        && !m.control
        && !m.platform
        && !m.alt
        && !m.shift
        && !m.function
}

impl MioChatApp {
    pub(super) fn set_feature_panel(&mut self, panel: Option<Panel>) {
        self.cancel_completion();
        self.feature_revision = self.feature_revision.wrapping_add(1);
        self.feature_filter = if panel.is_some() && self.screen == Screen::Chat {
            self.session_origin.as_ref().map(|o| {
                Filter::new(
                    self.feature_revision,
                    &self.user,
                    &self.room,
                    self.generation,
                    o,
                )
            })
        } else {
            None
        };
        self.feature_view = panel.as_ref().map(|_| {
            let handle = gpui::ScrollHandle::new();
            let anchor = gpui::ScrollAnchor::for_handle(handle.clone());
            (handle, anchor)
        });
        self.feature_filter_scroll = gpui::ScrollHandle::new();
        self.feature_panel = panel;
    }
    pub(super) fn dismiss_feature_panel(&mut self) {
        miochat_gpui::moderation::dismiss(&mut self.moderation_epoch, &mut self.feature_panel);
        self.set_feature_panel(None);
    }
    fn feature_filter_context(&self) -> feature_menu::InputContext<'_> {
        let enabled = self.screen == Screen::Chat
            && !self.must_reset
            && self
                .feature_panel
                .as_ref()
                .is_some_and(|p| p.scope.is_empty() || p.scope == self.room)
            && self.direct_form.is_none()
            && !self.composition_visible()
            && self.word_start.is_none()
            && self.profile.is_none()
            && !self.center_open
            && !self.preferences_open
            && !self.appearance_menu
            && !self.mock_menu
            && self.profile_editor.is_none()
            && self.room_form.is_none()
            && !self.secret_admin_visible()
            && !self.maintenance_visible();
        feature_menu::InputContext {
            revision: self.feature_revision,
            owner: &self.user,
            room: &self.room,
            generation: self.generation,
            origin: self.session_origin.as_ref(),
            enabled,
        }
    }
    pub(super) fn feature_filter_active(&self, revision: u64) -> bool {
        feature_menu::input_eligible(
            self.feature_filter.as_ref(),
            revision,
            &self.feature_filter_context(),
        )
    }
    pub(super) fn feature_input_poll(&mut self) {
        if self
            .feature_filter
            .as_ref()
            .is_some_and(|f| !self.feature_filter_active(f.revision))
        {
            self.feature_filter = None;
        }
    }
    pub(super) fn feature_anchor_for(&self, item: &Item) -> Option<gpui::ScrollAnchor> {
        self.feature_item_selected(item)
            .then(|| self.feature_view.as_ref().map(|(_, a)| a.clone()))
            .flatten()
    }
    pub(super) fn reveal_feature_selection(&self, window: &mut Window, _: &mut Context<Self>) {
        if let Some((_, anchor)) = &self.feature_view {
            let anchor = anchor.clone();
            // The first frame paints the newly selected nested card/row. The
            // second scroll request uses its new anchor. Replacement gets a new
            // handle, so an old queued request cannot scroll a different panel.
            window.on_next_frame(move |window, cx| anchor.scroll_to(window, cx));
        }
    }
    pub(super) fn feature_action_original(&self, panel: &Panel, command: &str) -> Option<usize> {
        if !self.feature_filter_active(self.feature_revision) {
            return None;
        }
        let filter = self.feature_filter.as_ref()?;
        feature_menu::card_action_original(panel, &filter.value, command)
    }
    pub(super) fn feature_original_selected(&self, original: usize) -> bool {
        self.feature_filter
            .as_ref()
            .and_then(|f| self.feature_panel.as_ref().and_then(|p| f.selected(p)))
            == Some(Item::Action(original))
    }
    pub(super) fn feature_member_item(
        &self,
        panel: &Panel,
        name: &str,
        grant: bool,
    ) -> Option<Item> {
        if !self.feature_filter_active(self.feature_revision) {
            return None;
        }
        feature_menu::member_item(panel, &self.feature_filter.as_ref()?.value, name, grant)
    }
    pub(super) fn feature_item_selected(&self, item: &Item) -> bool {
        self.feature_filter
            .as_ref()
            .and_then(|f| self.feature_panel.as_ref().and_then(|p| f.selected(p)))
            .as_ref()
            == Some(item)
    }
    pub(super) fn activate_visible_feature(
        &mut self,
        revision: u64,
        original: usize,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.activate_feature_item(revision, Item::Action(original), window, cx);
    }
    pub(super) fn activate_feature_item(
        &mut self,
        revision: u64,
        item: Item,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(panel) = self.feature_panel.as_ref() else {
            return;
        };
        if !feature_menu::admitted_item(
            self.feature_filter.as_ref(),
            revision,
            &self.feature_filter_context(),
            panel,
            &item,
            self.pending_send.is_some(),
        ) {
            return;
        }
        match item {
            Item::Action(original) => {
                let action = panel.actions[original].clone();
                self.activate_feature_action(&action, window, cx);
            }
            Item::MemberModerator { name, grant } => {
                self.open_room_form(RoomAction::Moderator { name, grant }, cx)
            }
        }
    }
    pub(super) fn feature_key(
        &mut self,
        event: &KeyDownEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        if self.feature_panel.is_none() {
            return false;
        }
        let key = event.keystroke.key.as_str();
        let m = &event.keystroke.modifiers;
        let plain = !m.control && !m.platform && !m.alt && !m.shift && !m.function;
        let revision = self.feature_revision;
        if !self.feature_filter_active(revision) {
            if key == "escape" && plain {
                self.dismiss_feature_panel();
                self.focus.focus(window, cx);
                cx.notify();
            }
            cx.stop_propagation();
            return true;
        }
        let marked = self.feature_filter.as_ref().unwrap().marked.is_some();
        if key == "escape" && plain && !marked {
            self.dismiss_feature_panel();
            self.focus.focus(window, cx);
            cx.notify();
            cx.stop_propagation();
            return true;
        }
        if activation_allowed(event, marked, self.pending_send.is_some()) {
            let original = self
                .feature_filter
                .as_ref()
                .and_then(|f| f.selected(self.feature_panel.as_ref().unwrap()));
            if let Some(original) = original {
                self.activate_feature_item(revision, original, window, cx);
            }
            cx.notify();
            cx.stop_propagation();
            return true;
        }
        // Composition belongs to the dedicated input handler, never a menu action.
        if marked || event.prefer_character_input {
            return true;
        }
        if key == "enter" {
            cx.stop_propagation();
            return true;
        }
        let filter = self.feature_filter.as_mut().unwrap();
        let command = (m.control ^ m.platform) && !m.alt && !m.shift && !m.function;
        let handled = if command && key == "a" {
            filter.select(0, filter.value.len());
            true
        } else if command && matches!(key, "c" | "x") {
            if !filter.selection.is_empty() {
                cx.write_to_clipboard(gpui::ClipboardItem::new_string(
                    filter.value[filter.selection.clone()].to_string(),
                ));
                if key == "x" {
                    filter.replace(None, "");
                }
            }
            true
        } else if command && key == "v" {
            if let Some(text) = cx.read_from_clipboard().and_then(|v| v.text()) {
                filter.replace(None, &text);
            }
            true
        } else if (plain && matches!(key, "up" | "down"))
            || (key == "tab" && !m.control && !m.platform && !m.alt && !m.function)
        {
            let count = projected_items(self.feature_panel.as_ref().unwrap(), &filter.value).len();
            filter.step(count, key == "down" || key == "tab" && !m.shift);
            true
        } else if plain && matches!(key, "backspace" | "delete") {
            let old = filter.selection.clone();
            if old.is_empty() {
                if key == "backspace" {
                    filter.selection.start = editing::previous(&filter.value, old.start);
                } else {
                    filter.selection.end = editing::next(&filter.value, old.end);
                }
            }
            if !filter.replace(None, "") {
                filter.selection = old;
            }
            true
        } else if !m.control
            && !m.platform
            && !m.alt
            && !m.function
            && matches!(key, "left" | "right" | "home" | "end")
        {
            filter.navigate(key, m.shift);
            true
        } else {
            false
        };
        if handled {
            self.reveal_feature_selection(window, cx);
            cx.notify();
            cx.stop_propagation();
        }
        true // Plain characters/space continue to the dedicated native text handler.
    }
    pub(super) fn feature_filter_input(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let revision = self.feature_revision;
        let active = self.feature_filter_active(revision);
        let count = self
            .feature_filter
            .as_ref()
            .filter(|_| active)
            .map_or(0, |f| {
                projected_items(self.feature_panel.as_ref().unwrap(), &f.value).len()
            });
        let app = cx.entity();
        let handler = cx.new(|_| FilterInput {
            app,
            revision,
            layout: None,
        });
        div().flex().flex_col().gap(px(5.)).when(active, |d| {
            d.child(
                div()
                    .id("feature-filter-input")
                    .role(gpui::Role::TextInput)
                    .aria_label("筛选操作（名称或命令）")
                    .relative()
                    .h(px(42.))
                    .overflow_x_scroll()
                    .overflow_y_hidden()
                    .track_scroll(&self.feature_filter_scroll)
                    .border_1()
                    .border_color(self.preference_border())
                    .rounded(px(8.))
                    .text_size(px(14.))
                    .cursor_text()
                    .on_mouse_down(
                        MouseButton::Left,
                        cx.listener({
                            let handler = handler.clone();
                            move |this, event: &gpui::MouseDownEvent, window, cx| {
                                if !this.feature_filter_active(revision) {
                                    return;
                                }
                                let layout = handler.read(cx).layout.clone();
                                let f = this.feature_filter.as_mut().unwrap();
                                if f.marked.is_some() {
                                    return;
                                }
                                if let Some(l) = layout.filter(|l| l.text == f.value) {
                                    let at = l.byte_for_point(event.position);
                                    let anchor = if event.modifiers.shift {
                                        f.anchor()
                                    } else {
                                        at
                                    };
                                    f.drag = Some(anchor);
                                    f.select(anchor, at);
                                    this.menu_focus.focus(window, cx);
                                    cx.notify();
                                    cx.stop_propagation();
                                }
                            }
                        }),
                    )
                    .on_mouse_move(cx.listener({
                        let handler = handler.clone();
                        move |this, event: &gpui::MouseMoveEvent, _, cx| {
                            if !this.feature_filter_active(revision) {
                                return;
                            }
                            let layout = handler.read(cx).layout.clone();
                            let f = this.feature_filter.as_mut().unwrap();
                            if let (Some(anchor), Some(l)) =
                                (f.drag, layout.filter(|l| l.text == f.value))
                            {
                                f.select(anchor, l.byte_for_point(event.position));
                                cx.notify();
                            }
                        }
                    }))
                    .on_mouse_up(
                        MouseButton::Left,
                        cx.listener(move |this, _, _, _| {
                            if this.feature_filter_active(revision) {
                                this.feature_filter.as_mut().unwrap().drag = None;
                            }
                        }),
                    )
                    .on_mouse_up_out(
                        MouseButton::Left,
                        cx.listener(move |this, _, _, _| {
                            if this.feature_filter_active(revision) {
                                this.feature_filter.as_mut().unwrap().drag = None;
                            }
                        }),
                    )
                    .child(FilterCapture { handler }),
            )
            .child(
                div()
                    .text_size(px(12.))
                    .child(format!("{count} 项操作 · ↑↓/Tab选择 · Enter执行 · Esc关闭")),
            )
            .when(count == 0, |d| {
                d.child(div().text_size(px(12.)).child("没有匹配操作"))
            })
        })
    }
}

// A separate entity captures the exact panel revision. A delayed IME callback
// after cancellation/replacement can never fall through into the chat composer.
struct FilterInput {
    app: Entity<MioChatApp>,
    revision: u64,
    layout: Option<FilterLayout>,
}
impl FilterInput {
    fn active(&self, cx: &App) -> bool {
        self.app.read(cx).feature_filter_active(self.revision)
    }
}
impl EntityInputHandler for FilterInput {
    fn text_for_range(
        &mut self,
        range: Range<usize>,
        actual: &mut Option<Range<usize>>,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) -> Option<String> {
        if !self.active(cx) {
            *actual = None;
            return None;
        }
        let f = self.app.read(cx).feature_filter.as_ref().unwrap();
        let bytes = editing::byte_range(&f.value, range);
        *actual = Some(editing::utf16_range(&f.value, bytes.clone()));
        Some(f.value[bytes].to_string())
    }
    fn selected_text_range(
        &mut self,
        _: bool,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) -> Option<UTF16Selection> {
        if !self.active(cx) {
            return None;
        }
        let f = self.app.read(cx).feature_filter.as_ref().unwrap();
        Some(UTF16Selection {
            range: editing::utf16_range(&f.value, f.selection.clone()),
            reversed: f.caret() < f.anchor(),
        })
    }
    fn marked_text_range(&self, _: &mut Window, cx: &mut Context<Self>) -> Option<Range<usize>> {
        if !self.active(cx) {
            return None;
        }
        let f = self.app.read(cx).feature_filter.as_ref().unwrap();
        f.marked.clone().map(|r| editing::utf16_range(&f.value, r))
    }
    fn unmark_text(&mut self, _: &mut Window, cx: &mut Context<Self>) {
        let rev = self.revision;
        self.app.update(cx, |a, cx| {
            if a.feature_filter_active(rev) {
                a.feature_filter.as_mut().unwrap().marked = None;
                cx.notify();
            }
        });
    }
    fn replace_text_in_range(
        &mut self,
        range: Option<Range<usize>>,
        text: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let rev = self.revision;
        self.app.update(cx, |a, cx| {
            if a.feature_filter_active(rev) {
                if a.feature_filter.as_mut().unwrap().replace(range, text) {
                    a.reveal_feature_selection(window, cx);
                }
                cx.notify();
            }
        });
    }
    fn replace_and_mark_text_in_range(
        &mut self,
        range: Option<Range<usize>>,
        text: &str,
        selected: Option<Range<usize>>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let rev = self.revision;
        self.app.update(cx, |a, cx| {
            if a.feature_filter_active(rev) {
                if a.feature_filter
                    .as_mut()
                    .unwrap()
                    .replace_marked(range, text, selected)
                {
                    a.reveal_feature_selection(window, cx);
                }
                cx.notify();
            }
        });
    }
    fn bounds_for_range(
        &mut self,
        range: Range<usize>,
        _: Bounds<Pixels>,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) -> Option<Bounds<Pixels>> {
        if !self.active(cx) {
            return None;
        }
        let f = self.app.read(cx).feature_filter.as_ref().unwrap();
        let layout = self.layout.as_ref().filter(|l| l.text == f.value)?;
        Some(layout.caret(editing::byte_offset(&f.value, range.start)))
    }
    fn character_index_for_point(
        &mut self,
        point: Point<Pixels>,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) -> Option<usize> {
        if !self.active(cx) {
            return None;
        }
        let f = self.app.read(cx).feature_filter.as_ref().unwrap();
        let layout = self.layout.as_ref().filter(|l| l.text == f.value)?;
        Some(editing::utf16_offset(
            &f.value,
            layout.byte_for_point(point),
        ))
    }
}
#[derive(Clone)]
struct FilterLayout {
    text: String,
    line: ShapedLine,
    origin: Point<Pixels>,
}
impl FilterLayout {
    fn byte_for_point(&self, p: Point<Pixels>) -> usize {
        let x = p.x - self.origin.x;
        if x <= px(0.) {
            0
        } else {
            self.line.index_for_x(x).unwrap_or(self.line.len())
        }
    }
    fn caret(&self, byte: usize) -> Bounds<Pixels> {
        Bounds::new(
            point(
                self.origin.x + self.line.x_for_index(byte.min(self.line.len())),
                self.origin.y,
            ),
            size(px(2.), px(22.)),
        )
    }
}
struct FilterCapture {
    handler: Entity<FilterInput>,
}
impl IntoElement for FilterCapture {
    type Element = Self;
    fn into_element(self) -> Self {
        self
    }
}
impl Element for FilterCapture {
    type RequestLayoutState = (String, ShapedLine);
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
        let h = self.handler.read(cx);
        let text = if h.active(cx) {
            h.app
                .read(cx)
                .feature_filter
                .as_ref()
                .unwrap()
                .value
                .clone()
        } else {
            String::new()
        };
        let style = window.text_style();
        let run = TextRun {
            len: text.len(),
            font: style.font(),
            color: style.color,
            background_color: None,
            underline: None,
            strikethrough: None,
        };
        let line = window.text_system().shape_line(
            text.clone().into(),
            style.font_size.to_pixels(window.rem_size()),
            &[run],
            None,
        );
        let mut style = Style::default();
        style.size.width = (line.width + px(24.)).max(px(40.)).into();
        style.size.height = px(40.).into();
        (window.request_layout(style, [], cx), (text, line))
    }
    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        state: &mut Self::RequestLayoutState,
        window: &mut Window,
        cx: &mut App,
    ) {
        let layout = FilterLayout {
            text: state.0.clone(),
            line: state.1.clone(),
            origin: point(bounds.left() + px(10.), bounds.top() + px(8.)),
        };
        let (app, rev) = {
            let h = self.handler.read(cx);
            (h.app.clone(), h.revision)
        };
        app.update(cx, |a, cx| {
            if !a.feature_filter_active(rev) {
                return;
            }
            let f = a.feature_filter.as_mut().unwrap();
            if f.value != layout.text {
                return;
            }
            if f.reveal && a.menu_focus.is_focused(window) {
                let caret = layout.caret(f.caret());
                let viewport = a.feature_filter_scroll.bounds();
                let mut offset = a.feature_filter_scroll.offset();
                if caret.left() < viewport.left() {
                    offset.x += viewport.left() - caret.left();
                }
                if caret.right() > viewport.right() {
                    offset.x -= caret.right() - viewport.right();
                }
                a.feature_filter_scroll.set_offset(offset);
                f.reveal = false;
                cx.notify();
            }
        });
        self.handler.update(cx, |h, _| h.layout = Some(layout));
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
        let h = self.handler.read(cx);
        if !h.active(cx) {
            return;
        }
        let app = h.app.read(cx);
        let f = app.feature_filter.as_ref().unwrap();
        let Some(l) = h.layout.as_ref().filter(|l| l.text == f.value).cloned() else {
            return;
        };
        let focus = app.menu_focus.clone();
        let selection = f.selection.clone();
        let marked = f.marked.clone();
        let caret = f.caret();
        window.handle_input(
            &focus,
            ElementInputHandler::new(bounds, self.handler.clone()),
            cx,
        );
        if !selection.is_empty() {
            let left = l.caret(selection.start);
            let right = l.caret(selection.end);
            window.paint_quad(gpui::fill(
                Bounds::new(
                    left.origin,
                    size((right.left() - left.left()).max(px(2.)), px(22.)),
                ),
                gpui::rgba(0x7ca9d04f),
            ));
        }
        let _ = l
            .line
            .paint(l.origin, px(22.), gpui::TextAlign::Left, None, window, cx);
        if let Some(marked) = marked {
            let left = l.caret(marked.start);
            let right = l.caret(marked.end);
            window.paint_quad(gpui::fill(
                Bounds::new(
                    point(left.left(), left.top() + px(20.)),
                    size((right.left() - left.left()).max(px(1.)), px(1.)),
                ),
                rgb(ACCENT),
            ));
        }
        if focus.is_focused(window) {
            window.paint_quad(gpui::fill(l.caret(caret), rgb(ACCENT)));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn key(name: &str) -> KeyDownEvent {
        KeyDownEvent {
            keystroke: gpui::Keystroke::parse(name).unwrap(),
            is_held: false,
            prefer_character_input: false,
        }
    }
    #[test]
    fn actual_feature_enter_excludes_space_modifiers_ime_repeat_and_pending() {
        assert!(activation_allowed(&key("enter"), false, false));
        for name in [
            "space",
            "ctrl-enter",
            "alt-enter",
            "shift-enter",
            "cmd-enter",
            "tab",
        ] {
            assert!(!activation_allowed(&key(name), false, false), "{name}");
        }
        assert!(!activation_allowed(&key("enter"), true, false));
        assert!(!activation_allowed(&key("enter"), false, true));
        let mut e = key("enter");
        e.is_held = true;
        assert!(!activation_allowed(&e, false, false));
        e.is_held = false;
        e.prefer_character_input = true;
        assert!(!activation_allowed(&e, false, false));
    }
}
