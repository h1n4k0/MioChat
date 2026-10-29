//! Bounded, owner/origin/scope-bound attachments-v1 state. This module performs
//! no networking, executes no files, fetches no URLs and persists no source bytes.
use crate::composition::Origin;
use serde::{Deserialize, Serialize};
use std::{collections::VecDeque, fs::File, io::Read, path::Path};

pub const CAPABILITY: &str = "attachments-v1";
pub const ITEM_LIMIT: usize = 20_000_000;
pub const DAILY_LIMIT: u64 = 100_000_000;
pub const CHUNK_BYTES: usize = 24 * 1024;
pub const LINE_LIMIT: usize = 64 * 1024;
pub const PIXEL_BUDGET: u64 = 8 * 1024 * 1024;
pub const MAX_DIMENSION: u32 = 4096;
pub const MAX_FRAMES: u32 = 60;
pub const DISCLOSURE: &str = "单文件最多 20,000,000 字节；每日 100,000,000 字节，UTC+08:00 零点重置。服务器按已接收的原始字节计入上传额度，取消或失败不退还已接收额度。原图原样上传，EXIF/位置等元数据可能保留。";

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Operation {
    Intent,
    Chunk,
    Commit,
    Cancel,
    Status,
    Fetch,
    Limits,
    Reference,
}
#[derive(Clone, Default, Serialize)]
pub struct Data {
    pub op: Option<Operation>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub intent_key: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub upload_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub filename: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub mime: String,
    #[serde(skip_serializing_if = "is_zero")]
    pub size: u64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub sha256: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub caption: String,
    #[serde(skip_serializing_if = "is_zero")]
    pub offset: u64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub data_b64: String,
    #[serde(skip_serializing_if = "is_zero_i64")]
    pub message_id: i64,
}
fn is_zero(n: &u64) -> bool {
    *n == 0
}
fn is_zero_i64(n: &i64) -> bool {
    *n == 0
}
impl Data {
    pub fn operation(op: Operation) -> Self {
        Self {
            op: Some(op),
            ..Self::default()
        }
    }
}
#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq)]
pub struct Info {
    pub id: String,
    pub filename: String,
    pub mime: String,
    pub size: u64,
    pub sha256: String,
    #[serde(default)]
    pub width: u32,
    #[serde(default)]
    pub height: u32,
    #[serde(default)]
    pub frames: u32,
    pub metadata_policy: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Quota {
    pub day: String,
    pub used: u64,
    pub reserved: u64,
    pub daily_limit: u64,
    pub item_limit: u64,
    pub reset_at: i64,
    pub meter: String,
}
impl Quota {
    pub fn remaining(&self) -> u64 {
        self.daily_limit
            .saturating_sub(self.used)
            .saturating_sub(self.reserved)
    }
}
#[derive(Clone, Default, Deserialize)]
pub struct Receipt {
    pub op: Option<Operation>,
    pub status: String,
    #[serde(default)]
    pub code: String,
    #[serde(default)]
    pub intent_key: String,
    #[serde(default)]
    pub upload_id: String,
    pub room: String,
    #[serde(default)]
    pub owner_uid: i64,
    #[serde(default)]
    pub accepted_offset: u64,
    #[serde(default)]
    pub size: u64,
    #[serde(default)]
    pub expires_at: i64,
    #[serde(default)]
    pub message_id: i64,
    #[serde(default)]
    pub source_message_id: i64,
    pub attachment: Option<Info>,
    pub quota: Option<Quota>,
    #[serde(default)]
    pub offset: u64,
    #[serde(default)]
    pub data_b64: String,
    #[serde(default)]
    pub more: bool,
    #[serde(default)]
    pub chunk_limit: usize,
    #[serde(default)]
    pub allowed_mimes: Vec<String>,
}
impl std::fmt::Debug for Receipt {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("AttachmentReceipt")
            .field("op", &self.op)
            .field("status", &self.status)
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}
#[derive(Clone, Eq, PartialEq)]
pub struct Binding {
    pub origin: Origin,
    pub owner: String,
    pub room: String,
    pub generation: u64,
}
impl Binding {
    pub fn new(origin: Origin, owner: String, room: String, generation: u64) -> Option<Self> {
        (!owner.is_empty() && !room.is_empty()).then_some(Self {
            origin,
            owner,
            room,
            generation,
        })
    }
    pub fn same_authority(&self, other: &Self) -> bool {
        self.origin == other.origin && self.owner == other.owner && self.room == other.room
    }
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Purpose {
    Attachment,
    CustomEmoji,
}
#[derive(Clone)]
pub struct Selection {
    binding: Binding,
    revision: u64,
    purpose: Purpose,
}

/// The immutable original selected bytes. Preview, digest and upload all refer
/// to this buffer, never a path that could later point at a different inode.
pub struct Prepared {
    pub info: Info,
    bytes: Vec<u8>,
}
impl Prepared {
    pub fn from_bytes(filename: String, bytes: Vec<u8>) -> Result<Self, &'static str> {
        if bytes.is_empty() || bytes.len() > ITEM_LIMIT {
            return Err("文件为空或超过 20,000,000 字节");
        }
        let mime = mime_for_filename(&filename).ok_or("不支持该文件类型")?;
        if !filename_valid(&filename) {
            return Err("文件名含有不安全字符或过长");
        }
        let (width, height, frames) = image_budget(mime, &bytes)?;
        let info = Info {
            id: String::new(),
            filename,
            mime: mime.into(),
            size: bytes.len() as u64,
            sha256: sha256_hex(&bytes),
            width,
            height,
            frames,
            metadata_policy: "preserve_original".into(),
        };
        Ok(Self { info, bytes })
    }
    pub fn bytes(&self) -> &[u8] {
        &self.bytes
    }
    pub fn image(&self) -> bool {
        self.info.mime.starts_with("image/")
    }
}
/// Open once, reject non-regular/symlink input, check metadata on the SAME open
/// handle before/after bounded read. No path is retained or included in errors.
pub fn read_selected_file(path: &Path) -> Result<Prepared, &'static str> {
    let before = std::fs::symlink_metadata(path).map_err(|_| "无法读取选中文件")?;
    if !before.is_file()
        || before.file_type().is_symlink()
        || reparse_point(&before)
        || before.len() > ITEM_LIMIT as u64
    {
        return Err("请选择不超过 20 MB 的普通文件（不支持符号链接）");
    }
    let file = open_selected(path).map_err(|_| "无法打开选中文件")?;
    let opened = file.metadata().map_err(|_| "无法检查选中文件")?;
    if !opened.is_file()
        || reparse_point(&opened)
        || opened.len() > ITEM_LIMIT as u64
        || !same_file(&before, &opened)
    {
        return Err("选中文件已变化，请重新选择");
    }
    let mut bytes = Vec::with_capacity(opened.len() as usize);
    let mut limited = (&file).take((ITEM_LIMIT + 1) as u64);
    limited
        .read_to_end(&mut bytes)
        .map_err(|_| "读取文件失败")?;
    let after = file.metadata().map_err(|_| "无法检查选中文件")?;
    if bytes.len() > ITEM_LIMIT
        || after.len() != opened.len()
        || bytes.len() as u64 != after.len()
        || after.modified().ok() != opened.modified().ok()
        || !same_file(&opened, &after)
    {
        return Err("读取期间文件已变化，请重新选择");
    }
    let filename = path
        .file_name()
        .and_then(|v| v.to_str())
        .ok_or("文件名不是有效文本")?
        .to_owned();
    Prepared::from_bytes(filename, bytes)
}
fn open_selected(path: &Path) -> std::io::Result<File> {
    let mut options = std::fs::OpenOptions::new();
    options.read(true);
    // Refuse symlink swaps and avoid blocking on a swapped FIFO/device. Check
    // regular-file metadata on the resulting handle before reading any bytes.
    #[cfg(target_os = "linux")]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.custom_flags(0x20000 | 0x800);
    }
    #[cfg(target_os = "macos")]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.custom_flags(0x100 | 0x4);
    }
    #[cfg(windows)]
    {
        use std::os::windows::fs::OpenOptionsExt;
        // FILE_FLAG_OPEN_REPARSE_POINT; FILE_SHARE_READ only (no write/delete).
        options.custom_flags(0x0020_0000).share_mode(0x0000_0001);
    }
    options.open(path)
}
#[cfg(windows)]
fn reparse_point(metadata: &std::fs::Metadata) -> bool {
    use std::os::windows::fs::MetadataExt;
    metadata.file_attributes() & 0x400 != 0
}
#[cfg(not(windows))]
fn reparse_point(_: &std::fs::Metadata) -> bool {
    false
}

