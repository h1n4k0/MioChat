//! Custom-image emoji references: memory-only static metadata, never original
//! image bytes, chat bodies, drafts, credentials, filenames used as paths, or URLs.
use crate::attachments::{
    Binding, Data, ITEM_LIMIT, Info, MAX_DIMENSION, MAX_FRAMES, Operation, PIXEL_BUDGET, Purpose,
    Receipt, valid_intent_key, valid_upload_id,
};
use crate::composition::Origin;
pub const RECENT_LIMIT: usize = 24;
#[derive(Clone, PartialEq, Eq)]
struct Authority {
    owner: String,
    origin: Origin,
}
impl Authority {
    fn of(b: &Binding) -> Self {
        Self {
            owner: b.owner.clone(),
            origin: b.origin.clone(),
        }
    }
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Entry {
    pub source_message_id: i64,
    pub owner_uid: i64,
    pub info: Info,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Phase {
    Preview,
    Pending,
    Unknown,
    Committed,
    Rejected,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ReferenceAction {
    Operation(Operation),
    Clear,
}
#[derive(Clone)]
pub struct ReferenceTicket {
    context: Binding,
    original: Binding,
    key: String,
    phase: Phase,
    revision: u64,
    action: ReferenceAction,
}
impl ReferenceTicket {
    pub fn action(&self) -> ReferenceAction {
        self.action
    }
}
struct Pending {
    id: String,
    op: Operation,
    generation: u64,
}
pub struct Reference {
    pub binding: Binding,
    pub entry: Entry,
    pub intent_key: String,
    pub phase: Phase,
    pub message_id: i64,
    pub unavailable: bool,
    pub notice: String,
    pending: Option<Pending>,
}
impl Reference {
    pub fn pending(&self) -> bool {
        self.pending.is_some()
    }
    pub fn terminal(&self) -> bool {
        matches!(self.phase, Phase::Committed | Phase::Rejected)
    }
    pub fn current(&self, b: &Binding) -> bool {
        self.binding == *b
    }
    pub fn recoverable(&self, b: &Binding) -> bool {
        self.binding.same_authority(b) && self.phase == Phase::Unknown && !self.pending()
    }
    fn uncertain(&mut self) {
        self.pending = None;
        self.phase = Phase::Unknown;
        self.notice =
            "引用结果未知；仅查询同一操作，不会重新发送。关闭界面不能撤回已发布消息。".into();
    }
}
#[derive(Default)]
pub struct Catalog {
    authority: Option<Authority>,
    entries: Vec<Entry>,
    pub reference: Option<Reference>,
    pub notice: Option<String>,
    pub revision: u64,
}
impl Catalog {
    pub fn action_ticket(
        &self,
        action: ReferenceAction,
        current: &Binding,
    ) -> Option<ReferenceTicket> {
        let r = self.reference.as_ref()?;
        if r.pending() || !r.binding.same_authority(current) {
            return None;
        }
        Some(ReferenceTicket {
            context: current.clone(),
            original: r.binding.clone(),
            key: r.intent_key.clone(),
            phase: r.phase,
            revision: self.revision,
            action,
        })
    }
    pub fn admits_action(&self, ticket: &ReferenceTicket, current: &Binding) -> bool {
        ticket.context == *current
            && ticket.revision == self.revision
            && self.reference.as_ref().is_some_and(|r| {
                r.binding == ticket.original
                    && r.intent_key == ticket.key
                    && r.phase == ticket.phase
                    && !r.pending()
            })
    }
    pub fn items(&self, b: &Binding) -> &[Entry] {
        if self.authority.as_ref() == Some(&Authority::of(b)) {
            &self.entries
        } else {
            &[]
        }
    }
    pub fn occupied(&self) -> bool {
        self.reference.as_ref().is_some_and(|r| !r.terminal())
    }
    /// Only call after the attachment upload state machine has accepted the
    /// committed receipt. Ordinary attachments cannot enter this catalog.
    pub fn remember_committed(&mut self, purpose: Purpose, b: &Binding, r: &Receipt) -> bool {
        if purpose != Purpose::CustomEmoji
            || r.status != "committed"
            || r.room != b.room
            || r.owner_uid <= 0
            || r.message_id <= 0
            || r.source_message_id != 0
        {
            return false;
        }
        let Some(info) = r
            .attachment
            .as_ref()
            .filter(|i| valid_info(i) && i.id == r.upload_id)
        else {
            return false;
        };
        let authority = Authority::of(b);
        if self.authority.as_ref() != Some(&authority) {
            self.entries.clear();
            self.authority = Some(authority);
            self.revision = self.revision.wrapping_add(1);
        }
        self.entries
            .retain(|e| e.source_message_id != r.message_id && e.info.id != info.id);
        self.entries.insert(
            0,
            Entry {
                source_message_id: r.message_id,
                owner_uid: r.owner_uid,
                info: info.clone(),
            },
        );
        self.entries.truncate(RECENT_LIMIT);
        self.revision = self.revision.wrapping_add(1);
        true
    }
    /// Pure metadata preview. No request is produced until explicit confirmation.
    pub fn select(
        &mut self,
        b: Binding,
        source_id: i64,
        revision: u64,
        key: String,
    ) -> Result<(), &'static str> {
        if self.occupied() {
            return Err("请先核对当前表情引用，未知结果不能新建发送");
        }
        if self.revision != revision || !valid_intent_key(&key) {
            return Err("最近表情列表已变化，请重新选择");
        }
        let entry = self
            .items(&b)
            .iter()
            .find(|e| e.source_message_id == source_id)
            .cloned()
            .ok_or("该表情不属于当前账户和服务器")?;
        self.revision = self.revision.wrapping_add(1);
        self.reference = Some(Reference {
            binding: b,
            entry,
            intent_key: key,
            phase: Phase::Preview,
            message_id: 0,
            unavailable: false,
            notice: "尚未发送。请确认当前目标；服务端会重新检查原图可见性和目标权限。".into(),
            pending: None,
        });
        Ok(())
    }
    pub fn dismiss_preview_or_terminal(&mut self) -> bool {
        if self
            .reference
            .as_ref()
            .is_some_and(|r| r.phase == Phase::Preview || r.terminal())
        {
            self.reference = None;
            true
        } else {
            false
        }
    }
    pub fn begin(&mut self, op: Operation, id: String, b: &Binding) -> Result<Data, &'static str> {
        let r = self.reference.as_mut().ok_or("没有表情引用预览")?;
        if id.is_empty() || r.pending() || !r.binding.same_authority(b) {
            return Err("目标或账户已变化，不能发送");
        }
        let mut d = Data::operation(op);
        d.intent_key = r.intent_key.clone();
        match op {
            Operation::Reference if r.phase == Phase::Preview && r.current(b) => {
                d.message_id = r.entry.source_message_id;
                r.phase = Phase::Pending;
            }
            Operation::Status if r.recoverable(b) => {}
            _ => return Err("未知引用仅可查询原 intent_key，不能重新发送"),
        }
        self.revision = self.revision.wrapping_add(1);
        r.pending = Some(Pending {
            id,
            op,
            generation: b.generation,
        });
        Ok(d)
    }
    pub fn pending_matches(&self, id: &str, b: &Binding) -> bool {
        self.reference.as_ref().is_some_and(|r| {
            r.binding.same_authority(b)
                && r.pending
                    .as_ref()
                    .is_some_and(|p| p.id == id && p.generation == b.generation)
        })
    }
    pub fn transport_uncertain(&mut self, id: &str) {
        if let Some(r) = self.reference.as_mut()
            && r.pending.as_ref().is_some_and(|p| p.id == id)
        {
            r.uncertain();
            self.revision = self.revision.wrapping_add(1);
        }
    }
    /// Accept only exact reference-key/source/owner/target receipts. In
    /// particular, a normal upload status cannot masquerade as a reference.
    pub fn accept(&mut self, id: &str, b: &Binding, receipt: &Receipt) -> bool {
        if !self.pending_matches(id, b) {
            return false;
        }
        let r = self.reference.as_mut().unwrap();
        let p = r.pending.as_ref().unwrap();
        if receipt.op != Some(p.op)
            || receipt.intent_key != r.intent_key
            || receipt.room != r.binding.room
            || (receipt.source_message_id != 0
                && receipt.source_message_id != r.entry.source_message_id)
            || (receipt.owner_uid > 0 && receipt.owner_uid != r.entry.owner_uid)
        {
            return false;
        }
        if matches!(receipt.status.as_str(), "unknown" | "error") {
            r.uncertain();
            self.revision = self.revision.wrapping_add(1);
            return true;
        }
        if receipt.status == "rejected" {
            if receipt.source_message_id != r.entry.source_message_id
                || receipt.owner_uid != r.entry.owner_uid
                || receipt.message_id != 0
                || receipt.accepted_offset != 0
                || receipt.size != 0
                || receipt.attachment.is_some()
            {
                return false;
            }
            self.revision = self.revision.wrapping_add(1);
            r.pending = None;
            r.phase = Phase::Rejected;
            r.notice = "服务端明确拒绝本次引用；没有重新上传图片。".into();
            return true;
        }
        if receipt.status != "committed"
            || receipt.message_id <= 0
            || receipt.source_message_id != r.entry.source_message_id
            || receipt.owner_uid != r.entry.owner_uid
            || receipt.accepted_offset != 0
            || receipt.upload_id != r.entry.info.id
        {
            return false;
        }
        let unavailable = receipt.code == "reference_unavailable" && receipt.attachment.is_none();
        if unavailable {
            if receipt.size != 0 && receipt.size != r.entry.info.size {
                return false;
            }
        } else if receipt.size != r.entry.info.size
            || receipt.attachment.as_ref() != Some(&r.entry.info)
        {
            return false;
        }
        self.revision = self.revision.wrapping_add(1);
        r.pending = None;
        r.phase = Phase::Committed;
        r.message_id = receipt.message_id;
        r.unavailable = unavailable;
        r.notice = if unavailable {
            "已确认曾发布；该引用现在不可用。不会把不可用当作未发送或重新发布。"
        } else {
            "表情引用已发布；没有重新上传图像字节，也没有重复扣上传额度。"
        }
        .into();
        if unavailable {
            let source = r.entry.source_message_id;
            self.entries.retain(|e| e.source_message_id != source);
            self.revision = self.revision.wrapping_add(1);
        }
        true
    }
    /// Scope/generation change keeps metadata but removes an unconfirmed
    /// preview; pending publication becomes UNKNOWN. Credentials/Origin change
    /// clears the one account catalog and preserves only a generic notice.
    pub fn freeze(&mut self, authority_changed: bool) {
        if let Some(r) = self.reference.as_mut() {
            if r.phase == Phase::Preview || r.terminal() {
                self.reference = None;
            } else {
                r.uncertain();
            }
        }
        if authority_changed {
            if self.reference.is_some() {
                self.notice =
                    Some("之前的表情引用结果未知；本地账户数据已清除，请勿据此重复发布。".into());
            }
            self.reference = None;
            self.entries.clear();
            self.authority = None;
        }
        self.revision = self.revision.wrapping_add(1);
    }
}
fn valid_info(i: &Info) -> bool {
    valid_upload_id(&i.id)
        && i.size > 0
        && i.size <= ITEM_LIMIT as u64
        && i.metadata_policy == "preserve_original"
        && matches!(i.mime.as_str(), "image/png" | "image/jpeg" | "image/gif")
        && !i.filename.is_empty()
        && i.filename.len() <= 180
        && !i.filename.chars().any(|c| c.is_control())
        && i.sha256.len() == 64
        && i.sha256
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        && i.width > 0
        && i.height > 0
        && i.width <= MAX_DIMENSION
        && i.height <= MAX_DIMENSION
        && i.frames > 0
        && i.frames <= MAX_FRAMES
        && i.width as u64 * i.height as u64 * i.frames as u64 <= PIXEL_BUDGET
}
#[cfg(test)]
mod tests {
    use super::*;
    fn binding() -> Binding {
        Binding::new(
            Origin::synthetic("test", 1),
            "owner".into(),
            "room-a".into(),
            3,
        )
        .unwrap()
    }
    fn committed(id: i64) -> Receipt {
        Receipt {
            op: Some(Operation::Commit),
            status: "committed".into(),
            room: "room-a".into(),
            intent_key: "upload_key_123".into(),
            upload_id: format!("{id:032x}"),
            owner_uid: 7,
            size: 6,
            accepted_offset: 6,
            message_id: id,
            attachment: Some(Info {
                id: format!("{id:032x}"),
                filename: "emoji.png".into(),
                mime: "image/png".into(),
                size: 6,
                sha256: "a".repeat(64),
                width: 1,
                height: 1,
                frames: 1,
                metadata_policy: "preserve_original".into(),
            }),
            ..Default::default()
        }
    }
    fn catalog() -> (Catalog, Binding) {
        let b = binding();
        let mut c = Catalog::default();
        assert!(c.remember_committed(Purpose::CustomEmoji, &b, &committed(11)));
        (c, b)
    }
    #[test]
    fn only_successful_custom_images_enter_bounded_account_catalog() {
        let b = binding();
        let mut c = Catalog::default();
        assert!(!c.remember_committed(Purpose::Attachment, &b, &committed(1)));
        for id in 1..=30 {
            assert!(c.remember_committed(Purpose::CustomEmoji, &b, &committed(id)));
        }
        assert_eq!(c.items(&b).len(), 24);
        assert_eq!(c.items(&b)[0].source_message_id, 30);
        let mut other = b.clone();
        other.owner = "other".into();
        assert!(c.items(&other).is_empty());
        other = b.clone();
        other.origin = Origin::synthetic("other", 1);
        assert!(c.items(&other).is_empty());
        other = b.clone();
        other.generation += 1;
        assert_eq!(c.items(&other).len(), 24);
    }
    #[test]
    fn preview_does_not_send_and_unknown_never_references_again() {
        let (mut c, mut b) = catalog();
        b.room = "target".into();
        c.select(b.clone(), 11, c.revision, "reference_key_123".into())
            .unwrap();
        assert_eq!(c.reference.as_ref().unwrap().phase, Phase::Preview);
        let d = c
            .begin(Operation::Reference, "emoji-attachment:3:1".into(), &b)
            .unwrap();
        assert_eq!(d.message_id, 11);
        assert!(d.data_b64.is_empty());
        c.transport_uncertain("emoji-attachment:3:1");
        assert!(c.begin(Operation::Reference, "again".into(), &b).is_err());
        b.generation += 1;
        let d = c.begin(Operation::Status, "status".into(), &b).unwrap();
        assert_eq!(d.intent_key, "reference_key_123");
        assert_eq!(d.message_id, 0);
        assert!(d.upload_id.is_empty());
    }
    #[test]
    fn references_bind_source_owner_and_fixed_target() {
        let (mut c, mut b) = catalog();
        b.room = "target".into();
        c.select(b.clone(), 11, c.revision, "reference_key_123".into())
            .unwrap();
        c.begin(Operation::Reference, "ref".into(), &b).unwrap();
        let mut r = committed(11);
        r.op = Some(Operation::Reference);
        r.intent_key = "reference_key_123".into();
        r.room = "target".into();
        r.source_message_id = 11;
        r.message_id = 90;
        r.accepted_offset = 0;
        r.owner_uid = 8;
        assert!(!c.accept("ref", &b, &r));
        r.owner_uid = 7;
        r.source_message_id = 12;
        assert!(!c.accept("ref", &b, &r));
        r.source_message_id = 11;
        r.accepted_offset = 6;
        assert!(!c.accept("ref", &b, &r));
        r.accepted_offset = 0;
        assert!(c.accept("ref", &b, &r));
        assert_eq!(c.reference.as_ref().unwrap().phase, Phase::Committed);
        assert_eq!(c.items(&b).len(), 1);
    }
    #[test]
    fn committed_but_unavailable_is_not_rollback() {
        let (mut c, b) = catalog();
        c.select(b.clone(), 11, c.revision, "reference_key_123".into())
            .unwrap();
        c.begin(Operation::Reference, "ref".into(), &b).unwrap();
        c.transport_uncertain("ref");
        c.begin(Operation::Status, "status".into(), &b).unwrap();
        let mut r = committed(11);
        r.op = Some(Operation::Status);
        r.intent_key = "reference_key_123".into();
        r.source_message_id = 11;
        r.message_id = 90;
        r.accepted_offset = 0;
        r.code = "reference_unavailable".into();
        r.attachment = None;
        assert!(c.accept("status", &b, &r));
        assert_eq!(c.reference.as_ref().unwrap().phase, Phase::Committed);
        assert!(c.reference.as_ref().unwrap().unavailable);
        assert!(c.items(&b).is_empty());
    }
    #[test]
    fn stale_reference_pointer_cannot_send_or_close_replacement_preview() {
        let (mut c, b) = catalog();
        c.select(b.clone(), 11, c.revision, "reference_key_one".into())
            .unwrap();
        let old = c
            .action_ticket(ReferenceAction::Operation(Operation::Reference), &b)
            .unwrap();
        let close = c.action_ticket(ReferenceAction::Clear, &b).unwrap();
        assert!(c.admits_action(&old, &b));
        c.dismiss_preview_or_terminal();
        c.select(b.clone(), 11, c.revision, "reference_key_two".into())
            .unwrap();
        assert!(!c.admits_action(&old, &b));
        assert!(!c.admits_action(&close, &b));
    }
    #[test]
    fn durable_rejected_is_terminal_but_never_mislabels_a_committed_message() {
        let (mut c, b) = catalog();
        c.select(b.clone(), 11, c.revision, "reference_key_123".into())
            .unwrap();
        c.begin(Operation::Reference, "ref".into(), &b).unwrap();
        let mut r = Receipt {
            op: Some(Operation::Reference),
            status: "rejected".into(),
            intent_key: "reference_key_123".into(),
            room: b.room.clone(),
            source_message_id: 11,
            owner_uid: 7,
            message_id: 90,
            ..Default::default()
        };
        assert!(!c.accept("ref", &b, &r));
        r.message_id = 0;
        assert!(c.accept("ref", &b, &r));
        assert_eq!(c.reference.as_ref().unwrap().phase, Phase::Rejected);
    }
    #[test]
    fn stale_picker_and_authority_freeze_cannot_leak() {
        let (mut c, b) = catalog();
        let rev = c.revision;
        c.freeze(false);
        assert!(
            c.select(b.clone(), 11, rev, "reference_key_123".into())
                .is_err()
        );
        c.select(b.clone(), 11, c.revision, "reference_key_123".into())
            .unwrap();
        c.begin(Operation::Reference, "ref".into(), &b).unwrap();
        c.freeze(true);
        assert!(c.reference.is_none());
        assert!(c.items(&b).is_empty());
        assert!(c.notice.is_some());
    }
}
