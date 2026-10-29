//! Prefix-only completion intents. Never retain a copy of the whole chat draft.
use crate::{composition::Origin, editing, session::Response, word};
use serde::{Deserialize, Serialize};
use std::ops::Range;

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Kind {
    Command,
    Room,
    User,
}
#[derive(Clone, Debug, Serialize)]
pub struct Data {
    pub kind: Kind,
    pub prefix: String,
}
#[derive(Clone, Debug, Deserialize)]
pub struct Receipt {
    pub kind: Kind,
    pub prefix: String,
    pub choices: Vec<String>,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Token {
    pub kind: Kind,
    pub prefix: String,
    pub range: Range<usize>,
}
pub fn token(text: &str, selection: Range<usize>, safe: bool) -> Option<Token> {
    if !safe
        || !selection.is_empty()
        || selection.end > text.len()
        || !text.is_char_boundary(selection.end)
    {
        return None;
    }
    let end = selection.end;
    let start = text[..end]
        .char_indices()
        .rev()
        .find(|(_, c)| c.is_whitespace())
        .map_or(0, |(i, c)| i + c.len_utf8());
    let prefix = &text[start..end];
    if prefix.len() > 256
        || prefix
            .chars()
            .any(|c| c.is_control() || word::is_draft_format_control(c))
    {
        return None;
    }
    let kind = if start == 0 && prefix.starts_with('/') {
        Kind::Command
    } else if prefix.starts_with('#') {
        Kind::Room
    } else if prefix.starts_with('@') || text.starts_with('/') {
        Kind::User
    } else {
        return None;
    };
    Some(Token {
        kind,
        prefix: prefix.into(),
        range: start..end,
    })
}
pub struct Context<'a> {
    pub enabled: bool,
    pub owner: &'a str,
    pub room: &'a str,
    pub origin: Option<&'a Origin>,
    pub generation: u64,
    pub revision: u64,
}
pub struct Intent {
    pub id: String,
    pub token: Token,
    owner: String,
    room: String,
    origin: Origin,
    generation: u64,
    revision: u64,
    pub choices: Option<Vec<String>>,
    pub cursor: usize,
    pub started: u64,
}
impl Intent {
    pub fn new(id: String, t: Token, c: Context<'_>, started: u64) -> Option<Self> {
        if !c.enabled || c.owner.is_empty() || c.room.is_empty() {
            return None;
        }
        Some(Self {
            id,
            token: t,
            owner: c.owner.into(),
            room: c.room.into(),
            origin: c.origin?.clone(),
            generation: c.generation,
            revision: c.revision,
            choices: None,
            cursor: 0,
            started,
        })
    }
    pub fn data(&self) -> Data {
        Data {
            kind: self.token.kind.clone(),
            prefix: self.token.prefix.clone(),
        }
    }
    pub fn current(&self, c: Context<'_>, text: &str, selection: Range<usize>, safe: bool) -> bool {
        c.enabled
            && self.owner == c.owner
            && self.room == c.room
            && Some(&self.origin) == c.origin
            && self.generation == c.generation
            && self.revision == c.revision
            && token(text, selection, safe).as_ref() == Some(&self.token)
    }
    pub fn claims(&self, r: &Response) -> bool {
        r.request_id == self.id
    }
    pub fn admits(&self, r: &Response) -> bool {
        if !self.claims(r) || self.choices.is_some() {
            return false;
        }
        if r.kind == "error" {
            return crate::information::is_plain_error_response(r)
                && r.room.is_empty()
                && r.user.is_empty()
                && r.capabilities.is_empty()
                && r.registration_mode.is_empty()
                && r.protocol == 0
                && r.online == 0
                && r.status.is_empty()
                && r.reply_to == 0
                && !r.at_latest;
        }
        let Some(v) = r.completion.as_ref() else {
            return false;
        };
        r.kind == "completion"
            && r.room == self.room
            && v.kind == self.token.kind
            && v.prefix == self.token.prefix
            && exclusive(r)
            && valid_choices(v)
    }
    pub fn insert(
        &self,
        index: usize,
        text: &mut String,
        selection: &mut Range<usize>,
        marked: &mut Option<Range<usize>>,
    ) -> bool {
        let Some(value) = self.choices.as_ref().and_then(|v| v.get(index)) else {
            return false;
        };
        if marked.is_some() || token(text, selection.clone(), true).as_ref() != Some(&self.token) {
            return false;
        }
        let range = editing::utf16_range(text, self.token.range.clone());
        word::replace_composer(text, selection, marked, Some(range), &format!("{value} "))
    }
    pub fn cycle(&mut self, back: bool) {
        if let Some(v) = self.choices.as_ref().filter(|v| !v.is_empty()) {
            self.cursor = if back {
                (self.cursor + v.len() - 1) % v.len()
            } else {
                (self.cursor + 1) % v.len()
            };
        }
    }
}
pub fn claims(r: &Response, intent: Option<&Intent>) -> bool {
    r.request_id.starts_with("completion:")
        || r.kind == "completion"
        || r.completion.is_some()
        || intent.is_some_and(|i| i.claims(r))
}
fn valid_choices(v: &Receipt) -> bool {
    let mut bytes = 0;
    if v.kind == Kind::User && v.choices.len() > 100 {
        return false;
    }
    v.choices.iter().enumerate().all(|(i, s)| {
        bytes += s.len();
        !s.is_empty()
            && s.len() <= 256
            && bytes <= 32 * 1024
            && !s
                .chars()
                .any(|c| c.is_whitespace() || c.is_control() || word::is_draft_format_control(c))
            && if v.kind == Kind::User {
                s.to_ascii_lowercase()
                    .starts_with(&v.prefix.to_ascii_lowercase())
            } else {
                s.starts_with(&v.prefix)
            }
            && (i == 0 || v.choices[i - 1] < *s)
    })
}
fn exclusive(r: &Response) -> bool {
    r.secret_admin.is_none()
        && r.maintenance.is_none()
        && r.composition.is_none()
        && r.configuration_receipt.is_none()
        && r.management_receipt.is_none()
        && r.sanction_receipt.is_none()
        && r.report_receipt.is_none()
        && r.guess_receipt.is_none()
        && r.word_receipt.is_none()
        && r.tic_receipt.is_none()
        && r.community_receipt.is_none()
        && r.direct_target.is_none()
        && !r.clear_view
        && r.mention.is_empty()
        && r.profile.is_none()
        && r.center.is_none()
        && r.preferences.is_none()
        && !r.key_reminder
        && r.device_key.is_none()
        && r.protocol == 0
        && r.registration_mode.is_empty()
        && r.user.is_empty()
        && r.rooms.is_empty()
        && r.messages.is_empty()
        && r.message.is_none()
        && r.online == 0
        && r.status.is_empty()
        && r.error.is_empty()
        && r.panel.is_none()
        && r.draft.is_none()
        && r.form.is_empty()
        && r.reply_to == 0
        && !r.replace
        && !r.at_latest
        && !r.draft_changed
        && r.capabilities.is_empty()
}