#[cfg(unix)]
fn same_file(a: &std::fs::Metadata, b: &std::fs::Metadata) -> bool {
    use std::os::unix::fs::MetadataExt;
    a.dev() == b.dev()
        && a.ino() == b.ino()
        && a.ctime() == b.ctime()
        && a.ctime_nsec() == b.ctime_nsec()
}
// Windows uses a single open handle with write/delete sharing denied and rejects
// reparse points. These stable metadata comparisons are NOT a strong file-ID
// proof. Windows real selection validation remains pending; Unix dev+ino checks
// must not be described as evidence that Windows identity checks passed.
#[cfg(not(unix))]
fn same_file(a: &std::fs::Metadata, b: &std::fs::Metadata) -> bool {
    a.len() == b.len()
        && a.created().ok() == b.created().ok()
        && a.modified().ok() == b.modified().ok()
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Phase {
    Preview,
    Uploading,
    ReadyToCommit,
    Paused,
    Unknown,
    Committed,
    Cancelled,
    Expired,
    Rejected,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum LocalAction {
    Operation(Operation),
    Resume,
    Discard,
    ForgetUnknown,
}
#[derive(Clone)]
pub struct ActionTicket {
    context: Binding,
    upload_binding: Binding,
    intent_key: String,
    phase: Phase,
    revision: u64,
    action: LocalAction,
    lifecycle: u64,
}
impl ActionTicket {
    pub fn action(&self) -> LocalAction {
        self.action
    }
}
struct Pending {
    id: String,
    op: Operation,
    expected: u64,
    generation: u64,
}
pub struct Upload {
    pub binding: Binding,
    pub info: Info,
    pub purpose: Purpose,
    pub phase: Phase,
    pub intent_key: String,
    pub upload_id: String,
    pub accepted: u64,
    pub message_id: i64,
    pub quota: Option<Quota>,
    pub notice: String,
    owner_uid: Option<i64>,
    source: Option<Prepared>,
    pending: Option<Pending>,
    confirmed: bool,
    revision: u64,
}
impl Upload {
    pub fn pending(&self) -> bool {
        self.pending.is_some()
    }
    pub fn source(&self) -> Option<&Prepared> {
        self.source.as_ref()
    }
    pub fn terminal(&self) -> bool {
        matches!(
            self.phase,
            Phase::Committed | Phase::Cancelled | Phase::Expired | Phase::Rejected
        )
    }
    pub fn can_recover(&self, binding: &Binding) -> bool {
        self.binding.same_authority(binding) && !self.intent_key.is_empty() && !self.terminal()
    }
    pub fn can_resume(&self, binding: &Binding) -> bool {
        self.binding == *binding
            && self.source.is_some()
            && self.phase == Phase::Paused
            && !self.pending()
    }
    fn unknown(&mut self) {
        self.revision = self.revision.wrapping_add(1);
        self.pending = None;
        self.phase = Phase::Unknown;
        self.notice = "结果未知；只可查询原上传，不会自动重新发送。取消不能撤回已发布消息。".into();
    }
    fn wipe(&mut self) {
        self.source = None;
        self.info.filename.clear();
        self.notice =
            "原上传结果待核对；本地文件字节已清除。请回到原服务器、账户和会话范围查询。".into();
    }
}
#[derive(Default)]
pub struct Attachments {
    pub supported: bool,
    pub upload: Option<Upload>,
    pub notice: Option<String>,
    revision: u64,
    selection: Option<Selection>,
}
impl Attachments {
    pub fn action_ticket(&self, action: LocalAction, current: &Binding) -> Option<ActionTicket> {
        let u = self.upload.as_ref()?;
        if u.pending() || !u.binding.same_authority(current) {
            return None;
        }
        Some(ActionTicket {
            context: current.clone(),
            upload_binding: u.binding.clone(),
            intent_key: u.intent_key.clone(),
            phase: u.phase,
            revision: u.revision,
            action,
            lifecycle: self.revision,
        })
    }
    pub fn admits_action(&self, ticket: &ActionTicket, current: &Binding) -> bool {
        ticket.context == *current
            && ticket.lifecycle == self.revision
            && self.upload.as_ref().is_some_and(|u| {
                u.binding == ticket.upload_binding
                    && u.intent_key == ticket.intent_key
                    && u.phase == ticket.phase
                    && u.revision == ticket.revision
                    && !u.pending()
            })
    }
    /// Explicit local-only exit; not a cancellation, rollback, or permission to
    /// automatically create another intent. Retains no filename/bytes/identity.
    pub fn forget_unknown_local(&mut self) -> bool {
        if self
            .upload
            .as_ref()
            .is_some_and(|u| u.phase == Phase::Unknown && !u.pending())
        {
            self.upload = None;
            self.notice=Some("仅清除了本地附件记录；原附件可能已发布或仍在服务器处理。这不代表取消或回滚，不会自动重试；再次上传可能重复。".into());
            true
        } else {
            false
        }
    }
    pub fn begin_selection(
        &mut self,
        binding: Binding,
        purpose: Purpose,
    ) -> Result<Selection, &'static str> {
        if !self.supported {
            return Err("该服务器不支持附件（需要 attachments-v1）");
        }
        if self.selection.is_some() || self.upload.as_ref().is_some_and(|u| !u.terminal()) {
            return Err("请先处理当前附件，结果未知时不能新建上传");
        }
        self.revision = self.revision.wrapping_add(1);
        let ticket = Selection {
            binding,
            revision: self.revision,
            purpose,
        };
        self.selection = Some(ticket.clone());
        self.upload = None;
        Ok(ticket)
    }
    pub fn selection_matches(&self, ticket: &Selection, current: &Binding) -> bool {
        self.supported
            && &ticket.binding == current
            && self
                .selection
                .as_ref()
                .is_some_and(|s| s.revision == ticket.revision && s.binding == ticket.binding)
    }
    pub fn finish_selection(
        &mut self,
        ticket: &Selection,
        current: &Binding,
        source: Prepared,
        intent_key: String,
    ) -> Result<(), &'static str> {
        if !self.selection_matches(ticket, current) {
            return Err("选择已失效，文件没有发送");
        }
        self.selection = None;
        if !valid_intent_key(&intent_key) {
            return Err("上传标识无效");
        }
        if ticket.purpose == Purpose::CustomEmoji && !source.image() {
            return Err("自定义表情只支持 PNG、JPEG、GIF");
        }
        self.upload = Some(Upload {
            binding: current.clone(),
            info: source.info.clone(),
            purpose: ticket.purpose,
            phase: Phase::Preview,
            intent_key,
            upload_id: String::new(),
            accepted: 0,
            message_id: 0,
            quota: None,
            notice: String::new(),
            owner_uid: None,
            source: Some(source),
            pending: None,
            confirmed: false,
            revision: 0,
        });
        Ok(())
    }
    /// An older native chooser must never cancel a newer selection.
    pub fn cancel_selection_if_current(&mut self, ticket: &Selection, current: &Binding) -> bool {
        if !self.selection_matches(ticket, current) {
            return false;
        }
        self.cancel_selection();
        true
    }
    pub fn cancel_selection(&mut self) {
        self.selection = None;
        self.revision = self.revision.wrapping_add(1);
    }
    /// Invoke immediately on scope/generation departure. Credential/authority
    /// invalidation forgets recovery identity too; only a generic notice survives.
    pub fn invalidate(&mut self, credential_or_authority_change: bool) {
        self.cancel_selection();
        if let Some(u) = self.upload.as_mut() {
            if (u.phase == Phase::Preview && !u.pending()) || u.terminal() {
                self.upload = None;
            } else {
                u.unknown();
                u.wipe();
                if credential_or_authority_change {
                    self.upload = None;
                    self.notice =
                        Some("之前附件结果未知；本地敏感数据已清除。不要据此重复上传。".into());
                }
            }
        }
    }
    pub fn discard_preview(&mut self) -> bool {
        if self
            .upload
            .as_ref()
            .is_some_and(|u| u.phase == Phase::Preview || u.terminal())
        {
            self.upload = None;
            true
        } else {
            false
        }
    }
    pub fn resume(&mut self, binding: &Binding) -> bool {
        let Some(u) = self.upload.as_mut() else {
            return false;
        };
        if !u.can_resume(binding) {
            return false;
        }
        u.revision = u.revision.wrapping_add(1);
        u.phase = if u.accepted == u.info.size {
            Phase::ReadyToCommit
        } else {
            Phase::Uploading
        };
        true
    }
    pub fn next_operation(&self, current: &Binding) -> Option<Operation> {
        self.upload
            .as_ref()
            .filter(|u| self.supported && u.binding == *current && u.confirmed && !u.pending())
            .filter(|u| {
                u.phase == Phase::Uploading && u.accepted < u.info.size && u.source.is_some()
            })
            .map(|_| Operation::Chunk)
    }
    /// Intent and Commit must each be reached from explicit user confirmation.
    /// Chunk is sequential; status is the only safe retry after uncertain I/O.
    pub fn begin(
        &mut self,
        op: Operation,
        id: String,
        current: &Binding,
    ) -> Result<Data, &'static str> {
        if !self.supported || id.is_empty() {
            return Err("附件功能不可用");
        }
        let u = self.upload.as_mut().ok_or("没有附件")?;
        if u.pending() || !u.binding.same_authority(current) {
            return Err("附件范围已变化或仍在等待确认");
        }
        let mut data = Data::operation(op);
        let mut expected = u.accepted;
        match op {
            Operation::Intent if u.phase == Phase::Preview && u.binding == *current => {
                let src = u.source.as_ref().ok_or("本地文件已清除")?;
                data.intent_key = u.intent_key.clone();
                data.filename = src.info.filename.clone();
                data.mime = src.info.mime.clone();
                data.size = src.info.size;
                data.sha256 = src.info.sha256.clone();
                u.confirmed = true;
            }
            Operation::Chunk
                if u.phase == Phase::Uploading && u.binding == *current && u.confirmed =>
            {
                let src = u.source.as_ref().ok_or("本地文件已清除")?;
                let start = usize::try_from(u.accepted).map_err(|_| "无效上传偏移")?;
                if start >= src.bytes.len() || start % CHUNK_BYTES != 0 {
                    return Err("无效上传偏移");
                }
                let end = (start + CHUNK_BYTES).min(src.bytes.len());
                expected = end as u64;
                data.upload_id = u.upload_id.clone();
                data.offset = start as u64;
                data.data_b64 = encode_base64(&src.bytes[start..end]);
            }
            Operation::Commit
                if u.phase == Phase::ReadyToCommit
                    && u.binding == *current
                    && u.confirmed
                    && u.source.is_some()
                    && u.accepted == u.info.size =>
            {
                data.upload_id = u.upload_id.clone();
            }
            Operation::Status if u.can_recover(current) => {
                if u.upload_id.is_empty() {
                    data.intent_key = u.intent_key.clone();
                } else {
                    data.upload_id = u.upload_id.clone();
                }
            }
            Operation::Cancel if u.can_recover(current) && !u.upload_id.is_empty() => {
                data.upload_id = u.upload_id.clone();
            }
            _ => return Err("当前状态不允许此附件操作"),
        }
        if matches!(op, Operation::Chunk | Operation::Commit | Operation::Cancel)
            && !valid_upload_id(&data.upload_id)
        {
            return Err("无效上传标识");
        }
        u.revision = u.revision.wrapping_add(1);
        u.pending = Some(Pending {
            id,
            op,
            expected,
            generation: current.generation,
        });
        Ok(data)
    }
    pub fn pending_matches(&self, id: &str, current: &Binding) -> bool {
        self.upload.as_ref().is_some_and(|u| {
            u.binding.same_authority(current)
                && u.pending
                    .as_ref()
                    .is_some_and(|p| p.id == id && p.generation == current.generation)
        })
    }
    pub fn transport_uncertain(&mut self, id: &str) {
        if let Some(u) = self.upload.as_mut()
            && u.pending.as_ref().is_some_and(|p| p.id == id)
        {
            u.unknown();
        }
    }
    /// Returns false without projecting unrelated payloads. The caller must gate
    /// the ENTIRE outer response before applying message/room/profile effects.
    pub fn accept(&mut self, id: &str, current: &Binding, r: &Receipt) -> bool {
        if !self.pending_matches(id, current) {
            return false;
        }
        let u = self.upload.as_mut().unwrap();
        let p = u.pending.as_ref().unwrap();
        if r.op != Some(p.op)
            || r.source_message_id != 0
            || r.room != u.binding.room
            || (!r.intent_key.is_empty() && r.intent_key != u.intent_key)
            || (!u.upload_id.is_empty() && r.upload_id != u.upload_id)
            || (r.owner_uid > 0 && u.owner_uid.is_some_and(|v| v != r.owner_uid))
        {
            return false;
        }
        if matches!(r.status.as_str(), "unknown" | "error") {
            u.unknown();
            return true;
        }
        if !matches!(
            r.status.as_str(),
            "uploading" | "committed" | "cancelled" | "expired" | "rejected"
        ) {
            return false;
        }
        if !valid_upload_id(&r.upload_id)
            || r.intent_key != u.intent_key
            || r.owner_uid <= 0
            || r.size != u.info.size
            || r.accepted_offset > u.info.size
            || (r.accepted_offset != u.info.size
                && !r.accepted_offset.is_multiple_of(CHUNK_BYTES as u64))
        {
            return false;
        }
        if r.attachment.as_ref().is_none_or(|a| {
            a.id != r.upload_id
                || a.size != u.info.size
                || a.sha256 != u.info.sha256
                || a.mime != u.info.mime
                || a.metadata_policy != "preserve_original"
                || (!u.info.filename.is_empty() && a.filename != u.info.filename)
        }) {
            return false;
        }
        let operation = p.op;
        if r.status == "uploading"
            && matches!(operation, Operation::Intent | Operation::Chunk)
            && r.accepted_offset != p.expected
        {
            return false;
        }
        if r.status == "committed"
            && (r.message_id <= 0
                || r.accepted_offset != u.info.size
                || matches!(operation, Operation::Intent | Operation::Chunk))
        {
            return false;
        }
        if r.status == "uploading" && operation == Operation::Commit {
            u.unknown();
            return true;
        }
        u.pending = None;
        u.revision = u.revision.wrapping_add(1);
        u.upload_id = r.upload_id.clone();
        u.owner_uid = Some(r.owner_uid);
        u.accepted = r.accepted_offset;
        u.quota = r.quota.clone();
        u.phase = match r.status.as_str() {
            "committed" => {
                u.message_id = r.message_id;
                u.source = None;
                Phase::Committed
            }
            "cancelled" => {
                u.source = None;
                Phase::Cancelled
            }
            "expired" => {
                u.source = None;
                Phase::Expired
            }
            "rejected" => {
                u.source = None;
                Phase::Rejected
            }
            _ if operation == Operation::Status || operation == Operation::Cancel => Phase::Paused,
            _ if u.accepted == u.info.size => Phase::ReadyToCommit,
            _ => Phase::Uploading,
        };
        u.notice = match u.phase {
            Phase::Committed => "服务器已确认发布；取消不能撤回此消息。",
            Phase::ReadyToCommit => "上传完成。请再次确认目标后发布。",
            Phase::Paused => "已核对原上传状态。不会自动续传；重连后只能查询或取消原上传。",
            Phase::Cancelled => "已取消未发布上传；已接收字节仍计入当日额度。",
            Phase::Rejected => "服务器拒绝此附件；没有发布。",
            Phase::Expired => "上传已过期；没有发布。",
            _ => "正在顺序上传…",
        }
        .into();
        true
    }
}
/// A private, byte-free transport recovery slot. Never expose its Upload/Info
/// until a fresh original-UID receipt is admitted. Unknown original UID can be
/// queried for generic outcomes, but can never unlock metadata locally.
pub struct Recovery {
    state: Attachments,
    expected_uid: Option<i64>,
    notice: &'static str,
    verified: bool,
}
#[derive(Clone, Copy, Eq, PartialEq)]
pub enum RecoveryAction {
    Status,
    Forget,
}
#[derive(Clone)]
pub struct RecoveryTicket {
    context: Binding,
    original: Binding,
    key: String,
    revision: u64,
    action: RecoveryAction,
}
impl RecoveryTicket {
    pub fn action(&self) -> RecoveryAction {
        self.action
    }
}
impl Attachments {
    pub fn seal_transport_recovery(&mut self) -> Option<Recovery> {
        let mut u = self.upload.take()?;
        if (u.phase == Phase::Preview && !u.pending()) || u.terminal() {
            return None;
        }
        let expected_uid = u.owner_uid;
        u.unknown();
        u.wipe();
        Some(Recovery {
            state: Attachments {
                supported: true,
                upload: Some(u),
                ..Default::default()
            },
            expected_uid,
            notice: "存在一次断线前未核对的附件操作。尚未验证原账户身份，不显示文件元信息，不自动查询或重发。",
            verified: false,
        })
    }
}
impl Recovery {
    fn upload(&self) -> &Upload {
        self.state
            .upload
            .as_ref()
            .expect("sealed recovery owns upload")
    }
    pub fn same_origin(&self, origin: &Origin) -> bool {
        self.upload().binding.origin == *origin
    }
    pub fn same_owner_origin(&self, b: &Binding) -> bool {
        let u = self.upload();
        u.binding.origin == b.origin && u.binding.owner == b.owner
    }
    pub fn in_scope(&self, b: &Binding) -> bool {
        self.same_owner_origin(b) && self.upload().binding.room == b.room
    }
    pub fn notice(&self) -> &'static str {
        self.notice
    }
    pub fn pending_matches(&self, id: &str, b: &Binding) -> bool {
        self.state.pending_matches(id, b)
    }
    pub fn ticket(&self, action: RecoveryAction, b: &Binding) -> Option<RecoveryTicket> {
        let u = self.upload();
        if !self.same_owner_origin(b) || u.pending() {
            return None;
        }
        if action == RecoveryAction::Status && (!self.in_scope(b) || !u.can_recover(b)) {
            return None;
        }
        Some(RecoveryTicket {
            context: b.clone(),
            original: u.binding.clone(),
            key: u.intent_key.clone(),
            revision: u.revision,
            action,
        })
    }
    pub fn admits(&self, t: &RecoveryTicket, b: &Binding) -> bool {
        let u = self.upload();
        t.context == *b
            && t.original == u.binding
            && t.key == u.intent_key
            && t.revision == u.revision
            && !u.pending()
            && self.same_owner_origin(b)
    }
    pub fn begin_status(
        &mut self,
        t: &RecoveryTicket,
        id: String,
        b: &Binding,
    ) -> Result<Data, &'static str> {
        if t.action != RecoveryAction::Status || !self.admits(t, b) {
            return Err("恢复查询已经失效");
        }
        self.state.begin(Operation::Status, id, b)
    }
    pub fn interrupt(&mut self) {
        if let Some(u) = self.state.upload.as_mut() {
            u.unknown();
        }
        self.verified = false;
    }
    pub fn transport_uncertain(&mut self, id: &str) {
        self.state.transport_uncertain(id);
        self.notice = "原上传查询未得到可核对结果；元信息仍隐藏，不自动重试。";
    }
    pub fn accept(&mut self, id: &str, b: &Binding, r: &Receipt) -> bool {
        if !self.state.accept(id, b, r) {
            return false;
        }
        self.verified = self.expected_uid.is_some_and(|uid| uid == r.owner_uid)
            && matches!(
                r.status.as_str(),
                "uploading" | "committed" | "cancelled" | "expired" | "rejected"
            );
        if self.verified {
            if let Some(info) = r.attachment.as_ref() {
                self.state.upload.as_mut().unwrap().info = info.clone();
            }
            self.notice = "原账户UID、目标及附件校验已由本次服务器回执确认。";
        } else {
            self.notice = match r.status.as_str() {
                "committed" => {
                    "服务器对此原key回报已发布，但旧账户UID未知，无法解封元信息；不会重新发送。"
                }
                "cancelled" | "expired" | "rejected" => {
                    "服务器对此原key回报终态，但旧账户UID未核对；元信息继续隐藏。"
                }
                _ => {
                    "尚未取得能验证原账户的服务器回执；元信息仍隐藏，只可查询原操作或清除本地记录。"
                }
            };
        }
        true
    }
    pub fn take_verified_upload(&mut self) -> Option<Upload> {
        if self.verified {
            self.state.upload.take()
        } else {
            None
        }
    }
}

