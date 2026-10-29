//! Curated Unicode only: no network, downloaded assets, automatic send or text learning.
use super::*;
const EMOJI: &[(&str, &str)] = &[
    ("😀", "微笑"),
    ("😂", "大笑"),
    ("🥰", "喜爱"),
    ("😎", "酷"),
    ("🤔", "思考"),
    ("😭", "哭泣"),
    ("😮", "惊讶"),
    ("😴", "睡觉"),
    ("👍", "赞"),
    ("👎", "不赞同"),
    ("👏", "鼓掌"),
    ("🙏", "感谢"),
    ("❤️", "爱心"),
    ("💔", "心碎"),
    ("🎉", "庆祝"),
    ("✨", "闪亮"),
    ("🔥", "火"),
    ("✅", "完成"),
    ("❓", "问题"),
    ("💡", "想法"),
    ("☕", "咖啡"),
    ("🍰", "蛋糕"),
    ("🌈", "彩虹"),
    ("🐱", "猫"),
    ("🐶", "狗"),
    ("🚀", "火箭"),
    ("💪", "加油"),
    ("👋", "招手"),
    ("🫶", "比心"),
    ("🎂", "生日"),
    ("🌙", "月亮"),
    ("☀️", "阳光"),
];
#[derive(Clone)]
struct Binding {
    owner: String,
    origin: miochat_gpui::composition::Origin,
    generation: u64,
}
impl Binding {
    fn matches(
        &self,
        owner: &str,
        origin: Option<&miochat_gpui::composition::Origin>,
        generation: u64,
    ) -> bool {
        self.owner == owner && origin == Some(&self.origin) && self.generation == generation
    }
}
pub(super) struct Picker {
    binding: Binding,
    room: String,
    body: String,
    selection: Range<usize>,
    serial: u64,
    cursor: usize,
    items: Vec<usize>,
}
#[derive(Default)]
pub(super) struct EmojiState {
    binding: Option<Binding>,
    recent: Vec<usize>,
    picker: Option<Picker>,
    serial: u64,
}
fn recent_insert(recent: &mut Vec<usize>, id: usize) {
    if id >= EMOJI.len() {
        return;
    }
    recent.retain(|v| *v != id);
    recent.insert(0, id);
    recent.truncate(24);
}
impl MioChatApp {
    pub(super) fn close_emoji(&mut self) {
        self.emoji.picker = None;
        self.emoji.serial = self.emoji.serial.wrapping_add(1);
    }
    pub(super) fn reset_emoji(&mut self) {
        self.emoji = EmojiState::default();
    }
    pub(super) fn emoji_open(&self) -> bool {
        self.emoji.picker.is_some()
    }
    pub(super) fn emoji_bound(&self) -> bool {
        self.emoji.picker.as_ref().is_some_and(|p| {
            self.screen == Screen::Chat
                && !self.must_reset
                && p.binding
                    .matches(&self.user, self.session_origin.as_ref(), self.generation)
                && p.room == self.room
                && p.body == self.composer
                && p.selection == self.selection
                && self.marked_range.is_none()
        })
    }
    pub(super) fn poll_emoji(&mut self) {
        if self.emoji_open() && !self.emoji_bound() {
            self.close_emoji();
        }
    }
    pub(super) fn open_emoji(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if !self.ordinary_chat_context_allowed()
            || !input_history::safe_text(&self.composer)
            || self.persistence.quarantined()
        {
            return;
        }
        let Some(origin) = self.session_origin.clone() else {
            return;
        };
        if self
            .emoji
            .binding
            .as_ref()
            .is_none_or(|b| !b.matches(&self.user, Some(&origin), self.generation))
        {
            self.reset_emoji();
        }
        let binding = Binding {
            owner: self.user.clone(),
            origin,
            generation: self.generation,
        };
        self.emoji.binding = Some(binding.clone());
        self.begin_feature_navigation();
        self.emoji.serial = self.emoji.serial.wrapping_add(1);
        let mut items = self.emoji.recent.clone();
        for id in 0..EMOJI.len() {
            if !items.contains(&id) {
                items.push(id);
            }
        }
        self.emoji.picker = Some(Picker {
            binding,
            room: self.room.clone(),
            body: self.composer.clone(),
            selection: self.selection.clone(),
            serial: self.emoji.serial,
            cursor: 0,
            items,
        });
        self.menu_focus.focus(window, cx);
        cx.notify();
    }
    fn choose_emoji(
        &mut self,
        id: usize,
        serial: u64,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if !self.emoji_bound()
            || self
                .emoji
                .picker
                .as_ref()
                .is_none_or(|p| p.serial != serial || !p.items.contains(&id))
        {
            return;
        }
        let Some((text, _)) = EMOJI.get(id) else {
            return;
        };
        self.close_emoji();
        // Uses the actual composer boundary, including reserved-secret and length guards.
        if self.replace_current(None, text) {
            recent_insert(&mut self.emoji.recent, id);
        }
        self.focus.focus(window, cx);
        cx.notify();
    }
    pub(super) fn emoji_key(
        &mut self,
        e: &KeyDownEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> bool {
        if !self.emoji_open() {
            let m = &e.keystroke.modifiers;
            if e.keystroke.key == "e"
                && m.control
                && m.shift
                && !m.platform
                && !m.alt
                && !m.function
                && !e.is_held
                && !e.prefer_character_input
            {
                self.open_emoji(window, cx);
                return self.emoji_open();
            }
            return false;
        }
        if !self.emoji_bound() {
            self.close_emoji();
            cx.notify();
            cx.stop_propagation();
            return true;
        }
        if !e.is_held && !e.prefer_character_input {
            let m = &e.keystroke.modifiers;
            let key = e.keystroke.key.as_str();
            if !m.modified() && key == "escape" {
                self.close_emoji();
                self.focus.focus(window, cx);
            } else if !m.control && !m.platform && !m.alt && !m.function {
                let p = self.emoji.picker.as_mut().unwrap();
                let n = p.items.len();
                match key {
                    "left" | "up" => p.cursor = (p.cursor + n - 1) % n,
                    "right" | "down" => p.cursor = (p.cursor + 1) % n,
                    "tab" => p.cursor = (p.cursor + if m.shift { n - 1 } else { 1 }) % n,
                    "enter" if !m.shift => {
                        let id = p.items[p.cursor];
                        let serial = p.serial;
                        self.choose_emoji(id, serial, window, cx);
                    }
                    _ => {}
                }
            }
        }
        cx.notify();
        cx.stop_propagation();
        true
    }
    pub(super) fn emoji_popup(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let p = self.emoji.picker.as_ref().unwrap();
        let serial = p.serial;
        div()
            .id("emoji-picker")
            .absolute()
            .right(px(24.))
            .bottom(px(140.))
            .w(px(380.))
            .p(px(16.))
            .rounded(px(12.))
            .bg(self.raised_color())
            .border_1()
            .border_color(self.preference_border())
            .shadow_lg()
            .occlude()
            .track_focus(&self.menu_focus)
            .on_click(|_, _, cx| cx.stop_propagation())
            .flex()
            .flex_col()
            .gap(px(10.))
            .child("Emoji · 仅插入草稿，需另行发送")
            .child("最近使用优先 · 方向键/Tab 选择 · Enter 插入 · Esc 取消")
            .child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap(px(6.))
                    .children(p.items.iter().enumerate().map(|(at, id)| {
                        let id = *id;
                        Self::button(
                            gpui::SharedString::from(format!("emoji-{id}")),
                            format!("{} {}", EMOJI[id].0, EMOJI[id].1),
                        )
                        .when(at == p.cursor, |d| d.bg(rgb(0xdde8ef)))
                        .on_click(cx.listener(
                            move |this, _, window, cx| this.choose_emoji(id, serial, window, cx),
                        ))
                    })),
            )
            .child(Self::button("emoji-close", "取消").on_click(cx.listener(
                |this, _, window, cx| {
                    this.close_emoji();
                    this.focus.focus(window, cx);
                    cx.notify();
                },
            )))
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn unicode_emoji_recent_contains_only_static_ids_and_is_bounded() {
        let mut r = vec![];
        for i in 0..EMOJI.len() {
            recent_insert(&mut r, i);
        }
        assert_eq!(r.len(), 24);
        let old = r.clone();
        recent_insert(&mut r, usize::MAX);
        assert_eq!(r, old);
        recent_insert(&mut r, 31);
        assert_eq!(r[0], 31);
        assert_eq!(r.iter().filter(|v| **v == 31).count(), 1);
        assert!(EMOJI.iter().all(|(t, _)| !t.is_empty() && !t.contains('/')));
    }
    #[test]
    fn unicode_emoji_inserts_at_utf8_selection_without_sending_or_learning_text() {
        let mut body = "中文原稿".to_owned();
        let mut s = 3..6;
        let mut marked = None;
        assert!(miochat_gpui::word::replace_composer(
            &mut body,
            &mut s,
            &mut marked,
            None,
            EMOJI[12].0
        ));
        assert_eq!(body, "中❤️原稿");
        assert!(input_history::safe_text(&body));
        assert!(!input_history::safe_text("/passwd synthetic-secret"));
        assert!(!input_history::safe_text(
            "/pass\u{200b}wd synthetic-secret"
        ));
    }
    #[test]
    fn unicode_emoji_recent_binding_rejects_other_account_server_and_generation() {
        let o = miochat_gpui::composition::Origin::synthetic("server-a", 1);
        let b = Binding {
            owner: "alice".into(),
            origin: o.clone(),
            generation: 4,
        };
        assert!(b.matches("alice", Some(&o), 4));
        assert!(!b.matches("bob", Some(&o), 4));
        assert!(!b.matches("alice", Some(&o), 5));
        assert!(!b.matches(
            "alice",
            Some(&miochat_gpui::composition::Origin::synthetic("server-b", 1)),
            4
        ));
        assert!(!b.matches("alice", None, 4));
    }
}
