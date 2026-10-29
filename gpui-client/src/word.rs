//! Word starts have a dedicated secret container; only public guesses use the
//! ordinary action form. Transport serialization necessarily makes a short-lived
//! copy. Clearing this String is not a secure memory/IME/clipboard erasure claim.
use crate::session::{Panel, Response};
use serde::Deserialize;

pub const RESERVED_NOTICE: &str = "猜词答案请使用秘密创建表单；此次输入未加入聊天草稿。";
pub const QUARANTINE_NOTICE: &str = "检测到以前已保存的猜词创建草稿；已暂停再次保存/复制。没有删除服务器草稿或旧备份，请明确核对处理。";
// Format-control table mirrors the approved Go toolchain's Unicode17 Cf/Zl/Zp
// tables (unicode/tables.go). No new dependency or approximate lowercase rule.
/// Shared exact draft-normalization classification; callers must not treat
/// raw format controls as part of a credential-command name.
pub fn is_draft_format_control(c: char) -> bool {
    let n = c as u32;
    const RANGES: &[(u32, u32, u32)] = &[
        (0xad, 0x600, 1363),
        (0x601, 0x605, 1),
        (0x61c, 0x6dd, 193),
        (0x70f, 0x890, 385),
        (0x891, 0x8e2, 81),
        (0x180e, 0x200b, 2045),
        (0x200c, 0x200f, 1),
        (0x2028, 0x202e, 1),
        (0x2060, 0x2064, 1),
        (0x2066, 0x206f, 1),
        (0xfeff, 0xfff9, 250),
        (0xfffa, 0xfffb, 1),
        (0x110bd, 0x110cd, 16),
        (0x13430, 0x1343f, 1),
        (0x1bca0, 0x1bca3, 1),
        (0x1d173, 0x1d17a, 1),
        (0xe0001, 0xe0020, 31),
        (0xe0021, 0xe007f, 1),
    ];
    RANGES
        .iter()
        .any(|&(a, b, stride)| n >= a && n <= b && (n - a).is_multiple_of(stride))
}
pub fn reserved_start(text: &str) -> bool {
    // Terminal escape sequences are not accepted as ordinary draft input. This
    // conservative reservation prevents ANSI stripping from revealing a command
    // after the client safety gate. No sequence is sent for server normalization.
    if text.contains('\u{1b}') {
        return true;
    }
    let normalized: String = text
        .replace("\r\n", "\n")
        .replace('\r', "\n")
        .chars()
        .filter(|&c| c == '\n' || c == '\t' || (!c.is_control() && !is_draft_format_control(c)))
        .collect();
    std::iter::once(normalized.as_str())
        .chain(
            normalized
                .match_indices('\n')
                .map(|(i, _)| &normalized[i + 1..]),
        )
        .any(|suffix| {
            let mut words = suffix.split_whitespace();
            words
                .next()
                .is_some_and(|s| s.eq_ignore_ascii_case("/word"))
                && words
                    .next()
                    .is_some_and(|s| s.eq_ignore_ascii_case("start"))
        })
}
/// Actual composer edit boundary, also used by committed and preedit IME paths.
/// Rejected replacements leave all three editable state values unchanged.
pub fn replace_composer(
    value: &mut String,
    selection: &mut std::ops::Range<usize>,
    marked: &mut Option<std::ops::Range<usize>>,
    range: Option<std::ops::Range<usize>>,
    text: &str,
) -> bool {
    let mut candidate = value.clone();
    let mut next_selection = selection.clone();
    let mut next_marked = marked.clone();
    if !crate::editing::replace(
        &mut candidate,
        &mut next_selection,
        &mut next_marked,
        range,
        text,
        1600,
    ) || reserved_start(&candidate)
    {
        return false;
    }
    *value = candidate;
    *selection = next_selection;
    *marked = next_marked;
    true
}
pub fn is_view(text: &str) -> bool {
    let mut w = text.split_whitespace();
    w.next().is_some_and(|s| s.eq_ignore_ascii_case("/word")) && w.next().is_none()
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WordIntent {
    pub start: bool,
    pub object_id: i64,
    pub room: String,
    pub requested: String, // only public guesses; always empty for a start
}
#[derive(Clone, Debug, Default, Deserialize)]
#[serde(default)]
pub struct WordReceipt {
    pub operation: String,
    pub object_id: i64,
    pub room: String,
    pub origin_scope: String,
    pub requested: String,
    pub value: String,
    pub closed: bool,
    pub feedback: String,
    pub committed: bool,
}
impl WordIntent {
    pub fn from_command(command: &str) -> Option<Self> {
        let (head, tail) = command.trim().split_once(char::is_whitespace)?;
        if !head.eq_ignore_ascii_case("/word") || tail.trim().is_empty() || reserved_start(command)
        {
            return None;
        }
        Some(Self {
            start: false,
            object_id: 0,
            room: String::new(),
            requested: tail.trim().into(),
        })
    }
    pub fn command(&self, body: &str) -> Result<String, &'static str> {
        if self.start {
            return Err("创建答案必须使用秘密表单");
        }
        if body.trim().is_empty() || body.chars().count() > 24 {
            return Err("请输入1–24字符的公开猜测");
        }
        Ok(format!("/word {}", body.trim()))
    }
    pub fn bind_submission(&self, command: &str) -> Option<Self> {
        if self.start || self.object_id <= 0 || self.room.is_empty() {
            return None;
        }
        let (head, tail) = command.trim().split_once(char::is_whitespace)?;
        if !head.eq_ignore_ascii_case("/word") || tail.trim().is_empty() {
            return None;
        }
        Some(Self {
            requested: tail.trim().into(),
            ..self.clone()
        })
    }
    pub fn accepts_ack(&self, response: &Response, origin: &str) -> bool {
        if response.configuration_receipt.is_some() || response.management_receipt.is_some() {
            return false;
        }
        let Some(r) = response.word_receipt.as_ref() else {
            return false;
        };
        response.room == origin
            && !response.replace
            && !response.clear_view
            && !response.draft_changed
            && response.message.is_none()
            && response.messages.is_empty()
            && response.draft.is_none()
            && response.panel.as_ref().is_none_or(|p| p.scope == origin)
            && r.committed
            && r.object_id > 0
            && r.origin_scope == origin
            && !r.room.is_empty()
            && (self.room.is_empty() || self.room == r.room)
            && (self.object_id == 0 || self.object_id == r.object_id)
            && if self.start {
                r.operation == "start"
                    && r.requested.is_empty()
                    && r.value.is_empty()
                    && r.feedback.is_empty()
                    && !r.closed
            } else {
                r.operation == "guess" && r.requested == self.requested
            }
    }
}
pub fn native_intent(command: &str, panel: &Panel, current: &str) -> Option<WordIntent> {
    if panel.scope != current {
        return None;
    }
    let view = panel.community.as_ref()?;
    if command == "local:word-start"
        && !view.room.is_empty()
        && view.detail.is_none()
        && panel
            .actions
            .iter()
            .any(|a| a.command == "local:word-start")
    {
        return Some(WordIntent {
            start: true,
            object_id: 0,
            room: view.room.clone(),
            requested: String::new(),
        });
    }
    let id = command
        .strip_prefix("local:word-input ")?
        .parse::<i64>()
        .ok()?;
    let d = view.detail.as_ref()?;
    let g = d.game.as_ref()?;
    (id > 0
        && d.card.id == id
        && d.card.kind == "word"
        && !d.card.closed
        && !d.card.unavailable
        && g.can_guess
        && !d.card.room.is_empty())
    .then(|| WordIntent {
        start: false,
        object_id: id,
        room: d.card.room.clone(),
        requested: String::new(),
    })
}
pub struct WordStartForm {
    pub owner: String,
    pub origin: String,
    pub generation: u64,
    pub intent: WordIntent,
    secret: String,
}
impl std::fmt::Debug for WordStartForm {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("WordStartForm")
            .field("secret", &"[REDACTED]")
            .finish()
    }
}
impl WordStartForm {
    pub fn new(owner: String, origin: String, generation: u64, intent: WordIntent) -> Self {
        Self {
            owner,
            origin,
            generation,
            intent,
            secret: String::new(),
        }
    }
    pub fn bound(&self, owner: &str, origin: &str, generation: u64) -> bool {
        self.owner == owner
            && !owner.is_empty()
            && self.origin == origin
            && self.generation == generation
    }
    pub fn value(&self) -> &str {
        &self.secret
    }
    pub fn replace(&mut self, text: String) {
        self.secret = text
    }
    pub fn take_command(&mut self) -> Result<String, &'static str> {
        if self.secret.trim().chars().count() < 2 || self.secret.chars().count() > 24 {
            return Err("答案须为2–24字符");
        }
        let secret = std::mem::take(&mut self.secret);
        Ok(format!("/word start {secret}"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn real_composer_boundary_rejects_typed_pasted_and_ime_candidates_atomically() {
        for bad in [
            "/word start 秘密",
            "/word \nstart hidden",
            "/word \r\nstart hidden",
            "safe\n /word \n START hidden",
            " /WORD START secret",
            "/word\tstart\tsecret",
            "safe\r\n /word start hidden",
            "/wo\u{200b}rd st\u{2060}art secret",
            "\u{1b}[31m/word start secret",
        ] {
            let mut v = "原稿".to_string();
            let mut s = 0..v.len();
            let mut m = Some(s.clone());
            let before = (v.clone(), s.clone(), m.clone());
            assert!(!replace_composer(&mut v, &mut s, &mut m, None, bad));
            assert_eq!((v, s, m), before);
        }
        let mut v = "/word star".to_string();
        let mut s = v.len()..v.len();
        let mut m = None;
        assert!(!replace_composer(&mut v, &mut s, &mut m, None, "t"));
        assert_eq!(v, "/word star");
        assert_eq!(s, 10..10);
        let mut v = "/word 猫".to_string();
        let mut s = v.len()..v.len();
        let mut m = None;
        assert!(replace_composer(&mut v, &mut s, &mut m, None, "咪"));
        assert_eq!(v, "/word 猫咪");
        assert!(!reserved_start("有人讨论 /word start 的命令"));
        assert!(!reserved_start("/word starter"));
    }
    #[test]
    fn dedicated_secret_is_redacted_consumed_once_and_not_a_public_intent() {
        let intent = WordIntent {
            start: true,
            object_id: 0,
            room: "private-a".into(),
            requested: String::new(),
        };
        let mut f = WordStartForm::new("alice".into(), "dm:bob".into(), 7, intent);
        f.replace("synthetic秘密".into());
        assert!(!format!("{f:?}").contains("synthetic"));
        assert!(f.bound("alice", "dm:bob", 7));
        assert!(!f.bound("bob", "dm:bob", 7));
        assert!(!f.bound("alice", "lobby", 7));
        assert!(!f.bound("alice", "dm:bob", 8));
        let command = f.take_command().unwrap();
        assert_eq!(command, "/word start synthetic秘密");
        assert!(f.value().is_empty());
        assert!(f.take_command().is_err());
        assert!(WordIntent::from_command(&command).is_none());
        assert!(f.intent.requested.is_empty());
        let public = WordIntent {
            start: false,
            object_id: 4,
            room: "private-a".into(),
            requested: String::new(),
        };
        assert_eq!(public.command("start").unwrap(), "/word start");
        assert_eq!(public.command("猫").unwrap(), "/word 猫");
    }
    #[test]
    fn receipt_never_accepts_start_echo_or_wrong_origin_object() {
        let intent = WordIntent {
            start: true,
            object_id: 0,
            room: "private-a".into(),
            requested: String::new(),
        };
        let mut r = Response {
            room: "dm:bob".into(),
            word_receipt: Some(WordReceipt {
                operation: "start".into(),
                object_id: 4,
                room: "private-a".into(),
                origin_scope: "dm:bob".into(),
                committed: true,
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(intent.accepts_ack(&r, "dm:bob"));
        r.word_receipt.as_mut().unwrap().requested = "synthetic-secret".into();
        assert!(!intent.accepts_ack(&r, "dm:bob"));
        r.word_receipt.as_mut().unwrap().requested.clear();
        r.word_receipt.as_mut().unwrap().room = "other".into();
        assert!(!intent.accepts_ack(&r, "dm:bob"));
        assert!(!intent.accepts_ack(&r, "lobby"));
    }
    #[test]
    fn actual_public_form_exception_requires_complete_current_intent() {
        use crate::direct_message::{DirectMessageForm, FormPurpose};
        let mut f = DirectMessageForm::new("alice".into(), "dm:bob".into(), 7);
        f.purpose = FormPurpose::Word(WordIntent {
            start: false,
            object_id: 12,
            room: "private".into(),
            requested: String::new(),
        });
        for body in ["start", "START", "start secret"] {
            f.body = body.into();
            let command = format!("/word {body}");
            assert!(reserved_start(&command));
            let intent = f
                .public_word_submission("alice", "dm:bob", 7, &command)
                .unwrap();
            assert_eq!(intent.requested, body);
            assert!(
                f.public_word_submission("other", "dm:bob", 7, &command)
                    .is_none()
            );
            assert!(
                f.public_word_submission("alice", "lobby", 7, &command)
                    .is_none()
            );
            assert!(
                f.public_word_submission("alice", "dm:bob", 8, &command)
                    .is_none()
            );
            assert!(
                f.public_word_submission("alice", "dm:bob", 7, "/word start another")
                    .is_none()
            );
        }
        f.purpose = FormPurpose::Direct;
        assert!(
            f.public_word_submission("alice", "dm:bob", 7, "/word start secret")
                .is_none()
        );
        f.purpose = FormPurpose::Word(WordIntent {
            start: false,
            object_id: 0,
            room: "private".into(),
            requested: String::new(),
        });
        assert!(
            f.public_word_submission("alice", "dm:bob", 7, "/word start secret")
                .is_none()
        );
    }
}