pub fn valid_intent_key(s: &str) -> bool {
    (8..=64).contains(&s.len())
        && s.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}
pub fn valid_upload_id(s: &str) -> bool {
    s.len() == 32
        && s.bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}
pub fn mime_for_filename(name: &str) -> Option<&'static str> {
    match name.rsplit('.').next()?.to_ascii_lowercase().as_str() {
        "png" => Some("image/png"),
        "jpg" | "jpeg" => Some("image/jpeg"),
        "gif" => Some("image/gif"),
        "txt" => Some("text/plain"),
        "md" => Some("text/markdown"),
        "csv" => Some("text/csv"),
        "json" => Some("application/json"),
        "pdf" => Some("application/pdf"),
        _ => None,
    }
}
fn filename_valid(s: &str) -> bool {
    if s.is_empty() || s.len() > 180 || s.chars().count() > 100 || s.trim() != s || s.ends_with('.') || s.chars().any(|c| c.is_control() || "\\/:<>\"|?*".contains(c) || matches!(c, '\u{200b}'..='\u{200f}' | '\u{2028}'..='\u{202e}' | '\u{2060}'..='\u{206f}' | '\u{feff}')) { return false; }
    let stem = s.split('.').next().unwrap_or_default().to_ascii_uppercase();
    !matches!(stem.as_str(), "CON" | "PRN" | "AUX" | "NUL")
        && !(stem.len() == 4
            && (stem.starts_with("COM") || stem.starts_with("LPT"))
            && stem.as_bytes()[3].is_ascii_digit())
}

