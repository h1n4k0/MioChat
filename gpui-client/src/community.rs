use serde::Deserialize;
use std::ops::Range;

#[derive(Clone, Debug, Default, Deserialize)]
pub struct ObjectCard {
    pub id: i64,
    pub kind: String,
    pub room: String,
    pub owner: String,
    pub title: String,
    pub created: i64,
    pub closed: bool,
    pub unavailable: bool,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct PollOption {
    pub index: u64,
    pub text: String,
    pub votes: u64,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct ObjectComment {
    pub user: String,
    pub body: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct GameView {
    #[serde(default)]
    pub masked: String,
    #[serde(default)]
    pub can_guess: bool,
    #[serde(default)]
    pub revision: i64,
    #[serde(default)]
    pub can_accept: bool,
    #[serde(default)]
    pub can_move: bool,
    #[serde(default)]
    pub max: u64,
    #[serde(default)]
    pub wrong: u64,
    #[serde(default)]
    pub guesses: Vec<String>,
    #[serde(default)]
    pub secret: String,
    #[serde(default)]
    pub x: String,
    #[serde(default)]
    pub o: String,
    #[serde(default)]
    pub turn: String,
    #[serde(default)]
    pub winner: String,
    #[serde(default)]
    pub cells: Vec<String>,
    #[serde(default)]
    pub accepted: bool,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct ObjectDetail {
    pub card: ObjectCard,
    #[serde(default)]
    pub body: String,
    #[serde(default)]
    pub when: i64,
    #[serde(default)]
    pub when_text: String,
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub options: Vec<PollOption>,
    #[serde(default)]
    pub participants: Vec<String>,
    #[serde(default)]
    pub participant_count: u64,
    #[serde(default)]
    pub next_participant: String,
    #[serde(default)]
    pub comments: Vec<ObjectComment>,
    pub game: Option<GameView>,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct CommunityView {
    #[serde(default)]
    pub room: String,
    #[serde(default)]
    pub objects: Vec<ObjectCard>,
    pub detail: Option<ObjectDetail>,
    #[serde(default)]
    pub daily_topic: String,
    #[serde(default)]
    pub daily_date: String,
}
/// Art is data, not terminal output. Preserve all source spaces and line breaks.
pub fn ascii_art_text(body: &str) -> String {
    body.to_string()
}

pub fn is_read_command(command: &str) -> bool {
    command.split_whitespace().next().is_some_and(|c| {
        matches!(
            c.to_ascii_lowercase().as_str(),
            "/board" | "/gallery" | "/events" | "/results" | "/participants" | "/daily" | "/object"
        )
    })
}

/// Explicit local insertion, never a send or whole-draft replacement. Refuse an
/// unfinished IME composition or stale view rather than losing pending input.
pub fn insert_daily_topic(
    draft: &mut String,
    selection: &mut Range<usize>,
    topic: &str,
    marked: bool,
    origin: &str,
    current: &str,
) -> bool {
    if marked || topic.is_empty() || origin != current {
        return false;
    }
    let mut next = draft.clone();
    let end = crate::editing::valid_selection(&next, selection.clone()).end;
    // Insert after the selection rather than replacing selected unsent text.
    let mut cursor = end..end;
    let mut ime = None;
    if !crate::editing::replace(
        &mut next,
        &mut cursor,
        &mut ime,
        None,
        &format!("{topic} "),
        1600,
    ) {
        return false;
    }
    *draft = next;
    *selection = cursor;
    true
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn full_width_art_retains_160_columns_blank_lines_and_spaces() {
        let body = format!("  {}  \n\n{}", "-".repeat(156), "*".repeat(160));
        assert_eq!(ascii_art_text(&body), body);
        assert_eq!(
            body.lines().map(str::len).collect::<Vec<_>>(),
            vec![160, 0, 160]
        );
    }
    #[test]
    fn daily_is_explicit_local_unicode_insertion_not_send_or_draft_replacement() {
        let mut draft = "前缀后缀".to_string();
        let mut cursor = 6..6;
        assert!(insert_daily_topic(
            &mut draft,
            &mut cursor,
            "今日话题",
            false,
            "lobby",
            "lobby"
        ));
        assert_eq!(draft, "前缀今日话题 后缀");
        let mut selected = "保留选中文本".to_string();
        let mut all = 0..selected.len();
        assert!(insert_daily_topic(
            &mut selected,
            &mut all,
            "话题",
            false,
            "lobby",
            "lobby"
        ));
        assert_eq!(selected, "保留选中文本话题 ");
        let original = draft.clone();
        assert!(!insert_daily_topic(
            &mut draft,
            &mut cursor,
            "不应插入",
            true,
            "lobby",
            "lobby"
        ));
        assert!(!insert_daily_topic(
            &mut draft,
            &mut cursor,
            "不应插入",
            false,
            "lobby",
            "dm:peer"
        ));
        assert_eq!(draft, original);
        let mut full = "字".repeat(1600);
        let mut cursor = full.len()..full.len();
        assert!(!insert_daily_topic(
            &mut full,
            &mut cursor,
            "话题",
            false,
            "lobby",
            "lobby"
        ));
        assert_eq!(full.chars().count(), 1600);
    }
    #[test]
    fn typed_object_projection_and_read_ack_preserve_identity_scope_and_closed_state() {
        let response:crate::session::Response=serde_json::from_str(r#"{"type":"action","room":"dm:bob","panel":{"kind":"community","scope":"dm:bob","title":"Community","community":{"room":"private-room","detail":{"card":{"id":7,"kind":"word","room":"private-room","owner":"alice","title":"game","created":1,"closed":false,"unavailable":false},"participant_count":0,"game":{"max":0,"wrong":1,"guesses":["x"]}}}}}"#).unwrap();
        assert!(crate::information::accepts_information_ack(
            &response, "dm:bob", "dm:bob"
        ));
        assert!(!crate::information::accepts_information_ack(
            &response, "dm:bob", "lobby"
        ));
        let detail = response.panel.unwrap().community.unwrap().detail.unwrap();
        assert_eq!(detail.card.id, 7);
        assert!(!detail.card.closed);
        assert!(detail.game.unwrap().secret.is_empty());
    }
}
