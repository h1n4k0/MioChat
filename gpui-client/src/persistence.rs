//! Pure state machine for draft/reading-position persistence. No sockets,
//! windows, credentials or wall-clock access; the UI supplies monotonic millis.
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
pub struct Position {
    pub last_read: i64,
    pub anchor: i64,
    /// Zero follows the latest page. Positive values are 1-based line offsets
    /// within the anchored message, matching the TUI's stored representation.
    pub offset: i32,
}
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Snapshot {
    pub body: String,
    pub reply_to: i64,
    pub position: Position,
}
impl Snapshot {
    fn same_text(&self, other: &Self) -> bool {
        self.body == other.body && self.reply_to == other.reply_to
    }
    fn has_text(&self) -> bool {
        !self.body.is_empty() || self.reply_to != 0
    }
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Operation {
    Load {
        room: String,
        manual: bool,
    },
    Save {
        room: String,
        version: i64,
        snapshot: Snapshot,
        keep: bool,
        backup_id: String,
    },
    Suspend {
        room: String,
        snapshot: Snapshot,
    },
}
impl Operation {
    pub fn safe_body(&self) -> bool {
        match self {
            Self::Load { .. } => true,
            Self::Save { snapshot, .. } | Self::Suspend { snapshot, .. } => {
                !crate::word::reserved_start(&snapshot.body)
            }
        }
    }