/// Structural preflight BEFORE handing bytes to GPUI's decoder. Animated GIF
/// counts full canvases; malformed/trailing data and excessive frames fail shut.
pub fn image_budget(mime: &str, b: &[u8]) -> Result<(u32, u32, u32), &'static str> {
    let invalid = "图像无效或超过安全解码预算";
    let dims = match mime {
        "image/png" => {
            if b.len() < 33
                || &b[..8] != b"\x89PNG\r\n\x1a\n"
                || &b[12..16] != b"IHDR"
                || be32(&b[8..12]) != 13
            {
                return Err(invalid);
            }
            let w = be32(&b[16..20]);
            let h = be32(&b[20..24]);
            // APNG must not acquire an unbounded animation path in future GPUI.
            let mut i = 8usize;
            while i < b.len() {
                if b.len() - i < 12 {
                    return Err(invalid);
                }
                let n = be32(&b[i..i + 4]) as usize;
                if n > b.len() - i - 12 {
                    return Err(invalid);
                }
                if &b[i + 4..i + 8] == b"acTL" {
                    return Err("暂不支持 APNG，请选择 PNG/JPEG/GIF");
                }
                i += n + 12;
            }
            (w, h, 1)
        }
        "image/jpeg" => {
            if b.len() < 4 || b[..2] != [0xff, 0xd8] {
                return Err(invalid);
            }
            let mut i = 2;
            let mut found = None;
            while i < b.len() {
                if b[i] != 0xff {
                    return Err(invalid);
                }
                while i < b.len() && b[i] == 0xff {
                    i += 1;
                }
                if i >= b.len() {
                    return Err(invalid);
                }
                let tag = b[i];
                i += 1;
                if tag == 0xda || tag == 0xd9 {
                    break;
                }
                if tag == 0x01 || (0xd0..=0xd7).contains(&tag) {
                    continue;
                }
                if b.len() - i < 2 {
                    return Err(invalid);
                }
                let n = u16::from_be_bytes([b[i], b[i + 1]]) as usize;
                if n < 2 || n > b.len() - i {
                    return Err(invalid);
                }
                if matches!(
                    tag,
                    0xc0 | 0xc1
                        | 0xc2
                        | 0xc3
                        | 0xc5
                        | 0xc6
                        | 0xc7
                        | 0xc9
                        | 0xca
                        | 0xcb
                        | 0xcd
                        | 0xce
                        | 0xcf
                ) {
                    if n < 8 {
                        return Err(invalid);
                    }
                    let d = (
                        u16::from_be_bytes([b[i + 5], b[i + 6]]) as u32,
                        u16::from_be_bytes([b[i + 3], b[i + 4]]) as u32,
                        1,
                    );
                    if found.is_some_and(|old| old != d) {
                        return Err(invalid);
                    }
                    found = Some(d);
                }
                i += n;
            }
            found.ok_or(invalid)?
        }
        "image/gif" => return gif_budget(b),
        _ => return Ok((0, 0, 0)),
    };
    if dimensions_ok(dims.0, dims.1) {
        Ok(dims)
    } else {
        Err(invalid)
    }
}
fn dimensions_ok(w: u32, h: u32) -> bool {
    w > 0
        && h > 0
        && w <= MAX_DIMENSION
        && h <= MAX_DIMENSION
        && w as u64 * h as u64 <= PIXEL_BUDGET
}
fn be32(b: &[u8]) -> u32 {
    u32::from_be_bytes([b[0], b[1], b[2], b[3]])
}
fn gif_budget(b: &[u8]) -> Result<(u32, u32, u32), &'static str> {
    let bad = "GIF 无效或超过帧数/像素预算";
    if b.len() < 14 || (&b[..6] != b"GIF87a" && &b[..6] != b"GIF89a") {
        return Err(bad);
    }
    let le = |i: usize| u16::from_le_bytes([b[i], b[i + 1]]) as u32;
    let (w, h) = (le(6), le(8));
    if !dimensions_ok(w, h) {
        return Err(bad);
    }
    let mut i = 13;
    let mut frames = 0;
    if b[10] & 0x80 != 0 {
        i += 3 * (1usize << ((b[10] & 7) + 1));
    }
    fn blocks(b: &[u8], i: &mut usize) -> bool {
        while *i < b.len() {
            let n = b[*i] as usize;
            *i += 1;
            if n == 0 {
                return true;
            }
            if n > b.len() - *i {
                return false;
            }
            *i += n;
        }
        false
    }
    while i < b.len() {
        let tag = b[i];
        i += 1;
        match tag {
            0x3b => {
                return if i == b.len() && frames > 0 {
                    Ok((w, h, frames))
                } else {
                    Err(bad)
                };
            }
            0x21 => {
                if i >= b.len() {
                    return Err(bad);
                }
                i += 1;
                if !blocks(b, &mut i) {
                    return Err(bad);
                }
            }
            0x2c => {
                if b.len() - i < 9 {
                    return Err(bad);
                }
                let (x, y, fw, fh, packed) = (le(i), le(i + 2), le(i + 4), le(i + 6), b[i + 8]);
                i += 9;
                frames += 1;
                if !dimensions_ok(fw, fh)
                    || x + fw > w
                    || y + fh > h
                    || frames > MAX_FRAMES
                    || w as u64 * h as u64 * frames as u64 > PIXEL_BUDGET
                {
                    return Err(bad);
                }
                if packed & 0x80 != 0 {
                    i += 3 * (1usize << ((packed & 7) + 1));
                }
                if i >= b.len() || !(2..=8).contains(&b[i]) {
                    return Err(bad);
                }
                i += 1;
                if !blocks(b, &mut i) {
                    return Err(bad);
                }
            }
            _ => return Err(bad),
        }
    }
    Err(bad)
}

