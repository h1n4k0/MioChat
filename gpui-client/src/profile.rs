use serde::Deserialize;

pub fn redact_recalled_quotes(
    messages: &mut [crate::session::Message],
    source: &crate::session::Message,
) -> bool {
    if source.recalled == 0 {
        return false;
    }
    let mut changed = false;
    for message in messages {
        if message.reply_to == source.id
            && (!message.reply_body.is_empty() || !message.reply_name.is_empty())
        {
            message.reply_body.clear();
            message.reply_name.clear();
            changed = true;
        }
    }
    changed
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Profile {
    #[serde(default)]
    pub can_admin: bool,
    pub viewer: String,
    pub name: String,
    pub uid: i64,
    pub role: String,
    pub bio: String,
    pub status: String,
    pub avatar: String,
    #[serde(default)]
    pub avatar_kind: String,
    pub created: String,
    pub last_login: String,
    pub online: bool,
    #[serde(rename = "self")]
    pub own: bool,
    pub following: bool,
    pub friend: bool,
    pub requested: bool,
    pub blocked: bool,
    pub note: String,
    #[serde(default)]
    pub badges: Vec<String>,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EditKind {
    Bio,
    Status,
    Note,
    Afk,
}
#[derive(Clone, Debug)]
pub struct ProfileEdit {
    pub owner: String,
    pub target: String,
    pub kind: EditKind,
}
impl ProfileEdit {
    pub fn command(&self, owner: &str, value: &str) -> Option<String> {
        if self.owner != owner || owner.is_empty() || self.target.chars().any(char::is_whitespace) {
            return None;
        }
        match self.kind {
            EditKind::Bio if self.target == owner => Some(format!("/bio {value}")),
            EditKind::Status if self.target == owner => Some(format!("/status {value}")),
            EditKind::Afk if self.target == owner => Some(format!("/afk {value}")),
            EditKind::Note if self.target != owner && !self.target.is_empty() => {
                Some(format!("/note {} {value}", self.target))
            }
            _ => None,
        }
    }
}

/// Insert at the current cursor, retaining both sides of an unsent draft.
/// The caller correlates the server-resolved name with the pending action.
pub fn insert_mention(text: &mut String, cursor: usize, name: &str) -> Option<usize> {
    if name.is_empty()
        || name.chars().any(char::is_whitespace)
        || cursor > text.len()
        || !text.is_char_boundary(cursor)
    {
        return None;
    }
    let prefix = if text[..cursor]
        .chars()
        .next_back()
        .is_some_and(|c| !c.is_whitespace())
    {
        " "
    } else {
        ""
    };
    let insert = format!("{prefix}@{name} ");
    if text.chars().count() + insert.chars().count() > 1600 {
        return None;
    }
    text.insert_str(cursor, &insert);
    Some(cursor + insert.len())
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn mention_keeps_unicode_draft_suffix_and_respects_limit() {
        let mut text = "中文🙂后缀".to_string();
        let cursor = "中文🙂".len();
        let end = insert_mention(&mut text, cursor, "用户").unwrap();
        assert_eq!(text, "中文🙂 @用户 后缀");
        assert_eq!(&text[end..], "后缀");
        assert_eq!(insert_mention(&mut text, 1, "user"), None);
        let mut full = "x".repeat(1600);
        assert_eq!(insert_mention(&mut full, 0, "user"), None);
        assert_eq!(full.len(), 1600);
    }
    #[test]
    fn recalled_paged_out_source_clears_cached_quotes_only() {
        use crate::session::Message;
        let mut messages = vec![
            Message {
                id: 12,
                reply_to: 1,
                reply_name: "blocked-peer".into(),
                reply_body: "old private quote".into(),
                ..Default::default()
            },
            Message {
                id: 13,
                reply_to: 2,
                reply_body: "other".into(),
                ..Default::default()
            },
        ];
        assert!(redact_recalled_quotes(
            &mut messages,
            &Message {
                id: 1,
                recalled: 10,
                ..Default::default()
            }
        ));
        assert!(messages[0].reply_body.is_empty() && messages[0].reply_name.is_empty());
        assert_eq!(messages[1].reply_body, "other");
    }
    #[test]
    fn edits_bind_owner_and_distinguish_private_note_from_own_fields() {
        let mut e = ProfileEdit {
            owner: "alice".into(),
            target: "bob".into(),
            kind: EditKind::Note,
        };
        assert_eq!(
            e.command("alice", "private"),
            Some("/note bob private".into())
        );
        assert_eq!(e.command("eve", "private"), None);
        e.kind = EditKind::Bio;
        assert_eq!(e.command("alice", "foreign"), None);
        e.target = "alice".into();
        assert_eq!(e.command("alice", "own"), Some("/bio own".into()));
    }
}
