//! A local action filter: never a chat draft, request payload or credential buffer.
use miochat_gpui::{composition::Origin, editing, session::Panel};
use std::ops::Range;

#[derive(Clone, Debug, PartialEq, Eq)]
pub(super) enum Item {
    Action(usize),
    MemberModerator { name: String, grant: bool },
}
pub(super) fn projected_items(panel: &Panel, query: &str) -> Vec<Item> {
    let mut items: Vec<_> = visible_actions(panel, query)
        .into_iter()
        .map(Item::Action)
        .collect();
    let query = query.trim().to_lowercase();
    // These are pre-existing native controls explicitly authorized by server
    // capability flags, not actions inferred from display-only member names.
    for m in &panel.members {
        for (allowed, grant, label, head) in [
            (m.can_mod, true, "设为管理员", "/roommod"),
            (m.can_unmod, false, "撤销管理员", "/unmod"),
        ] {
            let command = format!("{head} {}", m.name);
            if allowed
                && !panel
                    .actions
                    .iter()
                    .any(|a| !a.fill && a.command == command)
                && (query.is_empty()
                    || format!("{label} @{} {command}", m.name)
                        .to_lowercase()
                        .contains(&query))
            {
                items.push(Item::MemberModerator {
                    name: m.name.clone(),
                    grant,
                });
            }
        }
    }
    items
}
pub(super) fn visible_actions(panel: &Panel, query: &str) -> Vec<usize> {
    let query = query.trim().to_lowercase();
    panel
        .actions
        .iter()
        .enumerate()
        .filter_map(|(index, action)| {
            (query.is_empty()
                || format!("{} {}", action.label, action.command)
                    .to_lowercase()
                    .contains(&query))
            .then_some(index)
        })
        .collect()
}
fn primary_action(panel: &Panel, command: &str) -> Option<usize> {
    panel
        .actions
        .iter()
        .position(|a| !a.fill && a.command == command)
}
/// Fix the native representative before filtering; a matching duplicate remains
/// an explicit row and must not acquire a second card/button presentation.
pub(super) fn card_action_original(panel: &Panel, query: &str, command: &str) -> Option<usize> {
    let original = primary_action(panel, command)?;
    visible_actions(panel, query)
        .contains(&original)
        .then_some(original)
}
pub(super) fn member_item(panel: &Panel, query: &str, name: &str, grant: bool) -> Option<Item> {
    if !panel
        .members
        .iter()
        .any(|m| m.name == name && if grant { m.can_mod } else { m.can_unmod })
    {
        return None;
    }
    let command = format!("{} {name}", if grant { "/roommod" } else { "/unmod" });
    if primary_action(panel, &command).is_some() {
        return card_action_original(panel, query, &command).map(Item::Action);
    }
    let item = Item::MemberModerator {
        name: name.into(),
        grant,
    };
    projected_items(panel, query)
        .contains(&item)
        .then_some(item)
}
/// A projected action is rendered either by its native card/button or an action
/// row, never dropped merely because its command has a directory prefix.
pub(super) fn native_card_action(panel: &Panel, index: usize) -> bool {
    let Some(action) = panel.actions.get(index) else {
        return false;
    };
    if action.fill || primary_action(panel, &action.command) != Some(index) {
        return false; // Duplicate/fill actions remain explicit rows, not hidden targets.
    }
    panel
        .rooms
        .iter()
        .any(|r| action.command == format!("/join {}", r.name))
        || panel.community.as_ref().is_some_and(|v| {
            v.objects
                .iter()
                .any(|o| action.command == format!("/object {}", o.id))
        })
        || panel.members.iter().any(|m| {
            (m.can_remove && action.command == format!("/remove {}", m.name))
                || (m.can_mod && action.command == format!("/roommod {}", m.name))
                || (m.can_unmod && action.command == format!("/unmod {}", m.name))
        })
}