pub fn encode_base64(bytes: &[u8]) -> String {
    const A: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity(bytes.len().div_ceil(3) * 4);
    for c in bytes.chunks(3) {
        let a = c[0];
        let b = *c.get(1).unwrap_or(&0);
        let d = *c.get(2).unwrap_or(&0);
        out.push(A[(a >> 2) as usize] as char);
        out.push(A[(((a & 3) << 4) | (b >> 4)) as usize] as char);
        out.push(if c.len() > 1 {
            A[(((b & 15) << 2) | (d >> 6)) as usize] as char
        } else {
            '='
        });
        out.push(if c.len() > 2 {
            A[(d & 63) as usize] as char
        } else {
            '='
        });
    }
    out
}
pub fn decode_base64(s: &str, limit: usize) -> Result<Vec<u8>, &'static str> {
    let bad = "附件块编码无效";
    if s.len() > limit.div_ceil(3) * 4 || !s.len().is_multiple_of(4) {
        return Err(bad);
    }
    fn v(b: u8) -> Option<u8> {
        match b {
            b'A'..=b'Z' => Some(b - b'A'),
            b'a'..=b'z' => Some(b - b'a' + 26),
            b'0'..=b'9' => Some(b - b'0' + 52),
            b'+' => Some(62),
            b'/' => Some(63),
            _ => None,
        }
    }
    let mut out = Vec::with_capacity((s.len() / 4 * 3).min(limit));
    for (i, c) in s.as_bytes().chunks(4).enumerate() {
        let a = v(c[0]).ok_or(bad)?;
        let b = v(c[1]).ok_or(bad)?;
        let last = (i + 1) * 4 == s.len();
        let pads = if c[2] == b'=' {
            2
        } else if c[3] == b'=' {
            1
        } else {
            0
        };
        if pads > 0 && !last {
            return Err(bad);
        }
        if pads == 2 && (c[3] != b'=' || b & 15 != 0) {
            return Err(bad);
        }
        out.push((a << 2) | (b >> 4));
        if pads < 2 {
            let d = v(c[2]).ok_or(bad)?;
            if pads == 1 && d & 3 != 0 {
                return Err(bad);
            }
            out.push((b << 4) | (d >> 2));
            if pads == 0 {
                out.push((d << 6) | v(c[3]).ok_or(bad)?);
            }
        }
        if out.len() > limit {
            return Err(bad);
        }
    }
    Ok(out)
}
/// FIPS 180-4 SHA-256, isolated here because no dependency/lock changes are
/// permitted. Known-answer tests below include block-boundary and million-a.
pub fn sha256_hex(bytes: &[u8]) -> String {
    const K: [u32; 64] = [
        0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4,
        0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe,
        0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f,
        0x4a7484aa, 0x5cb0a9dc, 0x76f988da, 0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7,
        0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc,
        0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b,
        0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070, 0x19a4c116,
        0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
        0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7,
        0xc67178f2,
    ];
    let mut h = [
        0x6a09e667u32,
        0xbb67ae85,
        0x3c6ef372,
        0xa54ff53a,
        0x510e527f,
        0x9b05688c,
        0x1f83d9ab,
        0x5be0cd19,
    ];
    let bit_len = (bytes.len() as u64).wrapping_mul(8);
    let blocks = (bytes.len() + 9).div_ceil(64);
    for block in 0..blocks {
        let mut buf = [0u8; 64];
        for (j, b) in buf.iter_mut().enumerate() {
            let i = block * 64 + j;
            *b = if i < bytes.len() {
                bytes[i]
            } else if i == bytes.len() {
                0x80
            } else {
                0
            };
        }
        if block + 1 == blocks {
            buf[56..].copy_from_slice(&bit_len.to_be_bytes());
        }
        let mut w = [0u32; 64];
        for (i, c) in buf.as_chunks::<4>().0.iter().enumerate() {
            w[i] = be32(c);
        }
        for i in 16..64 {
            let s0 = w[i - 15].rotate_right(7) ^ w[i - 15].rotate_right(18) ^ (w[i - 15] >> 3);
            let s1 = w[i - 2].rotate_right(17) ^ w[i - 2].rotate_right(19) ^ (w[i - 2] >> 10);
            w[i] = w[i - 16]
                .wrapping_add(s0)
                .wrapping_add(w[i - 7])
                .wrapping_add(s1);
        }
        let [mut a, mut b, mut c, mut d, mut e, mut f, mut g, mut z] = h;
        for i in 0..64 {
            let s1 = e.rotate_right(6) ^ e.rotate_right(11) ^ e.rotate_right(25);
            let ch = (e & f) ^ (!e & g);
            let t1 = z
                .wrapping_add(s1)
                .wrapping_add(ch)
                .wrapping_add(K[i])
                .wrapping_add(w[i]);
            let s0 = a.rotate_right(2) ^ a.rotate_right(13) ^ a.rotate_right(22);
            let maj = (a & b) ^ (a & c) ^ (b & c);
            let t2 = s0.wrapping_add(maj);
            z = g;
            g = f;
            f = e;
            e = d.wrapping_add(t1);
            d = c;
            c = b;
            b = a;
            a = t1.wrapping_add(t2);
        }
        for (v, n) in h.iter_mut().zip([a, b, c, d, e, f, g, z]) {
            *v = v.wrapping_add(n);
        }
    }
    h.iter().map(|v| format!("{v:08x}")).collect()
}