    pub fn room(&self) -> &str {
        match self {
            Self::Load { room, .. } | Self::Save { room, .. } | Self::Suspend { room, .. } => room,
        }
    }
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Ack {
    Loaded,
    Saved,
    Conflict,
    Suspended,
}
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Update {
    pub apply: Option<Snapshot>,
    pub restore_position: bool,
    pub conflict: bool,
    pub ignored: bool,
}
#[derive(Clone, Default)]
struct RoomState {
    quarantined: bool,
    local: Snapshot,
    baseline: Option<(i64, Snapshot)>,
    loaded: bool,
    conflict: bool,
    backup_id: String,
    needs_backup: bool,
    changed_at: u64,
    retry_at: u64,
    input_backup: Option<Snapshot>,
}
impl RoomState {
    fn text_dirty(&self) -> bool {
        self.baseline
            .as_ref()
            .map_or(self.local.has_text(), |(_, saved)| {
                !self.local.same_text(saved)
            })
    }
    fn dirty(&self) -> bool {
        self.baseline
            .as_ref()
            .is_some_and(|(_, saved)| self.local != *saved)
    }
}
struct Pending {
    id: String,
    op: Operation,
    at: u64,
    before: Snapshot,
}
#[derive(Default)]
pub struct Persistence {
    owner: String,
    active: String,
    rooms: BTreeMap<String, RoomState>,
    pending: Option<Pending>,
}
impl Persistence {
    pub fn activate(&mut self, owner: &str, room: &str, now: u64) -> Snapshot {
        if self.owner != owner {
            self.owner = owner.into();
            self.active.clear();
            self.rooms.clear();
            self.pending = None;
        }
        if self.active != room {
            if let Some(old) = self.rooms.get_mut(&self.active) {
                old.needs_backup |= old.text_dirty();
                old.loaded = false;
            }
            self.active = room.into();
            let state = self.rooms.entry(room.into()).or_default();
            state.loaded = false;
            state.changed_at = now;
        }
        self.local().cloned().unwrap_or_default()
    }
    pub fn quarantined(&self) -> bool {
        self.rooms.get(&self.active).is_some_and(|s| s.quarantined)
    }
    /// Explicit user-confirmed CAS clear only. The caller captures owner/scope/
    /// generation/version before confirmation; no automatic keep or retry.
    pub fn clear_quarantined(&self, version: i64) -> Option<Operation> {
        if self.pending()
            || self.conflicted()
            || !self.quarantined()
            || self.version() != Some(version)
        {
            return None;
        }
        let state = self.rooms.get(&self.active)?;
        Some(Operation::Save {
            room: self.active.clone(),
            version,
            snapshot: Snapshot {
                body: String::new(),
                reply_to: 0,
                position: state.local.position.clone(),
            },
            keep: false,
            backup_id: String::new(),
        })
    }
    pub fn local(&self) -> Option<&Snapshot> {
        self.rooms.get(&self.active).map(|r| &r.local)
    }
    pub fn ready(&self) -> bool {
        self.rooms.get(&self.active).is_some_and(|r| r.loaded)
    }
    pub fn version(&self) -> Option<i64> {
        self.rooms
            .get(&self.active)
            .filter(|r| r.loaded)
            .and_then(|r| r.baseline.as_ref().map(|(v, _)| *v))
    }
    pub fn conflicted(&self) -> bool {
        self.rooms.get(&self.active).is_some_and(|r| r.conflict)
    }
    pub fn backup_id(&self) -> &str {
        self.rooms
            .get(&self.active)
            .map_or("", |r| r.backup_id.as_str())
    }
    pub fn pending(&self) -> bool {
        self.pending.is_some()
    }
    /// Inactive scopes may still need a durable private backup after a forced
    /// ACL move. An active-room save alone is insufficient before closing.
    pub fn has_unconfirmed_backups(&self) -> bool {
        self.rooms.values().any(|room| room.needs_backup)
    }
    pub fn recovery_text(&self) -> String {
        self.rooms
            .iter()
            .filter(|(room, state)| {
                (room.as_str() == self.active || state.needs_backup)
                    && state.local.has_text()
                    && !crate::word::reserved_start(&state.local.body)
            })
            .map(|(room, state)| {
                format!(
                    "[{room}; reply_to={}]\n{}",
                    state.local.reply_to, state.local.body
                )
            })
            .collect::<Vec<_>>()
            .join("\n\n")
    }
    pub fn dirty(&self) -> bool {
        self.rooms.get(&self.active).is_some_and(RoomState::dirty)
    }
    pub fn has_input_backup(&self) -> bool {
        self.rooms
            .get(&self.active)
            .is_some_and(|r| r.input_backup.is_some())
    }
    pub fn observe(&mut self, mut snapshot: Snapshot, now: u64) {
        if crate::word::reserved_start(&snapshot.body) || self.quarantined() {
            return;
        }
        let Some(state) = self.rooms.get_mut(&self.active) else {
            return;
        };
        snapshot.position.last_read = snapshot
            .position
            .last_read
            .max(state.local.position.last_read);
        if snapshot != state.local {
            if state.conflict && !snapshot.same_text(&state.local) {
                state.needs_backup = true;
            }
            state.local = snapshot;
            state.changed_at = now;
        }
    }
    pub fn next(&self, now: u64) -> Option<Operation> {
        if self.pending.is_some() {
            return None;
        }
        for (room, state) in &self.rooms {
            if !state.quarantined
                && state.needs_backup
                && now >= state.retry_at
                && (room != &self.active || now.saturating_sub(state.changed_at) >= 800)
            {
                return Some(Operation::Suspend {
                    room: room.clone(),
                    snapshot: state.local.clone(),
                });
            }
        }
        let state = self.rooms.get(&self.active)?;
        if now < state.retry_at {
            return None;
        }
        if !state.loaded {
            return Some(Operation::Load {
                room: self.active.clone(),
                manual: false,
            });
        }
        if state.quarantined
            || state.conflict
            || !state.dirty()
            || now.saturating_sub(state.changed_at) < 800
        {
            return None;
        }
        Some(Operation::Save {
            room: self.active.clone(),
            version: state.baseline.as_ref()?.0,
            snapshot: state.local.clone(),
            keep: false,
            backup_id: String::new(),
        })
    }
    pub fn manual(&self, load: bool) -> Option<Operation> {
        if self.pending.is_some() {
            return None;
        }
        let state = self.rooms.get(&self.active)?;
        if load {
            return Some(Operation::Load {
                room: self.active.clone(),
                manual: true,
            });
        }
        if state.quarantined {
            return None;
        }
        Some(Operation::Save {
            room: self.active.clone(),
            version: self.version()?,
            snapshot: state.local.clone(),
            keep: state.conflict,
            backup_id: state.backup_id.clone(),
        })
    }
    pub fn begin(&mut self, id: String, op: Operation, now: u64) -> bool {
        if !op.safe_body() || self.pending.is_some() || !self.rooms.contains_key(op.room()) {
            return false;
        }
        let before = self.rooms[op.room()].local.clone();
        self.pending = Some(Pending {
            id,
            op,
            at: now,
            before,
        });
        true
    }
    pub fn acknowledge(
        &mut self,
        key: (&str, &str),
        ack: Ack,
        version: i64,
        remote: Snapshot,
        backup_id: String,
        now: u64,
    ) -> Update {
        let (id, room) = key;
        if self.pending.as_ref().is_none_or(|p| {
            p.id != id
                || p.op.room() != room
                || !matches!(
                    (&p.op, ack),
                    (Operation::Load { .. }, Ack::Loaded)
                        | (Operation::Save { .. }, Ack::Saved | Ack::Conflict)
                        | (Operation::Suspend { .. }, Ack::Suspended)
                )
        }) {
            return Update {
                ignored: true,
                ..Default::default()
            };
        }
        let pending = self.pending.take().unwrap();
        let Some(state) = self.rooms.get_mut(room) else {
            return Update {
                ignored: true,
                ..Default::default()
            };
        };
        let remote_sensitive = crate::word::reserved_start(&remote.body);
        let active = self.active == room;
        let mut update = Update::default();
        match ack {
            Ack::Loaded => {
                let local_before = state.local.clone();
                let unchanged = state.local == pending.before;
                let manual = matches!(pending.op, Operation::Load { manual: true, .. });
                let text_dirty = state.text_dirty();
                let remote_changed = state
                    .baseline
                    .as_ref()
                    .map_or(remote.has_text(), |(_, old)| !remote.same_text(old));
                let metadata_dirty = state
                    .baseline
                    .as_ref()
                    .is_some_and(|(_, old)| state.local.position != old.position);
                if manual && unchanged {
                    if state.local != remote {
                        state.input_backup = Some(state.local.clone());
                    }
                    state.local = remote.clone();
                    state.conflict = false;
                } else if text_dirty && remote_changed && !state.local.same_text(&remote) {
                    state.conflict = true;
                    state.needs_backup = true;
                } else {
                    if !text_dirty {
                        state.local.body = remote.body.clone();
                        state.local.reply_to = remote.reply_to;
                    }
                    if !metadata_dirty {
                        state.local.position = remote.position.clone();
                    }
                    state.conflict = false;
                }
                state.local.position.last_read = state
                    .local
                    .position
                    .last_read
                    .max(remote.position.last_read);
                state.baseline = Some((version, remote));
                state.loaded = true;
                state.retry_at = 0;
                state.changed_at = now;
                update.conflict = state.conflict;
                if active {
                    update.restore_position =
                        state.local.position != local_before.position || !manual;
                    update.apply = Some(state.local.clone());
                }
            }
            Ack::Saved => {
                // Keep edits made after this request was sent. A saved response
                // acknowledges only its snapshot, not whatever is typed now.
                if let Operation::Save { snapshot, keep, .. } = &pending.op
                    && (state.local.same_text(snapshot)
                        || crate::word::reserved_start(&state.local.body)
                            && !keep
                            && snapshot.body.is_empty()
                            && snapshot.reply_to == 0)
                {
                    state.local.body = remote.body.clone();
                    state.local.reply_to = remote.reply_to;
                }
                state.local.position.last_read = state
                    .local
                    .position
                    .last_read
                    .max(remote.position.last_read);
                state.baseline = Some((version, remote));
                state.loaded = true;
                state.conflict = false;
                state.backup_id.clear();
                state.needs_backup = false;
                state.retry_at = 0;
                if active {
                    update.apply = Some(state.local.clone());
                }
            }
            Ack::Conflict => {
                let saved = state
                    .baseline
                    .as_ref()
                    .map(|(_, old)| old.clone())
                    .unwrap_or_default();
                state.baseline = Some((version, saved));
                state.loaded = true;
                state.conflict = true;
                state.backup_id = backup_id;
                state.needs_backup = !state.local.same_text(&pending.before);
                state.changed_at = now;
                update.conflict = true;
            }
            Ack::Suspended => {
                state.backup_id = backup_id;
                if let Operation::Suspend { snapshot, .. } = &pending.op {
                    state.needs_backup = !state.local.same_text(snapshot);
                }
                state.retry_at = 0;
            }
        }
        state.quarantined = remote_sensitive || crate::word::reserved_start(&state.local.body);
        update
    }
    pub fn fail(&mut self, id: &str, now: u64) -> bool {
        if self.pending.as_ref().is_none_or(|p| p.id != id) {
            return false;
        }
        let p = self.pending.take().unwrap();
        if let Some(state) = self.rooms.get_mut(p.op.room()) {
            state.loaded = false;
            state.retry_at = now.saturating_add(3000);
        }
        true
    }
    pub fn expire(&mut self, now: u64) -> bool {
        if let Some(p) = self.pending.as_ref()
            && now.saturating_sub(p.at) >= 10_000
        {
            let id = p.id.clone();
            return self.fail(&id, now);
        }
        false
    }
    pub fn disconnected(&mut self) {
        self.pending = None;
        for state in self.rooms.values_mut() {
            state.loaded = false;
        }
    }
    pub fn restore_input_backup(&mut self, now: u64) -> Option<Snapshot> {
        let state = self.rooms.get_mut(&self.active)?;
        if state
            .input_backup
            .as_ref()
            .is_some_and(|s| crate::word::reserved_start(&s.body))
        {
            return None;
        }
        let backup = state.input_backup.take()?;
        state.input_backup = Some(std::mem::replace(&mut state.local, backup));
        state.changed_at = now;
        Some(state.local.clone())
    }
    pub fn restore_conflict(
        &mut self,
        version: i64,
        snapshot: Snapshot,
        backup_id: String,
        now: u64,
    ) {
        if let Some(state) = self.rooms.get_mut(&self.active) {
            if state.local != snapshot {
                state.input_backup = Some(state.local.clone());
            }
            state.quarantined = crate::word::reserved_start(&snapshot.body);
            state.local = snapshot.clone();
            state.baseline = Some((version, snapshot));
            state.loaded = true;
            state.conflict = true;
            state.backup_id = backup_id;
            state.needs_backup = false;
            state.changed_at = now;
        }
    }
    pub fn invalidate_active(&mut self) {
        if let Some(state) = self.rooms.get_mut(&self.active) {
            state.loaded = false;
            state.baseline = None;
        }
    }
    /// A committed send can clear a matching canonical draft. It does not
    /// acknowledge later typing or changes from another device.
    pub fn sent_baseline(&mut self, room: &str, version: i64, snapshot: Snapshot) {
        if let Some(state) = self.rooms.get_mut(room) {
            state.quarantined |= crate::word::reserved_start(&snapshot.body);
            state.baseline = Some((version, snapshot));
            state.loaded = true;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn wrong_kind_ack_does_not_release_or_replace_pending_load() {
        let mut p = Persistence::default();
        p.activate("alice", "private", 0);
        let op = p.next(0).unwrap();
        p.begin("read".into(), op, 0);
        assert!(
            p.acknowledge(
                ("read", "private"),
                Ack::Saved,
                8,
                text("wrong kind"),
                String::new(),
                1
            )
            .ignored
        );
        assert!(p.pending());
        assert!(!p.ready());
        assert!(p.local().unwrap().body.is_empty());
        assert!(
            !p.acknowledge(
                ("read", "private"),
                Ack::Loaded,
                0,
                Snapshot::default(),
                String::new(),
                2
            )
            .ignored
        );
    }
    #[test]
    fn quick_flush_waits_for_new_typing_and_room_activation_keeps_origin() {
        let mut p = Persistence::default();
        p.activate("alice", "lobby", 0);
        load(&mut p, "load", 0, Snapshot::default(), 0);
        p.observe(text("first"), 1);
        // A close/room switch may request an immediate save without waiting
        // for debounce. New typing still requires its own acknowledged save.
        let op = p.manual(false).unwrap();
        p.begin("flush".into(), op, 2);
        p.observe(text("typed during flush"), 3);
        p.acknowledge(
            ("flush", "lobby"),
            Ack::Saved,
            1,
            text("first"),
            String::new(),
            4,
        );
        assert!(p.dirty());
        assert_eq!(p.local().unwrap().body, "typed during flush");
        let op = p.manual(false).unwrap();
        p.begin("flush2".into(), op, 5);
        p.acknowledge(
            ("flush2", "lobby"),
            Ack::Saved,
            2,
            text("typed during flush"),
            String::new(),
            6,
        );
        assert!(!p.dirty());
        assert!(!p.pending());
        assert!(p.activate("alice", "dev", 7).body.is_empty());
        assert_eq!(p.activate("alice", "lobby", 8).body, "typed during flush");
    }
    fn text(body: &str) -> Snapshot {
        Snapshot {
            body: body.into(),
            ..Default::default()
        }
    }
    fn load(p: &mut Persistence, id: &str, version: i64, remote: Snapshot, now: u64) -> Update {
        let op = p.next(now).expect("load due");
        assert!(matches!(op, Operation::Load { .. }));
        let room = op.room().to_string();
        p.begin(id.into(), op, now);
        p.acknowledge(
            (id, &room),
            Ack::Loaded,
            version,
            remote,
            String::new(),
            now + 1,
        )
    }
    #[test]
    fn baseline_load_precedes_debounced_save_and_inflight_edits_survive() {
        let mut p = Persistence::default();
        p.activate("alice", "lobby", 0);
        assert!(!p.ready());
        load(&mut p, "load", 0, Snapshot::default(), 0);
        p.observe(text("first"), 10);
        assert!(p.next(809).is_none());
        let op = p.next(810).unwrap();
        assert!(matches!(op, Operation::Save { version: 0, .. }));
        p.begin("save".into(), op, 810);
        p.observe(text("later typing"), 820);
        let update = p.acknowledge(
            ("save", "lobby"),
            Ack::Saved,
            1,
            text("first"),
            String::new(),
            830,
        );
        assert_eq!(update.apply.unwrap().body, "later typing");
        assert!(p.next(1619).is_none());
        assert!(
            matches!(p.next(1620),Some(Operation::Save{version:1,snapshot,..}) if snapshot.body=="later typing")
        );
    }
    #[test]
    fn reconnect_detects_remote_edit_and_backs_up_without_overwriting() {
        let mut p = Persistence::default();
        p.activate("alice", "lobby", 0);
        load(&mut p, "load", 3, text("base"), 0);
        p.observe(text("local"), 10);
        p.disconnected();
        let update = load(&mut p, "reload", 4, text("remote"), 20);
        assert!(update.conflict);
        assert_eq!(update.apply.unwrap().body, "local");
        assert!(
            matches!(p.next(821),Some(Operation::Suspend{snapshot,..}) if snapshot.body=="local")
        );
        let op = p.next(821).unwrap();
        p.begin("backup".into(), op, 821);
        p.acknowledge(
            ("backup", "lobby"),
            Ack::Suspended,
            0,
            Snapshot::default(),
            "owned-session".into(),
            822,
        );
        assert!(p.next(2000).is_none());
        assert!(matches!(
            p.manual(false),
            Some(Operation::Save {
                keep: true,
                version: 4,
                ..
            })
        ));
    }
    #[test]
    fn forced_scope_change_keeps_origin_and_rejects_stale_ack() {
        let mut p = Persistence::default();
        p.activate("alice", "private", 0);
        load(&mut p, "load", 0, Snapshot::default(), 0);
        p.observe(text("private unsent"), 5);
        assert_eq!(p.activate("alice", "lobby", 10).body, "");
        let op = p.next(10).unwrap();
        assert!(
            matches!(&op,Operation::Suspend{room,snapshot} if room=="private"&&snapshot.body=="private unsent")
        );
        p.begin("backup".into(), op, 10);
        assert!(p.has_unconfirmed_backups());
        assert!(
            p.recovery_text()
                .contains("[private; reply_to=0]\nprivate unsent")
        );
        assert!(
            p.acknowledge(
                ("stale", "lobby"),
                Ack::Saved,
                9,
                text("wrong"),
                String::new(),
                11
            )
            .ignored
        );
        assert_eq!(p.local().unwrap().body, "");
        p.acknowledge(
            ("backup", "private"),
            Ack::Suspended,
            0,
            Snapshot::default(),
            "owner".into(),
            12,
        );
        assert!(!p.has_unconfirmed_backups());
        assert!(matches!(p.next(13),Some(Operation::Load{room,..}) if room=="lobby"));
    }
    #[test]
    fn failed_read_never_saves_unknown_version_and_owner_change_clears_private_data() {
        let mut p = Persistence::default();
        p.activate("alice", "dm:peer", 0);
        let op = p.next(0).unwrap();
        p.begin("read".into(), op, 0);
        assert!(p.fail("read", 1));
        assert!(p.next(3000).is_none());
        assert!(matches!(p.next(3001), Some(Operation::Load { .. })));
        assert!(p.manual(false).is_none());
        p.activate("bob", "lobby", 4000);
        assert_eq!(p.local().unwrap(), &Snapshot::default());
        assert!(!p.has_input_backup());
        assert!(
            p.acknowledge(
                ("read", "dm:peer"),
                Ack::Loaded,
                8,
                text("alice private"),
                String::new(),
                4001
            )
            .ignored
        );
    }
    #[test]
    fn position_only_update_adopts_remote_text_and_preserves_local_reading_anchor() {
        let mut p = Persistence::default();
        p.activate("alice", "lobby", 0);
        load(&mut p, "read", 1, text("base"), 0);
        let local = Snapshot {
            position: Position {
                last_read: 20,
                anchor: 12,
                offset: 3,
            },
            ..text("base")
        };
        p.observe(local, 10);
        p.disconnected();
        let remote = Snapshot {
            position: Position {
                last_read: 30,
                anchor: 22,
                offset: 1,
            },
            ..text("other device text")
        };
        let update = load(&mut p, "reconnect", 2, remote, 20);
        let applied = update.apply.unwrap();
        assert!(!update.conflict);
        assert_eq!(applied.body, "other device text");
        assert_eq!(applied.position.anchor, 12);
        assert_eq!(applied.position.last_read, 30);
    }
    #[test]
    fn manual_reload_keeps_recoverable_input_and_timeout_rechecks_before_retrying() {
        let mut p = Persistence::default();
        p.activate("alice", "lobby", 0);
        load(&mut p, "read", 0, Snapshot::default(), 0);
        p.observe(text("unsent"), 10);
        let op = p.manual(true).unwrap();
        p.begin("manual".into(), op, 11);
        p.acknowledge(
            ("manual", "lobby"),
            Ack::Loaded,
            1,
            text("remote"),
            String::new(),
            12,
        );
        assert!(p.has_input_backup());
        assert_eq!(p.restore_input_backup(13).unwrap().body, "unsent");
        let op = p.next(813).unwrap();
        p.begin("save".into(), op, 813);
        assert!(p.expire(10813));
        assert!(p.next(13812).is_none());
        assert!(matches!(p.next(13813), Some(Operation::Load { .. })));
    }
}

#[cfg(test)]
mod word_quarantine_tests {
    use super::*;
    fn loaded(p: &mut Persistence, id: &str, version: i64, body: &str) {
        let op = p.manual(true).or_else(|| p.next(0)).unwrap();
        assert!(p.begin(id.into(), op, 0));
        assert!(
            !p.acknowledge(
                (id, "private"),
                Ack::Loaded,
                version,
                Snapshot {
                    body: body.into(),
                    ..Default::default()
                },
                String::new(),
                1
            )
            .ignored
        );
    }
    #[test]
    fn new_secret_never_observed_saved_suspended_or_exported() {
        let mut p = Persistence::default();
        p.activate("alice", "private", 0);
        loaded(&mut p, "load", 3, "safe draft");
        p.observe(
            Snapshot {
                body: "/word start synthetic-secret".into(),
                ..Default::default()
            },
            2,
        );
        assert_eq!(p.local().unwrap().body, "safe draft");
        assert!(p.next(1000).is_none());
        assert!(!p.recovery_text().contains("secret"));
        for op in [
            Operation::Save {
                room: "private".into(),
                version: 3,
                snapshot: Snapshot {
                    body: "/word start secret".into(),
                    ..Default::default()
                },
                keep: true,
                backup_id: String::new(),
            },
            Operation::Suspend {
                room: "private".into(),
                snapshot: Snapshot {
                    body: "/WORD START secret".into(),
                    ..Default::default()
                },
            },
        ] {
            assert!(!op.safe_body());
            assert!(!p.begin("bad".into(), op, 3));
        }
    }
    #[test]
    fn old_saved_secret_quarantines_without_blank_overwrite_and_other_room_still_saves() {
        let mut p = Persistence::default();
        p.activate("alice", "private", 0);
        loaded(&mut p, "load", 7, "/word start old-secret");
        assert!(p.quarantined());
        assert_eq!(p.version(), Some(7));
        assert!(p.local().unwrap().body.contains("old-secret"));
        p.observe(Snapshot::default(), 2);
        assert!(p.local().unwrap().body.contains("old-secret"));
        assert!(p.next(1000).is_none());
        assert!(p.manual(false).is_none());
        assert!(!p.recovery_text().contains("old-secret"));
        assert!(p.clear_quarantined(6).is_none());
        p.activate("alice", "lobby", 3);
        let op = p.next(3).unwrap();
        assert!(p.begin("lobby-load".into(), op, 3));
        p.acknowledge(
            ("lobby-load", "lobby"),
            Ack::Loaded,
            0,
            Snapshot::default(),
            String::new(),
            4,
        );
        p.observe(
            Snapshot {
                body: "ordinary".into(),
                ..Default::default()
            },
            5,
        );
        let op = p.next(1000).unwrap();
        assert!(matches!(&op,Operation::Save{room,..} if room=="lobby"));
        assert!(p.begin("safe-save".into(), op, 1000));
        p.acknowledge(
            ("safe-save", "lobby"),
            Ack::Saved,
            1,
            Snapshot {
                body: "ordinary".into(),
                ..Default::default()
            },
            String::new(),
            1001,
        );
        assert!(!p.dirty());
        p.activate("alice", "private", 1002);
        loaded(&mut p, "again", 7, "/word start old-secret");
        let clear = p.clear_quarantined(7).unwrap();
        assert!(
            matches!(&clear,Operation::Save{snapshot,keep:false,version:7,..} if snapshot.body.is_empty())
        );
        assert!(p.begin("explicit-clear".into(), clear, 1003));
        // A different device wrote a newer ordinary draft. Conflict cannot turn
        // into an automatic keep or a new-version delete; re-read is required.
        p.acknowledge(
            ("explicit-clear", "private"),
            Ack::Conflict,
            8,
            Snapshot::default(),
            "backup".into(),
            1004,
        );
        assert!(p.quarantined());
        assert!(p.clear_quarantined(8).is_none());
        assert!(p.next(3000).is_none());
        loaded(&mut p, "read-new", 8, "other-device ordinary");
        assert!(!p.quarantined());
        assert_eq!(p.local().unwrap().body, "other-device ordinary");
        assert!(p.clear_quarantined(8).is_none());
        assert!(p.next(4000).is_none());
    }
    #[test]
    fn explicit_matching_clear_ack_releases_only_quarantined_draft() {
        let mut p = Persistence::default();
        p.activate("alice", "private", 0);
        loaded(&mut p, "load", 2, "/word start old");
        let op = p.clear_quarantined(2).unwrap();
        assert!(p.begin("clear".into(), op, 2));
        p.acknowledge(
            ("clear", "private"),
            Ack::Saved,
            3,
            Snapshot::default(),
            String::new(),
            3,
        );
        assert!(!p.quarantined());
        assert_eq!(p.local().unwrap().body, "");
        assert_eq!(p.version(), Some(3));
        assert!(!p.dirty());
    }
    #[test]
    fn restored_sensitive_backup_is_not_a_current_remote_clear_authority() {
        let mut p = Persistence::default();
        p.activate("alice", "private", 0);
        loaded(&mut p, "load", 7, "current ordinary");
        p.restore_conflict(
            7,
            Snapshot {
                body: "/word \nstart old-backup".into(),
                ..Default::default()
            },
            "old-session".into(),
            2,
        );
        assert!(p.quarantined() && p.conflicted());
        assert!(p.clear_quarantined(7).is_none());
        assert!(p.manual(false).is_none());
        loaded(&mut p, "read-current", 7, "current ordinary");
        assert!(!p.quarantined());
        assert_eq!(p.local().unwrap().body, "current ordinary");
        assert!(p.clear_quarantined(7).is_none());
    }
}