pub(super) struct Filter {
    pub revision: u64,
    owner: String,
    room: String,
    generation: u64,
    origin: Origin,
    pub value: String,
    pub selection: Range<usize>,
    pub marked: Option<Range<usize>>,
    pub cursor: usize,
    input_cursor: Option<(usize, usize)>,
    pub drag: Option<usize>,
    pub reveal: bool,
}
impl Filter {
    pub fn new(revision: u64, owner: &str, room: &str, generation: u64, origin: &Origin) -> Self {
        Self {
            revision,
            owner: owner.into(),
            room: room.into(),
            generation,
            origin: origin.clone(),
            value: String::new(),
            selection: 0..0,
            marked: None,
            cursor: 0,
            input_cursor: None,
            drag: None,
            reveal: true,
        }
    }
    pub fn bound(
        &self,
        revision: u64,
        owner: &str,
        room: &str,
        generation: u64,
        origin: Option<&Origin>,
    ) -> bool {
        self.revision == revision
            && self.owner == owner
            && self.room == room
            && self.generation == generation
            && origin == Some(&self.origin)
    }
    pub fn replace(&mut self, range: Option<Range<usize>>, text: &str) -> bool {
        if text.chars().any(char::is_control) {
            return false;
        }
        let mut value = self.value.clone();
        let mut selection = self.selection.clone();
        let mut marked = self.marked.clone();
        if !editing::replace(&mut value, &mut selection, &mut marked, range, text, 512)
            || value.len() > 512
        {
            return false;
        }
        if value != self.value {
            self.cursor = 0;
        }
        self.value = value;
        self.selection = selection;
        self.input_cursor = None;
        self.reveal = true;
        self.marked = marked;
        true
    }
    pub fn replace_marked(
        &mut self,
        range: Option<Range<usize>>,
        text: &str,
        selected: Option<Range<usize>>,
    ) -> bool {
        let start = range
            .clone()
            .map(|r| editing::byte_range(&self.value, r).start)
            .or_else(|| self.marked.as_ref().map(|r| r.start))
            .unwrap_or(self.selection.start);
        if !self.replace(range, text) {
            return false;
        }
        self.marked = (!text.is_empty()).then_some(start..start + text.len());
        self.selection = selected
            .map(|r| {
                let r = editing::byte_range(text, r);
                start + r.start..start + r.end
            })
            .unwrap_or(start + text.len()..start + text.len());
        true
    }
    pub fn caret(&self) -> usize {
        self.input_cursor
            .filter(|(a, c)| self.selection == ((*a).min(*c)..(*a).max(*c)))
            .map_or(self.selection.end, |(_, c)| c)
    }
    pub fn anchor(&self) -> usize {
        self.input_cursor
            .filter(|(a, c)| self.selection == ((*a).min(*c)..(*a).max(*c)))
            .map_or(self.selection.start, |(a, _)| a)
    }
    pub fn select(&mut self, anchor: usize, caret: usize) {
        let anchor = editing::valid_selection(&self.value, anchor..anchor).start;
        let caret = editing::valid_selection(&self.value, caret..caret).start;
        self.selection = anchor.min(caret)..anchor.max(caret);
        self.input_cursor = Some((anchor, caret));
        self.reveal = true;
    }
    pub fn navigate(&mut self, key: &str, extend: bool) {
        let at = self.caret();
        let next = match key {
            "left" if !extend && !self.selection.is_empty() => self.selection.start,
            "right" if !extend && !self.selection.is_empty() => self.selection.end,
            "left" => editing::previous(&self.value, at),
            "right" => editing::next(&self.value, at),
            "home" => 0,
            "end" => self.value.len(),
            _ => return,
        };
        self.select(if extend { self.anchor() } else { next }, next);
    }
    pub fn step(&mut self, count: usize, forward: bool) {
        self.cursor = if count == 0 {
            0
        } else if forward {
            self.cursor.saturating_add(1).min(count - 1)
        } else {
            self.cursor.saturating_sub(1).min(count - 1)
        };
    }
    pub fn selected(&self, panel: &Panel) -> Option<Item> {
        projected_items(panel, &self.value)
            .get(self.cursor)
            .cloned()
    }
}

pub(super) struct InputContext<'a> {
    pub revision: u64,
    pub owner: &'a str,
    pub room: &'a str,
    pub generation: u64,
    pub origin: Option<&'a Origin>,
    pub enabled: bool,
}
pub(super) fn input_eligible(
    filter: Option<&Filter>,
    requested: u64,
    context: &InputContext<'_>,
) -> bool {
    context.enabled
        && filter.is_some_and(|f| {
            f.revision == requested
                && f.bound(
                    context.revision,
                    context.owner,
                    context.room,
                    context.generation,
                    context.origin,
                )
        })
}

pub(super) fn admitted_item(
    filter: Option<&Filter>,
    requested: u64,
    context: &InputContext<'_>,
    panel: &Panel,
    item: &Item,
    pending: bool,
) -> bool {
    !pending
        && input_eligible(filter, requested, context)
        && filter.is_some_and(|f| projected_items(panel, &f.value).contains(item))
}