/// Explicitly evictable encoded-byte cache: at most 20 MB total, at most eight
/// objects; keys include authority+room+generation+message. It is NOT ACL proof:
/// a fresh server fetch must precede every display/download, even on a cache hit.
#[derive(Default)]
pub struct ByteCache {
    entries: VecDeque<(Binding, i64, Info, Vec<u8>)>,
    total: usize,
}
impl ByteCache {
    pub fn clear(&mut self) {
        self.entries.clear();
        self.total = 0;
    }
    pub fn evict(&mut self, binding: &Binding, id: i64) {
        if let Some(i) = self
            .entries
            .iter()
            .position(|e| e.0 == *binding && e.1 == id)
            && let Some(e) = self.entries.remove(i)
        {
            self.total -= e.3.len();
        }
    }
    pub fn insert_verified(
        &mut self,
        binding: Binding,
        id: i64,
        info: Info,
        bytes: Vec<u8>,
    ) -> bool {
        if id <= 0
            || bytes.len() > ITEM_LIMIT
            || bytes.len() as u64 != info.size
            || sha256_hex(&bytes) != info.sha256
        {
            return false;
        }
        self.evict(&binding, id);
        while self.total + bytes.len() > ITEM_LIMIT || self.entries.len() >= 8 {
            if let Some(e) = self.entries.pop_front() {
                self.total -= e.3.len();
            } else {
                break;
            }
        }
        self.total += bytes.len();
        self.entries.push_back((binding, id, info, bytes));
        true
    }
    pub fn bytes_after_fresh_acl(&self, binding: &Binding, id: i64, info: &Info) -> Option<&[u8]> {
        self.entries
            .iter()
            .find(|e| e.0 == *binding && e.1 == id && e.2 == *info)
            .map(|e| e.3.as_slice())
    }
    pub fn len_bytes(&self) -> usize {
        self.total
    }
}

