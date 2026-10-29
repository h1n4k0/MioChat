//! Owner-bound long-form editor state. Raw edit generations are not server
//! canonical snapshots; no text here belongs to the ordinary chat draft.
use crate::session::Response;
use serde::{Deserialize, Serialize};
use std::fmt;

/// Exact connection + locally trusted host-key context, not an assertion of the
/// negotiated SSH fingerprint. No keys/paths are serialized or Debug-printed.
#[derive(Clone, PartialEq, Eq)]
pub struct Origin {
    endpoint: String,
    port: u16,
    ssh_user: String,
    authentication: String,
    trusted_keys: String,
    synthetic: Option<u64>,
}
impl Origin {
    pub fn ssh(
        spec: &crate::connection::ConnectionSpec,
        trusted_keys: String,
        identity: &str,
    ) -> Option<Self> {
        if spec.validate().is_err() || trusted_keys.is_empty() {
            return None;
        }
        Some(Self {
            endpoint: spec.host.clone(),
            port: spec.port,
            ssh_user: spec.ssh_user.clone(),
            authentication: if identity.is_empty() {
                "keyboard-interactive".into()
            } else {
                format!("identity:{identity}")
            },
            trusted_keys,
            synthetic: None,
        })
    }
    pub fn synthetic(tag: &str, nonce: u64) -> Self {
        Self {
            endpoint: tag.into(),
            port: 0,
            ssh_user: String::new(),
            authentication: String::new(),
            trusted_keys: String::new(),
            synthetic: Some(nonce),
        }
    }
}
#[derive(Clone, PartialEq, Eq, Deserialize)]
pub struct Snapshot {
    pub owner: String,
    pub room: String,
    pub kind: String,
    pub title: String,
    pub body: String,
    pub version: i64,
    pub exists: bool,
}
impl fmt::Debug for Snapshot {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("CompositionSnapshot")
            .field("version", &self.version)
            .field("exists", &self.exists)
            .finish_non_exhaustive()
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Operation {
    Load,
    Reserve,
    Save,
    Suspend,
    Publish,
    Remote,
    Reload,
}
#[derive(Clone, Serialize)]
pub struct CompositionRequest {
    pub operation: Operation,
    pub room: String,
    pub kind: String,
    pub expected_version: Option<i64>,
    pub edit_generation: u64,
    pub title: String,
    pub body: String,
}
#[derive(Clone, Debug, Deserialize)]
pub struct Receipt {
    pub operation: Operation,
    pub edit_generation: u64,
    pub expected_version: Option<i64>,
    pub snapshot: Snapshot,
    #[serde(default)]
    pub object_id: i64,
    pub committed: bool,
}
#[derive(Clone)]
pub struct Pending {
    pub request_id: String,
    pub request: CompositionRequest,
    pub started: u64,
}
#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Slot {
    Title,
    Body,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Effect {
    NeedReserve,
    Ready,
    Saved,
    Remote,
    Reloaded,
    Close,
    Published { object_id: i64, newer_input: bool },
}
#[derive(Clone)]
pub struct Editor {
    pub origin: Option<Origin>,
    pub owner: String,
    pub room: String,
    pub kind: String,
    pub generation: u64,
    pub title: String,
    pub body: String,
    pub revision: u64,
    pub snapshot: Option<Snapshot>,
    pub remote: Option<Snapshot>,
    pub pending: Option<Pending>,
    pub frozen: bool,
    pub blocked: bool,
    pub preview: bool,
    pub close_after: Option<u64>,
    pub discard_armed: bool,
    pub status: String,
    pub last_edit: u64,
    saved_title: String,
    saved_body: String,
    opening: Option<Snapshot>,
}
impl Editor {
    pub fn new(
        owner: String,
        room: String,
        kind: String,
        title: String,
        generation: u64,
    ) -> Option<Self> {
        if owner.is_empty()
            || room.is_empty()
            || room.starts_with("dm:")
            || !matches!(kind.as_str(), "post" | "art")
        {
            return None;
        }
        Some(Self {
            origin: None,
            owner,
            room,
            kind,
            generation,
            title,
            body: String::new(),
            revision: 0,
            snapshot: None,
            remote: None,
            pending: None,
            frozen: false,
            blocked: false,
            preview: false,
            close_after: None,
            discard_armed: false,
            status: "正在读取原频道草稿…".into(),
            last_edit: 0,
            saved_title: String::new(),
            saved_body: String::new(),
            opening: None,
        })
    }
    pub fn belongs_to_origin(&self, owner: &str, origin: Option<&Origin>) -> bool {
        self.owner == owner
            && self
                .origin
                .as_ref()
                .zip(origin)
                .is_some_and(|(a, b)| a == b)
    }
    pub fn scope_changed(&mut self, room: &str, forced: bool) -> bool {
        if self.room == room && !forced {
            return false;
        }
        self.frozen = true;
        self.remote = None;
        self.close_after = None;
        self.discard_armed = false;
        if self.pending.as_ref().is_some_and(|p| {
            matches!(
                p.request.operation,
                Operation::Load | Operation::Reserve | Operation::Remote | Operation::Reload
            )
        }) {
            self.pending = None;
            self.blocked = true;
        }
        true
    }
    pub fn bound(&self, owner: &str, generation: u64) -> bool {
        self.owner == owner && self.generation == generation
    }
    pub fn ready(&self) -> bool {
        self.snapshot.is_some()
    }
    pub fn dirty(&self) -> bool {
        self.title != self.saved_title || self.body != self.saved_body
    }
    pub fn value(&self, slot: Slot) -> &str {
        match slot {
            Slot::Title => &self.title,
            Slot::Body => &self.body,
        }
    }
    pub fn edit(&mut self, slot: Slot, value: String, now: u64) -> bool {
        if !self.ready()
            || self.frozen
            || value.len() > if slot == Slot::Title { 4096 } else { 32 * 1024 }
            || slot == Slot::Title && value.contains(['\r', '\n'])
        {
            return false;
        }
        if self.value(slot) == value {
            return true;
        }
        match slot {
            Slot::Title => self.title = value,
            Slot::Body => self.body = value,
        }
        self.revision = self.revision.wrapping_add(1);
        self.last_edit = now;
        self.remote = None;
        self.close_after = None;
        self.discard_armed = false;
        true
    }
    pub fn request_close(&mut self) -> bool {
        if self.ready() && !self.dirty() && self.pending.is_none() {
            return true;
        }
        self.close_after = Some(self.revision);
        false
    }
    pub fn autosave_due(&self, now: u64) -> bool {
        self.ready()
            && self.dirty()
            && !self.blocked
            && self.pending.is_none()
            && now.saturating_sub(self.last_edit) >= 1500
    }
    pub fn begin(
        &mut self,
        operation: Operation,
        request_id: String,
        now: u64,
    ) -> Option<CompositionRequest> {
        if self.pending.is_some() {
            return None;
        }
        let expected = match operation {
            Operation::Load if !self.ready() => None,
            Operation::Reserve => Some(self.opening.as_ref()?.version),
            Operation::Remote => None,
            Operation::Reload => Some(self.remote.as_ref()?.version),
            Operation::Save | Operation::Suspend | Operation::Publish => {
                Some(self.snapshot.as_ref()?.version)
            }
            _ => return None,
        };
        if self.frozen && !matches!(operation, Operation::Suspend) {
            return None;
        }
        if self.blocked
            && matches!(
                operation,
                Operation::Save | Operation::Suspend | Operation::Publish
            )
        {
            return None;
        }
        let request = CompositionRequest {
            operation,
            room: self.room.clone(),
            kind: self.kind.clone(),
            expected_version: expected,
            edit_generation: self.revision,
            title: if matches!(
                operation,
                Operation::Reserve | Operation::Save | Operation::Suspend | Operation::Publish
            ) {
                self.title.clone()
            } else {
                String::new()
            },
            body: if matches!(
                operation,
                Operation::Save | Operation::Suspend | Operation::Publish
            ) {
                self.body.clone()
            } else {
                String::new()
            },
        };
        self.pending = Some(Pending {
            request_id,
            request: request.clone(),
            started: now,
        });
        self.discard_armed = false;
        Some(request)
    }
    pub fn keep_local(&mut self, request_id: String, now: u64) -> Option<CompositionRequest> {
        if self.pending.is_some() || self.frozen {
            return None;
        }
        let remote = self.remote.take()?;
        self.snapshot = Some(remote);
        self.blocked = false;
        self.begin(Operation::Save, request_id, now)
    }
    pub fn interrupt(&mut self) {
        if self.pending.take().is_some() {
            self.blocked = true;
            self.status = "连接/凭据发生变化，提交结果待确认；请检查原频道，勿自动重发。".into();
        }
        self.frozen = true;
        self.close_after = None;
        self.remote = None;
        self.discard_armed = false;
    }
    pub fn expire(&mut self, now: u64) -> bool {
        if self
            .pending
            .as_ref()
            .is_some_and(|p| now.saturating_sub(p.started) >= 10_000)
        {
            self.pending = None;
            self.blocked = true;
            self.close_after = None;
            self.remote = None;
            self.status = "确认超时；输入仍保留。请读取原频道版本后明确处理，不会自动重发。".into();
            return true;
        }
        false
    }
    pub fn failed(&mut self, request_id: &str, notice: &str) {
        if self
            .pending
            .as_ref()
            .is_some_and(|p| p.request_id == request_id)
        {
            self.pending = None;
            self.blocked = true;
            self.close_after = None;
            self.remote = None;
            self.status = notice.into();
        }
    }
    fn valid_snapshot(&self, s: &Snapshot) -> bool {
        s.owner == self.owner
            && s.room == self.room
            && s.kind == self.kind
            && s.version >= 0
            && s.title.len() <= 4096
            && s.body.len() <= 32 * 1024
            && (!s.exists || s.version > 0)
            && (s.exists || s.title.is_empty() && s.body.is_empty())
    }
    pub fn admits(&self, r: &Response, owner: &str, generation: u64) -> bool {
        let Some(p) = self
            .pending
            .as_ref()
            .filter(|p| p.request_id == r.request_id)
        else {
            return false;
        };
        if !self.bound(owner, generation) {
            return false;
        }
        if crate::information::is_plain_error_response(r) {
            return true;
        }
        let Some(c) = r.composition.as_ref() else {
            return false;
        };
        if r.kind != "composition"
            || r.room != self.room
            || !exclusive(r)
            || !self.valid_snapshot(&c.snapshot)
            || c.operation != p.request.operation
            || c.edit_generation != p.request.edit_generation
            || c.expected_version != p.request.expected_version
        {
            return false;
        }
        if self.frozen
            && matches!(
                c.operation,
                Operation::Load | Operation::Reserve | Operation::Remote | Operation::Reload
            )
        {
            return false;
        }
        let read = matches!(
            c.operation,
            Operation::Load | Operation::Remote | Operation::Reload
        );
        if c.committed == read || (c.operation == Operation::Publish) != (c.object_id > 0) {
            return false;
        }
        if c.operation != Operation::Publish && c.object_id != 0 {
            return false;
        }
        match c.operation {
            Operation::Load | Operation::Remote => c.expected_version.is_none(),
            Operation::Reload => c.expected_version == Some(c.snapshot.version),
            Operation::Reserve => {
                c.snapshot.exists && c.expected_version.is_some_and(|v| c.snapshot.version >= v)
            }
            Operation::Save | Operation::Suspend | Operation::Publish => {
                c.snapshot.exists
                    && c.expected_version.is_some_and(|v| c.snapshot.version > v)
                    && (c.operation != Operation::Publish || c.snapshot.body.is_empty())
            }
        }
    }
    pub fn accept(&mut self, r: &Response) -> Option<Effect> {
        if !self.admits(r, &self.owner, self.generation) {
            return None;
        }
        let c = r.composition.as_ref()?;
        let p = self.pending.take()?;
        if p.request_id != r.request_id {
            self.pending = Some(p);
            return None;
        }
        let current = self.revision == p.request.edit_generation;
        match c.operation {
            Operation::Load => {
                self.opening = Some(c.snapshot.clone());
                Some(Effect::NeedReserve)
            }
            Operation::Reserve => {
                self.title = c.snapshot.title.clone();
                self.body = c.snapshot.body.clone();
                self.saved_title = self.title.clone();
                self.saved_body = self.body.clone();
                self.snapshot = Some(c.snapshot.clone());
                self.opening = None;
                self.blocked = false;
                self.status = "原频道草稿已载入".into();
                if self.close_after == Some(self.revision) && !self.dirty() {
                    Some(Effect::Close)
                } else {
                    Some(Effect::Ready)
                }
            }
            Operation::Remote => {
                self.remote = Some(c.snapshot.clone());
                self.status = "远端预览已读取；重载会舍弃本地，保留本地会覆盖这个固定版本。".into();
                Some(Effect::Remote)
            }
            Operation::Reload => {
                self.remote = None;
                if !current {
                    self.status = "读取期间又有新输入，未替换本地；请重新明确处理。".into();
                    return None;
                }
                self.title = c.snapshot.title.clone();
                self.body = c.snapshot.body.clone();
                self.saved_title = self.title.clone();
                self.saved_body = self.body.clone();
                self.snapshot = Some(c.snapshot.clone());
                self.blocked = false;
                self.close_after = None;
                self.status = "已明确重载原频道草稿".into();
                Some(Effect::Reloaded)
            }
            Operation::Save | Operation::Suspend => {
                self.snapshot = Some(c.snapshot.clone());
                self.saved_title = p.request.title;
                self.saved_body = p.request.body;
                self.remote = None;
                self.blocked = false;
                self.status = "草稿已保存；后续新输入会单独保存".into();
                if self.close_after == Some(self.revision) && current && !self.dirty() {
                    Some(Effect::Close)
                } else {
                    Some(Effect::Saved)
                }
            }
            Operation::Publish => {
                self.snapshot = Some(c.snapshot.clone());
                self.saved_title = p.request.title;
                self.saved_body.clear();
                self.remote = None;
                self.blocked = false;
                if current {
                    self.body.clear();
                }
                self.close_after = None;
                self.status = if current {
                    "作品已提交，原草稿已清空"
                } else {
                    "作品已提交；提交后的新输入仍留在编辑器，未被清除"
                }
                .into();
                Some(Effect::Published {
                    object_id: c.object_id,
                    newer_input: !current,
                })
            }
        }
    }
}
/// Composition frames never carry siblings handled by global UI workflows.
pub fn exclusive(r: &Response) -> bool {
    r.secret_admin.is_none() && r.maintenance.is_none() && exclusive_shared(r)
}
pub fn exclusive_shared(r: &Response) -> bool {
    r.panel.is_none()
        && r.profile.is_none()
        && r.center.is_none()
        && r.preferences.is_none()
        && r.device_key.is_none()
        && !r.key_reminder
        && r.rooms.is_empty()
        && r.messages.is_empty()
        && r.message.is_none()
        && r.draft.is_none()
        && !r.draft_changed
        && !r.replace
        && !r.clear_view
        && r.form.is_empty()
        && r.mention.is_empty()
        && r.reply_to == 0
        && r.user.is_empty()
        && r.error.is_empty()
        && r.configuration_receipt.is_none()
        && r.management_receipt.is_none()
        && r.sanction_receipt.is_none()
        && r.report_receipt.is_none()
        && r.direct_target.is_none()
        && r.community_receipt.is_none()
        && r.tic_receipt.is_none()
        && r.guess_receipt.is_none()
        && r.word_receipt.is_none()
}
pub fn response_admitted(
    r: &Response,
    editor: Option<&Editor>,
    owner: &str,
    generation: u64,
    chat: bool,
) -> bool {
    let matching = editor.is_some_and(|e| {
        e.pending
            .as_ref()
            .is_some_and(|p| p.request_id == r.request_id)
    });
    if r.composition.is_some() || r.kind == "composition" || matching {
        return chat && editor.is_some_and(|e| e.admits(r, owner, generation));
    }
    true
}

#[cfg(test)]
mod tests {
    use super::*;
    fn snapshot(version: i64, body: &str) -> Snapshot {
        Snapshot {
            owner: "alice".into(),
            room: "private-room".into(),
            kind: "post".into(),
            title: "title".into(),
            body: body.into(),
            version,
            exists: version > 0,
        }
    }
    fn response(e: &Editor, s: Snapshot, id: i64) -> Response {
        let p = e.pending.as_ref().unwrap();
        Response {
            kind: "composition".into(),
            request_id: p.request_id.clone(),
            room: e.room.clone(),
            maintenance: None,
            composition: Some(Receipt {
                operation: p.request.operation,
                edit_generation: p.request.edit_generation,
                expected_version: p.request.expected_version,
                snapshot: s,
                object_id: id,
                committed: !matches!(
                    p.request.operation,
                    Operation::Load | Operation::Remote | Operation::Reload
                ),
            }),
            ..Response::default()
        }
    }
    fn ready() -> Editor {
        let mut e = Editor::new(
            "alice".into(),
            "private-room".into(),
            "post".into(),
            "title".into(),
            7,
        )
        .unwrap();
        e.begin(Operation::Load, "load".into(), 0).unwrap();
        let mut s = snapshot(0, "");
        s.title.clear();
        let r = response(&e, s, 0);
        assert!(e.admits(&r, "alice", 7));
        assert_eq!(e.accept(&r), Some(Effect::NeedReserve));
        let reserve = e.begin(Operation::Reserve, "reserve".into(), 1).unwrap();
        assert_eq!(reserve.expected_version, Some(0));
        let r = response(&e, snapshot(1, ""), 0);
        assert!(e.admits(&r, "alice", 7));
        assert_eq!(e.accept(&r), Some(Effect::Ready));
        e
    }
    #[test]
    fn canonical_save_ack_and_close_bind_submitted_generation_not_new_input() {
        let mut e = ready();
        e.edit(Slot::Body, "A\tB\n".into(), 2);
        assert!(!e.request_close());
        let sent = e.begin(Operation::Save, "save".into(), 3).unwrap();
        assert_eq!(sent.body, "A\tB\n");
        e.edit(Slot::Body, "A\tB\nnewer 中文".into(), 4);
        let r = response(&e, snapshot(7, "A    B"), 0);
        assert!(e.admits(&r, "alice", 7));
        assert_eq!(e.accept(&r), Some(Effect::Saved));
        assert_eq!(e.body, "A\tB\nnewer 中文");
        assert!(e.dirty());
        assert!(e.close_after.is_none());
        assert_eq!(e.snapshot.as_ref().unwrap().version, 7);
        assert!(!e.request_close());
        e.begin(Operation::Save, "save-current".into(), 5).unwrap();
        let r = response(&e, snapshot(9, "A    B\nnewer 中文"), 0);
        assert_eq!(e.accept(&r), Some(Effect::Close));
        assert!(!e.dirty());
        assert_eq!(e.body, "A\tB\nnewer 中文");
    }
    #[test]
    fn publication_ack_never_deletes_input_typed_after_submission() {
        for newer in [false, true] {
            let mut e = ready();
            e.edit(Slot::Body, "published\ttext\n".into(), 2);
            e.begin(Operation::Publish, "publish".into(), 3).unwrap();
            if newer {
                e.edit(Slot::Body, "new local text".into(), 4);
            }
            let r = response(&e, snapshot(6, ""), 44);
            assert!(e.admits(&r, "alice", 7));
            assert_eq!(
                e.accept(&r),
                Some(Effect::Published {
                    object_id: 44,
                    newer_input: newer
                })
            );
            assert_eq!(e.body, if newer { "new local text" } else { "" });
            assert_eq!(e.dirty(), newer);
            assert_eq!(e.snapshot.as_ref().unwrap().version, 6);
        }
    }
    #[test]
    fn real_zero_absent_version_and_receipts_reject_mixed_owner_scope_old_generation() {
        let mut e = ready();
        e.edit(Slot::Body, "private body".into(), 2);
        e.begin(Operation::Save, "save".into(), 3).unwrap();
        let good = response(&e, snapshot(2, "private body"), 0);
        assert!(response_admitted(&good, Some(&e), "alice", 7, true));
        assert!(!response_admitted(&good, Some(&e), "bob", 7, true));
        assert!(!response_admitted(&good, Some(&e), "alice", 8, true));
        assert!(!response_admitted(&good, Some(&e), "alice", 7, false));
        assert!(!response_admitted(&good, None, "alice", 7, true));
        let mut wrong = good.clone();
        wrong.composition.as_mut().unwrap().snapshot.room = "lobby".into();
        assert!(!e.admits(&wrong, "alice", 7));
        wrong = good.clone();
        wrong.composition.as_mut().unwrap().expected_version = None;
        assert!(!e.admits(&wrong, "alice", 7));
        wrong = good.clone();
        wrong.composition.as_mut().unwrap().edit_generation += 1;
        assert!(!e.admits(&wrong, "alice", 7));
        wrong = good.clone();
        wrong.preferences = Some(Default::default());
        assert!(!e.admits(&wrong, "alice", 7));
        let error = Response {
            kind: "error".into(),
            request_id: "save".into(),
            error: "static error".into(),
            ..Response::default()
        };
        assert!(e.admits(&error, "alice", 7));
        let mut mixed = error;
        mixed.composition = good.composition.clone();
        assert!(!e.admits(&mixed, "alice", 7));
        let state = Response {
            kind: "state".into(),
            preferences: Some(Default::default()),
            ..Response::default()
        };
        assert!(response_admitted(&state, Some(&e), "alice", 7, true));
        assert!(!format!("{:?}", good).contains("private body"));
    }
    #[test]
    fn remote_reload_and_keep_are_explicit_version_bound_without_auto_retry() {
        let mut e = ready();
        e.edit(Slot::Body, "unsaved local".into(), 1);
        e.begin(Operation::Save, "old-save".into(), 2).unwrap();
        e.failed("old-save", "conflict");
        assert!(e.blocked && e.dirty());
        assert!(!e.autosave_due(9000));
        e.begin(Operation::Remote, "remote".into(), 3).unwrap();
        let r = response(&e, snapshot(8, "remote private"), 0);
        assert!(e.admits(&r, "alice", 7));
        assert_eq!(e.accept(&r), Some(Effect::Remote));
        assert_eq!(e.body, "unsaved local");
        let request = e.keep_local("keep".into(), 4).unwrap();
        assert_eq!(request.expected_version, Some(8));
        assert_eq!(request.body, "unsaved local");
        e.failed("keep", "another remote update");
        assert!(e.blocked && e.remote.is_none());
        assert!(e.keep_local("cannot retry".into(), 5).is_none());
        e.begin(Operation::Remote, "remote-again".into(), 6)
            .unwrap();
        let r = response(&e, snapshot(10, "new remote"), 0);
        e.accept(&r);
        e.begin(Operation::Reload, "reload".into(), 7).unwrap();
        e.edit(Slot::Body, "newer local while loading".into(), 8);
        let r = response(&e, snapshot(10, "new remote"), 0);
        assert!(e.admits(&r, "alice", 7));
        assert_eq!(e.accept(&r), None);
        assert_eq!(e.body, "newer local while loading");
    }
    #[test]
    fn interruption_is_owner_isolated_and_unknown_publish_never_retries() {
        let mut e = ready();
        e.edit(Slot::Body, "retained owner text".into(), 1);
        e.begin(Operation::Publish, "publish".into(), 2).unwrap();
        let r = response(&e, snapshot(2, ""), 90);
        e.interrupt();
        assert!(e.pending.is_none() && e.blocked && e.frozen);
        assert_eq!(e.body, "retained owner text");
        assert!(!response_admitted(&r, Some(&e), "alice", 7, true));
        assert!(!e.autosave_due(99999));
        assert!(e.begin(Operation::Publish, "retry".into(), 3).is_none());
        assert!(!e.edit(Slot::Body, "credential input".into(), 4));
    }
    #[test]
    fn close_during_open_waits_for_matching_reserve_and_no_old_ack_revives() {
        let mut e = Editor::new(
            "alice".into(),
            "private-room".into(),
            "post".into(),
            "title".into(),
            7,
        )
        .unwrap();
        e.begin(Operation::Load, "load".into(), 0).unwrap();
        assert!(!e.request_close());
        let mut empty = snapshot(0, "");
        empty.title.clear();
        let r = response(&e, empty, 0);
        assert_eq!(e.accept(&r), Some(Effect::NeedReserve));
        e.begin(Operation::Reserve, "reserve".into(), 1).unwrap();
        let r = response(&e, snapshot(1, "server retained body"), 0);
        assert_eq!(e.accept(&r), Some(Effect::Close));
        assert!(!e.dirty());
        assert!(!e.admits(&r, "alice", 7));
    }
    #[test]
    fn ime_multiline_raw_selection_serialization_and_autosave_are_editor_only() {
        let mut e = ready();
        let mut value = "A😀\n中文".to_string();
        let mut selection = 1..1;
        let mut marked = None;
        assert!(crate::editing::replace(
            &mut value,
            &mut selection,
            &mut marked,
            Some(1..3),
            "候选",
            32768
        ));
        assert_eq!(value, "A候选\n中文");
        assert_eq!(selection, 7..7);
        assert!(e.edit(Slot::Body, value.clone(), 100));
        assert!(!e.autosave_due(1599));
        assert!(e.autosave_due(1600));
        let data = e.begin(Operation::Save, "save".into(), 1600).unwrap();
        let mut request = crate::session::Request::simple("composition", "save");
        request.room = "private-room";
        request.composition = Some(&data);
        let wire = serde_json::to_value(request).unwrap();
        assert!(wire.get("body").is_none() || wire["body"] == "");
        assert_eq!(wire["composition"]["body"], value);
        assert!(
            e.begin(Operation::Publish, "publish".into(), 1601)
                .is_none()
        );
        assert!(e.edit(Slot::Body, "newer".into(), 1602));
        assert!(e.expire(11600));
        assert!(e.blocked && e.pending.is_none() && e.body == "newer");
        assert!(!e.autosave_due(999999));
    }
    #[test]
    fn accepted_scope_change_blocks_same_batch_remote_reload_but_keeps_commit_ack() {
        for op in [Operation::Remote, Operation::Reload] {
            let mut e = ready();
            e.edit(Slot::Body, "original local".into(), 2);
            if op == Operation::Reload {
                e.remote = Some(snapshot(3, "private remote"));
            }
            e.begin(op, "read".into(), 3).unwrap();
            let r = response(&e, snapshot(3, "private remote"), 0);
            assert!(e.scope_changed("fallback", false));
            assert!(e.frozen && e.remote.is_none() && e.pending.is_none());
            assert!(!e.admits(&r, "alice", 7));
            assert_eq!(e.accept(&r), None);
            assert_eq!(e.body, "original local");
            e.scope_changed("private-room", false);
            assert!(e.frozen);
            assert!(!e.admits(&r, "alice", 7));
        }
        let mut e = ready();
        e.edit(Slot::Body, "submitted".into(), 2);
        e.begin(Operation::Publish, "write".into(), 3).unwrap();
        let r = response(&e, snapshot(2, ""), 18);
        e.scope_changed("fallback", false);
        assert!(e.pending.is_some() && e.admits(&r, "alice", 7));
        assert_eq!(
            e.accept(&r),
            Some(Effect::Published {
                object_id: 18,
                newer_input: false
            })
        );
    }
}