#[cfg(test)]
mod tests {
    use super::*;
    use miochat_gpui::session::PanelAction;
    fn panel(actions: &[(&str, &str)]) -> Panel {
        Panel {
            actions: actions
                .iter()
                .map(|(label, command)| PanelAction {
                    label: (*label).into(),
                    command: (*command).into(),
                    fill: false,
                })
                .collect(),
            ..Default::default()
        }
    }
    #[test]
    fn actual_visible_projection_keeps_card_keyboard_actions_and_original_indices() {
        let mut p = panel(&[
            ("移除 bob", "/remove bob"),
            ("通知", "/notifications"),
            ("中文😀", "/help"),
        ]);
        assert_eq!(visible_actions(&p, ""), vec![0, 1, 2]);
        assert_eq!(visible_actions(&p, "HELP"), vec![2]);
        assert_eq!(visible_actions(&p, "中文😀"), vec![2]);
        assert!(visible_actions(&p, "missing").is_empty());
        assert!(!native_card_action(&p, 0)); // Unmatched actions remain visible rows.
        p.members.push(miochat_gpui::session::RoomMember {
            name: "bob".into(),
            can_remove: true,
            ..Default::default()
        });
        assert!(native_card_action(&p, 0));
        assert_eq!(visible_actions(&p, "bob"), vec![0]); // Native button is still a keyboard target.
        p.kind = "rooms".into();
        p.rooms.push(miochat_gpui::session::RoomCard {
            name: "lobby".into(),
            ..Default::default()
        });
        p.actions.push(PanelAction {
            label: "大厅".into(),
            command: "/join lobby".into(),
            fill: false,
        });
        assert!(native_card_action(&p, 3));
        assert_eq!(visible_actions(&p, "lobby"), vec![3]);
        p.actions.push(p.actions[3].clone());
        p.actions[4].label = "duplicate-only".into();
        assert_eq!(card_action_original(&p, "lobby", "/join lobby"), Some(3));
        assert_eq!(visible_actions(&p, "duplicate-only"), vec![4]);
        assert_eq!(
            card_action_original(&p, "duplicate-only", "/join lobby"),
            None
        );
        assert!(!native_card_action(&p, 4));
        p.actions[4].fill = true;
        assert!(!native_card_action(&p, 4));
        p.actions.pop();
        let o = Origin::synthetic("fixture", 1);
        let mut f = Filter::new(1, "alice", "#lobby", 3, &o);
        f.replace(None, "lobby");
        assert_eq!(f.selected(&p), Some(Item::Action(3)));
    }
    #[test]
    fn actual_filter_is_bound_to_replacement_origin_owner_scope_and_generation() {
        let o = Origin::synthetic("fixture", 1);
        let mut f = Filter::new(1, "alice", "#lobby", 3, &o);
        assert!(f.replace(None, "私人查询"));
        assert!(f.bound(1, "alice", "#lobby", 3, Some(&o)));
        assert!(!f.bound(2, "alice", "#lobby", 3, Some(&o)));
        assert!(!f.bound(1, "bob", "#lobby", 3, Some(&o)));
        assert!(!f.bound(1, "alice", "@bob", 3, Some(&o)));
        assert!(!f.bound(1, "alice", "#lobby", 4, Some(&o)));
        assert!(!f.bound(1, "alice", "#lobby", 3, None));
        assert!(!f.bound(
            1,
            "alice",
            "#lobby",
            3,
            Some(&Origin::synthetic("other", 1))
        ));
        let fresh = Filter::new(2, "alice", "#lobby", 3, &o);
        assert!(fresh.value.is_empty());
        assert_eq!(fresh.selection, 0..0);
        assert_eq!(fresh.cursor, 0);
    }
    #[test]
    fn actual_local_filter_utf16_marking_and_limits_do_not_share_chat_input() {
        let o = Origin::synthetic("fixture", 1);
        let mut f = Filter::new(1, "alice", "#lobby", 3, &o);
        let chat = ("保留聊天草稿😀".to_string(), 2..5, 17i64);
        assert!(f.replace(None, "中😀文"));
        assert_eq!(f.selection, 10..10);
        assert!(f.replace_marked(Some(1..3), "测试", Some(0..1)));
        assert_eq!(f.value, "中测试文");
        assert_eq!(f.marked, Some(3..9));
        assert_eq!(f.selection, 3..6);
        f.marked = None;
        let before = (f.value.clone(), f.selection.clone());
        assert!(!f.replace(None, "\n"));
        assert!(!f.replace(None, &"x".repeat(600)));
        assert!(!f.replace(None, &"中".repeat(171)));
        assert_eq!((f.value.clone(), f.selection.clone()), before);
        assert_eq!(chat, ("保留聊天草稿😀".to_string(), 2..5, 17));
    }
    #[test]
    fn actual_filtered_navigation_empty_results_never_reuses_old_index() {
        let o = Origin::synthetic("fixture", 1);
        let mut f = Filter::new(1, "alice", "#lobby", 3, &o);
        let p = panel(&[("one", "/help"), ("two", "/stats")]);
        f.step(2, true);
        assert_eq!(f.selected(&p), Some(Item::Action(1)));
        f.step(2, true);
        assert_eq!(f.cursor, 1);
        assert!(f.replace(None, "不存在"));
        assert_eq!(f.cursor, 0);
        f.step(0, true);
        assert_eq!(f.selected(&p), None);
        f.selection = 0..f.value.len();
        assert!(f.replace(None, "two"));
        assert_eq!(f.selected(&p), Some(Item::Action(1)));
    }
    #[test]
    fn actual_projection_never_invents_write_from_display_only_member_or_stale_revision() {
        let mut p = Panel::default();
        p.members.push(miochat_gpui::session::RoomMember {
            name: "bob".into(),
            ..Default::default()
        });
        assert!(projected_items(&p, "").is_empty());
        p.members[0].can_mod = true;
        assert_eq!(
            projected_items(&p, "bob"),
            vec![Item::MemberModerator {
                name: "bob".into(),
                grant: true
            }]
        );
        assert_eq!(
            member_item(&p, "bob", "bob", true),
            Some(Item::MemberModerator {
                name: "bob".into(),
                grant: true
            })
        );
        p.actions = vec![
            PanelAction {
                label: "primary".into(),
                command: "/roommod bob".into(),
                fill: false,
            },
            PanelAction {
                label: "duplicate-only".into(),
                command: "/roommod bob".into(),
                fill: false,
            },
        ];
        assert_eq!(member_item(&p, "bob", "bob", true), Some(Item::Action(0)));
        assert_eq!(visible_actions(&p, "duplicate-only"), vec![1]);
        assert_eq!(member_item(&p, "duplicate-only", "bob", true), None);
        assert!(!native_card_action(&p, 1));
        assert_eq!(projected_items(&p, "duplicate-only"), vec![Item::Action(1)]);
        p.actions.clear();
        let old = projected_items(&p, "");
        p.members[0].name = "carol".into();
        assert_eq!(old.len(), projected_items(&p, "").len());
        assert_ne!(old, projected_items(&p, ""));
        p.members[0].can_mod = false;
        assert!(projected_items(&p, "").is_empty());
    }
    #[test]
    fn actual_filter_input_gate_rejects_cancelled_and_same_count_replaced_callbacks() {
        let origin = Origin::synthetic("fixture", 1);
        let mut state = Some(Filter::new(8, "alice", "#lobby", 3, &origin));
        let mut context = InputContext {
            revision: 8,
            owner: "alice",
            room: "#lobby",
            generation: 3,
            origin: Some(&origin),
            enabled: true,
        };
        assert!(input_eligible(state.as_ref(), 8, &context));
        state = None;
        context.revision = 9;
        assert!(!input_eligible(state.as_ref(), 8, &context));
        state = Some(Filter::new(10, "alice", "#lobby", 3, &origin));
        context.revision = 10;
        assert!(!input_eligible(state.as_ref(), 8, &context));
        context.enabled = false;
        assert!(!input_eligible(state.as_ref(), 10, &context));
        assert!(state.unwrap().value.is_empty());
    }
    #[test]
    fn actual_filter_reverse_selection_and_shift_navigation_follow_utf8_caret() {
        let origin = Origin::synthetic("fixture", 1);
        let mut f = Filter::new(8, "alice", "#lobby", 3, &origin);
        assert!(f.replace(None, "A中文😀Z"));
        f.select(f.value.len(), 1);
        assert_eq!(f.caret(), 1);
        assert_eq!(&f.value[f.selection.clone()], "中文😀Z");
        f.navigate("right", true);
        assert_eq!(f.caret(), 4);
        assert_eq!(&f.value[f.selection.clone()], "文😀Z");
        f.navigate("left", true);
        assert_eq!(f.caret(), 1);
        f.navigate("right", false);
        assert_eq!(f.selection, f.value.len()..f.value.len());
        f.select(0, 3); // An old/intermediate byte is floored, never split UTF8.
        assert_eq!(f.caret(), 1);
        assert!(f.replace(None, "测试"));
        assert_eq!(f.caret(), "测试".len());
    }
    #[test]
    fn actual_pointer_and_keyboard_gate_rechecks_filter_revision_item_and_pending() {
        let origin = Origin::synthetic("fixture", 1);
        let mut f = Filter::new(8, "alice", "#lobby", 3, &origin);
        let mut context = InputContext {
            revision: 8,
            owner: "alice",
            room: "#lobby",
            generation: 3,
            origin: Some(&origin),
            enabled: true,
        };
        let p = panel(&[("old", "/help"), ("new", "/stats")]);
        let item = Item::Action(0);
        assert!(admitted_item(Some(&f), 8, &context, &p, &item, false));
        assert!(!admitted_item(Some(&f), 8, &context, &p, &item, true));
        assert!(f.replace(None, "new"));
        assert!(!admitted_item(Some(&f), 8, &context, &p, &item, false));
        let selected = f.selected(&p).unwrap();
        assert!(admitted_item(Some(&f), 8, &context, &p, &selected, false));
        context.revision = 9;
        assert!(!admitted_item(Some(&f), 8, &context, &p, &selected, false));
        assert!(!admitted_item(None, 8, &context, &p, &selected, false));
    }
}