#[cfg(test)]
mod tests {
    use super::*;
    fn ctx(o: &Origin) -> Context<'_> {
        Context {
            enabled: true,
            owner: "alice",
            room: "lobby",
            origin: Some(o),
            generation: 7,
            revision: 9,
        }
    }
    fn intent(o: &Origin) -> Intent {
        Intent::new(
            "r".into(),
            token("你好 @bo tail", 10..10, true).unwrap(),
            ctx(o),
            0,
        )
        .unwrap()
    }
    fn response() -> Response {
        Response {
            kind: "completion".into(),
            request_id: "r".into(),
            room: "lobby".into(),
            completion: Some(Receipt {
                kind: Kind::User,
                prefix: "@bo".into(),
                choices: vec!["@bob".into(), "@bobby".into()],
            }),
            ..Default::default()
        }
    }
    #[test]
    fn completion_token_unicode_middle_and_sensitive_preflight() {
        assert_eq!(token("你好 @bo tail", 10..10, true).unwrap().range, 7..10);
        assert!(token("你好 @bo tail", 8..10, true).is_none());
        assert!(token("中文", 1..1, true).is_none());
        assert!(token("/passwd hidden", 14..14, false).is_none());
        assert_eq!(token("/msg ", 5..5, true).unwrap().prefix, "");
        assert_eq!(token("#l", 2..2, true).unwrap().kind, Kind::Room);
        assert!(token("nothing", 7..7, true).is_none());
        assert_eq!(token("/ti", 3..3, true).unwrap().kind, Kind::Command);
        assert!(token(&format!("@{}", "a".repeat(256)), 257..257, true).is_none());
    }
    #[test]
    fn completion_response_exclusive_gate_blocks_mixed_fields_and_wrong_kinds() {
        let o = Origin::synthetic("fixture", 1);
        let i = intent(&o);
        let r = response();
        assert!(i.admits(&r));
        for kind in ["error", "state", "action"] {
            let mut bad = r.clone();
            bad.kind = kind.into();
            assert!(!i.admits(&bad));
        }
        let mut bad = r.clone();
        bad.preferences = Some(Default::default());
        assert!(!i.admits(&bad));
        bad = r.clone();
        bad.status = "unrelated".into();
        assert!(!i.admits(&bad));
        bad = r.clone();
        bad.room = "dm:bob".into();
        assert!(!i.admits(&bad));
        bad = r.clone();
        bad.request_id = "old".into();
        assert!(!i.admits(&bad));
        assert!(claims(&bad, None));
        let mut error = Response {
            kind: "error".into(),
            request_id: "r".into(),
            error: "no access".into(),
            ..Default::default()
        };
        assert!(i.admits(&error));
        error.preferences = Some(Default::default());
        assert!(!i.admits(&error));
        assert!(!claims(
            &Response {
                kind: "state".into(),
                ..Default::default()
            },
            Some(&i)
        ));
    }
    #[test]
    fn completion_intent_epoch_cursor_owner_origin_and_scope_do_not_retarget() {
        let o = Origin::synthetic("fixture", 1);
        let other = Origin::synthetic("other", 1);
        let i = intent(&o);
        assert!(i.current(ctx(&o), "你好 @bo tail", 10..10, true));
        for c in [
            Context {
                owner: "bob",
                ..ctx(&o)
            },
            Context {
                room: "dm:bob",
                ..ctx(&o)
            },
            Context {
                generation: 8,
                ..ctx(&o)
            },
            Context {
                revision: 10,
                ..ctx(&o)
            },
            Context {
                origin: Some(&other),
                ..ctx(&o)
            },
            Context {
                enabled: false,
                ..ctx(&o)
            },
        ] {
            assert!(!i.current(c, "你好 @bo tail", 10..10, true));
        }
        assert!(!i.current(ctx(&o), "你好 @bo tail", 9..9, true));
        assert!(!i.current(ctx(&o), "你好 @ba tail", 10..10, true));
        assert!(!i.current(ctx(&o), "你好 @bo tail", 10..10, false));
    }
    #[test]
    fn completion_insert_preserves_suffix_and_does_not_submit_or_rewrite_other_state() {
        let o = Origin::synthetic("fixture", 1);
        let mut i = intent(&o);
        i.choices = Some(response().completion.unwrap().choices);
        let mut text = "你好 @bo tail🙂".to_string();
        let mut selection = 10..10;
        assert!(i.insert(0, &mut text, &mut selection, &mut None));
        assert_eq!(text, "你好 @bob  tail🙂");
        assert_eq!(selection, 12..12);
        let before = text.clone();
        assert!(!i.insert(1, &mut text, &mut selection, &mut None));
        assert_eq!(text, before);
        i.cycle(true);
        assert_eq!(i.cursor, 1);
        i.cycle(false);
        assert_eq!(i.cursor, 0);
        i.choices = Some(vec![]);
        i.cycle(false);
        assert!(!i.insert(0, &mut text, &mut selection, &mut None));
    }
    #[test]
    fn completion_choices_reject_control_duplicate_oversize_and_preserve_literal_prefix() {
        let mut v = Receipt {
            kind: Kind::User,
            prefix: "@B".into(),
            choices: vec!["@bob".into()],
        };
        assert!(valid_choices(&v));
        v.choices = vec!["@bob".into(), "@bob".into()];
        assert!(!valid_choices(&v));
        v.choices = vec!["@b\nsecret".into()];
        assert!(!valid_choices(&v));
        v.choices = vec!["@x".into()];
        assert!(!valid_choices(&v));
        v.prefix = "@%".into();
        v.choices = vec!["@%literal".into()];
        assert!(valid_choices(&v));
        v.prefix = "".into();
        v.choices = (0..101).map(|i| format!("u{i:03}")).collect();
        assert!(!valid_choices(&v));
    }
}