/// One sequential, bounded fetch. Only a message ID can start this operation;
/// every chunk rechecks ACL server-side. No arbitrary URL/path is accepted.
pub struct Download {
    pub binding: Binding,
    pub message_id: i64,
    pub info: Info,
    bytes: Vec<u8>,
    pending: Option<String>,
    pub complete: bool,
}
impl Download {
    pub fn pending_matches(&self, id: &str) -> bool {
        self.pending.as_deref() == Some(id)
    }
    pub fn new(binding: Binding, message_id: i64, info: Info) -> Result<Self, &'static str> {
        if message_id <= 0
            || !valid_upload_id(&info.id)
            || info.size == 0
            || info.size > ITEM_LIMIT as u64
        {
            return Err("附件元数据无效");
        }
        Ok(Self {
            binding,
            message_id,
            info,
            bytes: Vec::new(),
            pending: None,
            complete: false,
        })
    }
    pub fn begin(&mut self, id: String, current: &Binding) -> Option<Data> {
        if self.binding != *current || self.pending.is_some() || self.complete {
            return None;
        }
        self.pending = Some(id);
        let mut d = Data::operation(Operation::Fetch);
        d.message_id = self.message_id;
        d.offset = self.bytes.len() as u64;
        Some(d)
    }
    pub fn accept(
        &mut self,
        id: &str,
        current: &Binding,
        r: &Receipt,
    ) -> Result<bool, &'static str> {
        if self.binding != *current || self.pending.as_deref() != Some(id) {
            return Ok(false);
        }
        let bad = "下载响应不匹配；没有显示或保存文件";
        if r.op != Some(Operation::Fetch)
            || r.status != "fetched"
            || r.room != self.binding.room
            || r.message_id != self.message_id
            || r.offset != self.bytes.len() as u64
            || r.attachment.as_ref() != Some(&self.info)
        {
            self.bytes.clear();
            self.pending = None;
            return Err(bad);
        }
        let data = decode_base64(&r.data_b64, CHUNK_BYTES)?;
        let size = self.bytes.len() + data.len();
        if data.is_empty()
            || size > self.info.size as usize
            || r.more != (size < self.info.size as usize)
            || (r.more && data.len() != CHUNK_BYTES)
        {
            self.bytes.clear();
            self.pending = None;
            return Err(bad);
        }
        self.bytes.extend_from_slice(&data);
        self.pending = None;
        if !r.more {
            if sha256_hex(&self.bytes) != self.info.sha256 {
                self.bytes.clear();
                return Err("附件摘要不匹配");
            }
            image_budget(&self.info.mime, &self.bytes)?;
            self.complete = true;
        }
        Ok(true)
    }
    pub fn take_bytes(&mut self) -> Option<Vec<u8>> {
        if !self.complete {
            return None;
        }
        self.complete = false;
        Some(std::mem::take(&mut self.bytes))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    fn binding() -> Binding {
        Binding::new(
            Origin::synthetic("test", 1),
            "owner".into(),
            "lobby".into(),
            7,
        )
        .unwrap()
    }
    fn staged(n: usize) -> (Attachments, Binding) {
        let b = binding();
        let mut a = Attachments {
            supported: true,
            ..Default::default()
        };
        let s = a.begin_selection(b.clone(), Purpose::Attachment).unwrap();
        a.finish_selection(
            &s,
            &b,
            Prepared::from_bytes("a.txt".into(), vec![b'a'; n]).unwrap(),
            "intent_test_123".into(),
        )
        .unwrap();
        (a, b)
    }
    fn receipt(a: &Attachments, op: Operation, status: &str, offset: u64) -> Receipt {
        let u = a.upload.as_ref().unwrap();
        let mut info = u.info.clone();
        info.id = "1234567890abcdef1234567890abcdef".into();
        Receipt {
            op: Some(op),
            status: status.into(),
            intent_key: u.intent_key.clone(),
            upload_id: info.id.clone(),
            room: u.binding.room.clone(),
            owner_uid: 42,
            accepted_offset: offset,
            size: info.size,
            attachment: Some(info),
            ..Default::default()
        }
    }
    #[test]
    fn sha_known_answers() {
        assert_eq!(
            sha256_hex(b""),
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
        assert_eq!(
            sha256_hex(b"abc"),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
        assert_eq!(
            sha256_hex(b"abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq"),
            "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1"
        );
        assert_eq!(
            sha256_hex(&vec![b'a'; 1_000_000]),
            "cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0"
        );
    }
    #[test]
    fn base64_vectors_and_canonical() {
        for (raw, b64) in [
            ("", ""),
            ("f", "Zg=="),
            ("fo", "Zm8="),
            ("foo", "Zm9v"),
            ("foob", "Zm9vYg=="),
            ("foobar", "Zm9vYmFy"),
        ] {
            assert_eq!(encode_base64(raw.as_bytes()), b64);
            assert_eq!(decode_base64(b64, 20).unwrap(), raw.as_bytes());
        }
        for s in ["Zg=", "Zh==", "Zm9=", "Zg==AAAA", "Zg==\n", "!!!!"] {
            assert!(decode_base64(s, 20).is_err());
        }
        assert!(decode_base64("Zm9v", 2).is_err());
    }
    #[test]
    fn exact_ack_and_two_user_confirmations() {
        let (mut a, b) = staged(CHUNK_BYTES + 1);
        assert!(a.next_operation(&b).is_none());
        let d = a.begin(Operation::Intent, "i".into(), &b).unwrap();
        assert_eq!(d.size, (CHUNK_BYTES + 1) as u64);
        assert!(a.begin(Operation::Intent, "i2".into(), &b).is_err());
        let r = receipt(&a, Operation::Intent, "uploading", 0);
        assert!(a.accept("i", &b, &r));
        let d = a.begin(Operation::Chunk, "c1".into(), &b).unwrap();
        assert_eq!(
            decode_base64(&d.data_b64, CHUNK_BYTES).unwrap().len(),
            CHUNK_BYTES
        );
        let mut r = receipt(&a, Operation::Chunk, "uploading", CHUNK_BYTES as u64);
        r.accepted_offset = 1;
        assert!(!a.accept("c1", &b, &r));
        r.accepted_offset = CHUNK_BYTES as u64;
        assert!(a.accept("c1", &b, &r));
        assert!(!a.accept("c1", &b, &r));
        let d = a.begin(Operation::Chunk, "c2".into(), &b).unwrap();
        assert_eq!(d.offset, CHUNK_BYTES as u64);
        assert_eq!(decode_base64(&d.data_b64, CHUNK_BYTES).unwrap(), vec![b'a']);
        let r = receipt(&a, Operation::Chunk, "uploading", (CHUNK_BYTES + 1) as u64);
        assert!(a.accept("c2", &b, &r));
        assert_eq!(a.upload.as_ref().unwrap().phase, Phase::ReadyToCommit);
        assert!(a.next_operation(&b).is_none());
        a.begin(Operation::Commit, "commit".into(), &b).unwrap();
        let mut r = receipt(&a, Operation::Commit, "committed", (CHUNK_BYTES + 1) as u64);
        r.message_id = 21;
        assert!(a.accept("commit", &b, &r));
        assert!(a.upload.as_ref().unwrap().source().is_none());
        assert!(a.begin(Operation::Cancel, "cancel".into(), &b).is_err());
    }
    #[test]
    fn unknown_never_new_intent_or_automatic_retry() {
        let (mut a, b) = staged(3);
        a.begin(Operation::Intent, "i".into(), &b).unwrap();
        a.transport_uncertain("i");
        assert_eq!(a.upload.as_ref().unwrap().phase, Phase::Unknown);
        assert!(a.begin_selection(b.clone(), Purpose::Attachment).is_err());
        assert!(a.begin(Operation::Intent, "retry".into(), &b).is_err());
        let d = a.begin(Operation::Status, "s".into(), &b).unwrap();
        assert_eq!(d.intent_key, "intent_test_123");
        assert!(d.upload_id.is_empty());
        let r = Receipt {
            op: Some(Operation::Status),
            status: "unknown".into(),
            code: "not_found".into(),
            intent_key: d.intent_key,
            room: b.room.clone(),
            ..Default::default()
        };
        assert!(a.accept("s", &b, &r));
        assert!(a.next_operation(&b).is_none());
    }
    #[test]
    fn lifecycle_selection_and_reconnect_are_bound() {
        let (mut a, b) = staged(1);
        a.discard_preview();
        let s = a.begin_selection(b.clone(), Purpose::Attachment).unwrap();
        a.cancel_selection();
        assert!(!a.selection_matches(&s, &b));
        let (mut a, b) = staged(3);
        a.begin(Operation::Intent, "i".into(), &b).unwrap();
        let r = receipt(&a, Operation::Intent, "uploading", 0);
        assert!(a.accept("i", &b, &r));
        a.invalidate(false);
        assert!(a.upload.as_ref().unwrap().source().is_none());
        let mut next = b.clone();
        next.generation += 1;
        assert!(a.begin(Operation::Chunk, "c".into(), &next).is_err());
        assert!(a.begin(Operation::Status, "s".into(), &next).is_ok());
        let mut wrong = next.clone();
        wrong.origin = Origin::synthetic("other", 1);
        assert!(!a.pending_matches("s", &wrong));
        wrong = next.clone();
        wrong.owner = "other".into();
        assert!(!a.pending_matches("s", &wrong));
        wrong = next;
        wrong.room = "other".into();
        assert!(!a.pending_matches("s", &wrong));
        a.invalidate(true);
        assert!(a.upload.is_none());
        assert!(a.notice.is_some());
    }
    #[test]
    fn stale_chooser_cancel_cannot_cancel_new_selection() {
        let b = binding();
        let mut a = Attachments {
            supported: true,
            ..Default::default()
        };
        let old = a.begin_selection(b.clone(), Purpose::Attachment).unwrap();
        a.cancel_selection();
        let new = a.begin_selection(b.clone(), Purpose::Attachment).unwrap();
        assert!(!a.cancel_selection_if_current(&old, &b));
        assert!(a.selection_matches(&new, &b));
        assert!(a.cancel_selection_if_current(&new, &b));
        assert!(!a.selection_matches(&new, &b));
    }
    #[test]
    fn safe_files_and_bounded_read() {
        let temp = tempfile::tempdir().unwrap();
        let path = temp.path().join("safe.txt");
        std::fs::write(&path, b"source").unwrap();
        let p = read_selected_file(&path).unwrap();
        assert_eq!(p.bytes(), b"source");
        std::fs::write(&path, b"changed").unwrap();
        assert_eq!(p.bytes(), b"source");
        let big = temp.path().join("big.txt");
        File::create(&big)
            .unwrap()
            .set_len(ITEM_LIMIT as u64 + 1)
            .unwrap();
        assert!(read_selected_file(&big).is_err());
        assert!(read_selected_file(temp.path()).is_err());
        #[cfg(unix)]
        {
            let link = temp.path().join("link.txt");
            std::os::unix::fs::symlink(&path, &link).unwrap();
            assert!(read_selected_file(&link).is_err());
        }
    }
    #[test]
    fn malformed_image_headers_are_not_decoded() {
        assert!(Prepared::from_bytes("bad.png".into(), vec![0; 33]).is_err());
        assert!(image_budget("image/gif", b"GIF89a").is_err());
        let mut gif=b"GIF89a\x01\x00\x01\x00\x00\x00\x00\x2c\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x01\x00\x00\x3b".to_vec();
        assert_eq!(image_budget("image/gif", &gif).unwrap(), (1, 1, 1));
        gif.push(0);
        assert!(image_budget("image/gif", &gif).is_err());
    }
    #[test]
    fn wire_chunks_fit_line_budget() {
        let (mut a, b) = staged(CHUNK_BYTES);
        a.begin(Operation::Intent, "i".into(), &b).unwrap();
        let r = receipt(&a, Operation::Intent, "uploading", 0);
        a.accept("i", &b, &r);
        let d = a.begin(Operation::Chunk, "c".into(), &b).unwrap();
        let j = serde_json::to_vec(&d).unwrap();
        assert!(j.len() + 4096 < LINE_LIMIT);
        assert!(!String::from_utf8(j).unwrap().contains("filename"));
    }
    #[test]
    fn captured_pointer_cannot_confirm_or_discard_replacement_upload() {
        let (mut a, b) = staged(3);
        let ticket = a
            .action_ticket(LocalAction::Operation(Operation::Intent), &b)
            .unwrap();
        let discard = a.action_ticket(LocalAction::Discard, &b).unwrap();
        assert!(a.admits_action(&ticket, &b));
        a.discard_preview();
        let sel = a.begin_selection(b.clone(), Purpose::Attachment).unwrap();
        a.finish_selection(
            &sel,
            &b,
            Prepared::from_bytes("b.txt".into(), b"new".to_vec()).unwrap(),
            "replacement_intent".into(),
        )
        .unwrap();
        assert!(!a.admits_action(&ticket, &b));
        assert!(!a.admits_action(&discard, &b));
        let new = a
            .action_ticket(LocalAction::Operation(Operation::Intent), &b)
            .unwrap();
        let mut wrong = b.clone();
        wrong.generation += 1;
        assert!(!a.admits_action(&new, &wrong));
        a.begin(Operation::Intent, "send".into(), &b).unwrap();
        a.transport_uncertain("send");
        assert!(!a.admits_action(&new, &b));
        let forget = a.action_ticket(LocalAction::ForgetUnknown, &b).unwrap();
        assert!(a.admits_action(&forget, &b));
        assert!(a.forget_unknown_local());
        assert!(a.upload.is_none());
        assert!(a.notice.as_ref().unwrap().contains("不代表取消或回滚"));
    }
    #[test]
    fn sealed_recovery_requires_fresh_pinned_uid_and_keeps_old_generation() {
        let (mut a, b) = staged(3);
        a.begin(Operation::Intent, "intent".into(), &b).unwrap();
        let r = receipt(&a, Operation::Intent, "uploading", 0);
        assert!(a.accept("intent", &b, &r));
        let mut recovery = a.seal_transport_recovery().unwrap();
        assert!(a.upload.is_none());
        assert!(recovery.upload().source().is_none());
        let mut current = b.clone();
        current.generation += 1;
        let ticket = recovery.ticket(RecoveryAction::Status, &current).unwrap();
        recovery
            .begin_status(&ticket, "status".into(), &current)
            .unwrap();
        let mut r = receipt(&recovery.state, Operation::Status, "uploading", 0);
        r.owner_uid += 1;
        assert!(!recovery.accept("status", &current, &r));
        r.owner_uid -= 1;
        assert!(recovery.accept("status", &current, &r));
        let u = recovery.take_verified_upload().unwrap();
        assert_eq!(u.binding.generation, b.generation);
        assert!(u.source().is_none());
        assert_eq!(u.phase, Phase::Paused);
    }
    #[test]
    fn lost_initial_ack_can_query_but_never_unlock_metadata_or_replay() {
        for credential_change in [false, true] {
            let (mut pending, binding) = staged(3);
            pending
                .begin(Operation::Intent, "pending".into(), &binding)
                .unwrap();
            pending.invalidate(credential_change);
            if credential_change {
                assert!(pending.upload.is_none());
                assert!(pending.notice.as_ref().unwrap().contains("未知"));
            } else {
                let upload = pending.upload.as_ref().unwrap();
                assert_eq!(upload.phase, Phase::Unknown);
                assert!(upload.source().is_none());
                assert_eq!(upload.intent_key, "intent_test_123");
            }
        }
        let (mut a, b) = staged(3);
        a.begin(Operation::Intent, "intent".into(), &b).unwrap();
        let mut recovery = a.seal_transport_recovery().unwrap();
        let mut current = b.clone();
        current.generation += 1;
        let t = recovery.ticket(RecoveryAction::Status, &current).unwrap();
        let d = recovery
            .begin_status(&t, "status".into(), &current)
            .unwrap();
        assert_eq!(d.intent_key, "intent_test_123");
        let r = receipt(&recovery.state, Operation::Status, "uploading", 0);
        assert!(recovery.accept("status", &current, &r));
        assert!(recovery.take_verified_upload().is_none());
        assert!(recovery.notice().contains("隐藏"));
        let mut wrong = current.clone();
        wrong.owner = "replacement".into();
        assert!(recovery.ticket(RecoveryAction::Status, &wrong).is_none());
        wrong = current;
        wrong.origin = Origin::synthetic("other", 2);
        assert!(recovery.ticket(RecoveryAction::Status, &wrong).is_none());
    }
    #[test]
    fn cache_bound_and_explicit_eviction() {
        let mut cache = ByteCache::default();
        let p = Prepared::from_bytes("a.txt".into(), vec![b'a'; 3]).unwrap();
        assert!(cache.insert_verified(binding(), 1, p.info.clone(), p.bytes().to_vec()));
        assert_eq!(cache.len_bytes(), 3);
        assert_eq!(
            cache.bytes_after_fresh_acl(&binding(), 1, &p.info).unwrap(),
            b"aaa"
        );
        cache.clear();
        assert_eq!(cache.len_bytes(), 0);
    }
    #[test]
    fn fetch_exact_identity_offset_digest() {
        let b = binding();
        let p = Prepared::from_bytes("a.txt".into(), b"abc".to_vec()).unwrap();
        let mut info = p.info.clone();
        info.id = "1234567890abcdef1234567890abcdef".into();
        let mut d = Download::new(b.clone(), 4, info.clone()).unwrap();
        assert!(d.begin("f".into(), &b).is_some());
        let r = Receipt {
            op: Some(Operation::Fetch),
            status: "fetched".into(),
            room: b.room.clone(),
            message_id: 4,
            attachment: Some(info),
            data_b64: "YWJj".into(),
            ..Default::default()
        };
        assert!(d.accept("f", &b, &r).unwrap());
        assert_eq!(d.take_bytes().unwrap(), b"abc");
    }
}
