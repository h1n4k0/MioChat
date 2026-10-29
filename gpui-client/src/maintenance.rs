//! Connection-bound background maintenance. Completion pushes only status;
//! private paths/diagnostics require a separately correlated, visible read.
use crate::{composition::Origin, session::Response};
use serde::{Deserialize, Serialize};
use std::fmt;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Kind {
    Backup,
    Check,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Operation {
    Start,
    Cancel,
    View,
    Complete,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum State {
    Running,
    CancelRequested,
    Complete,
    Cancelled,
    Failed,
    Unavailable,
}
impl State {
    pub fn terminal(self) -> bool {
        matches!(
            self,
            Self::Complete | Self::Cancelled | Self::Failed | Self::Unavailable
        )
    }
}
#[derive(Clone, Serialize)]
pub struct MaintenanceRequest {
    pub operation: Operation,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub kind: Option<Kind>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub job_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub start_id: String,
}
#[derive(Clone, Deserialize)]
pub struct Receipt {
    pub operation: Operation,
    pub job_id: String,
    pub start_id: String,
    pub kind: Kind,
    pub origin_room: String,
    pub state: State,
    pub committed: Option<bool>,
    pub has_warning: bool,
    #[serde(default)]
    pub lines: Vec<String>,
    #[serde(default)]
    pub warning: String,
}
impl fmt::Debug for Receipt {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("MaintenanceReceipt")
            .field("operation", &self.operation)
            .field("state", &self.state)
            .field("committed", &self.committed)
            .finish_non_exhaustive()
    }
}
#[derive(Clone)]
pub struct Pending {
    pub id: String,
    pub operation: Operation,
    pub epoch: u64,
    pub at: u64,
}
#[derive(Clone)]
pub struct Job {
    pub owner: String,
    pub generation: u64,
    pub origin: Origin,
    pub room: String,
    pub kind: Kind,
    pub start_id: String,
    pub job_id: String,
    pub pending: Option<Pending>,
    pub state: Option<State>,
    pub confirming: bool,
    pub uncertain: bool,
    pub rejected: bool,
    pub expired: bool,
    pub committed: Option<bool>,
    pub has_warning: bool,
    pub visible_epoch: Option<u64>,
    pub lines: Vec<String>,
    pub warning: String,
    pub status: String,
    pub view_requested: bool,
    pub completed_at: Option<u64>,
    pub opening_input: Option<String>,
    pub cancel_input: Option<String>,
}
impl Job {
    pub fn new(
        owner: String,
        generation: u64,
        origin: Origin,
        room: String,
        kind: Kind,
        epoch: u64,
        opening_input: Option<String>,
    ) -> Self {
        Self {
            owner,
            generation,
            origin,
            room,
            kind,
            start_id: String::new(),
            job_id: String::new(),
            pending: None,
            state: None,
            confirming: kind == Kind::Backup,
            uncertain: false,
            rejected: false,
            expired: false,
            committed: None,
            has_warning: false,
            visible_epoch: Some(epoch),
            lines: Vec::new(),
            warning: String::new(),
            status: "尚未启动作业".into(),
            view_requested: false,
            completed_at: None,
            opening_input,
            cancel_input: None,
        }
    }
    pub fn bound(&self, owner: &str, generation: u64, origin: Option<&Origin>) -> bool {
        self.owner == owner && self.generation == generation && origin == Some(&self.origin)
    }
    pub fn active(&self) -> bool {
        !self.expired
            && !self.rejected
            && (self.confirming || self.state.is_none_or(|s| !s.terminal()) || self.uncertain)
    }
    pub fn visible(&self, epoch: u64) -> bool {
        self.visible_epoch == Some(epoch)
    }
    pub fn hide(&mut self) {
        self.visible_epoch = None;
        self.lines.clear();
        self.warning.clear();
        self.view_requested = false;
        if self
            .pending
            .as_ref()
            .is_some_and(|p| p.operation == Operation::View)
        {
            self.pending = None;
        }
    }
    pub fn open(&mut self, epoch: u64) {
        self.hide();
        self.visible_epoch = Some(epoch);
    }
    pub fn sync(&mut self, now: u64, epoch: u64) {
        if self.visible_epoch.is_some_and(|v| v != epoch) {
            self.hide();
        }
        if self
            .completed_at
            .is_some_and(|at| now.saturating_sub(at) >= 300_000)
        {
            self.hide();
            self.expired = true;
            self.status = "维护结果已过期；不会自动重跑作业".into();
        }
        if self
            .pending
            .as_ref()
            .is_some_and(|p| now.saturating_sub(p.at) >= 30_000)
        {
            let pending = self.pending.take().expect("matched pending");
            self.uncertain = pending.operation != Operation::View;
            self.status = "请求结果尚未确认；不会自动重试备份，可明确取消当前作业或稍后查看".into();
        }
    }
    pub fn begin(
        &mut self,
        operation: Operation,
        id: String,
        epoch: u64,
        now: u64,
    ) -> Option<MaintenanceRequest> {
        if self.pending.is_some() || self.expired {
            return None;
        }
        let request = match operation {
            Operation::Start if self.start_id.is_empty() && !self.rejected => {
                self.start_id = id.clone();
                self.confirming = false;
                self.status = "正在请求启动后台作业…".into();
                MaintenanceRequest {
                    operation,
                    kind: Some(self.kind),
                    job_id: String::new(),
                    start_id: String::new(),
                }
            }
            Operation::Cancel
                if !self.start_id.is_empty() && !self.state.is_some_and(State::terminal) =>
            {
                MaintenanceRequest {
                    operation,
                    kind: None,
                    job_id: self.job_id.clone(),
                    start_id: self.start_id.clone(),
                }
            }
            Operation::View
                if self.visible(epoch)
                    && self.state.is_some_and(State::terminal)
                    && self.state != Some(State::Unavailable)
                    && !self.job_id.is_empty() =>
            {
                self.view_requested = true;
                MaintenanceRequest {
                    operation,
                    kind: None,
                    job_id: self.job_id.clone(),
                    start_id: self.start_id.clone(),
                }
            }
            _ => return None,
        };
        self.pending = Some(Pending {
            id,
            operation,
            epoch,
            at: now,
        });
        Some(request)
    }
    pub fn matches_request(&self, id: &str) -> bool {
        !id.is_empty() && (id == self.start_id || self.pending.as_ref().is_some_and(|p| p.id == id))
    }
    pub fn admits(&self, r: &Response, epoch: u64) -> bool {
        if crate::information::is_plain_error_response(r) {
            return self.pending.as_ref().is_some_and(|p| {
                p.id == r.request_id
                    && (p.operation != Operation::View || self.visible(epoch) && p.epoch == epoch)
            });
        }
        let Some(c) = r.maintenance.as_ref() else {
            return false;
        };
        if self.start_id.is_empty()
            || r.request_id.is_empty()
            || r.kind != "maintenance"
            || !exclusive(r)
            || c.kind != self.kind
            || c.origin_room != self.room
            || r.room != self.room
            || c.start_id != self.start_id
            || c.job_id
                .parse::<u64>()
                .ok()
                .is_none_or(|n| n == 0 || n.to_string() != c.job_id)
            || c.job_id.len() > 20
            || !self.job_id.is_empty() && c.job_id != self.job_id
        {
            return false;
        }
        let expected_commit = match c.state {
            State::Complete => Some(self.kind == Kind::Backup),
            State::Cancelled | State::Failed => Some(false),
            _ => None,
        };
        if c.committed != expected_commit
            || c.has_warning && !(c.state == State::Complete && self.kind == Kind::Backup)
        {
            return false;
        }
        if c.operation == Operation::View
            && (c.has_warning == c.warning.is_empty() || c.lines.is_empty())
        {
            return false;
        }
        if c.operation != Operation::View && (!c.lines.is_empty() || !c.warning.is_empty()) {
            return false;
        }
        if c.operation == Operation::Complete
            && self.state.is_some_and(State::terminal)
            && (self.state != Some(c.state)
                || self.committed != c.committed
                || self.has_warning != c.has_warning)
        {
            return false;
        }
        match c.operation {
            Operation::Start => r.request_id == self.start_id && c.state == State::Running,
            Operation::Complete => r.request_id == self.start_id && c.state.terminal(),
            Operation::Cancel => {
                self.pending
                    .as_ref()
                    .is_some_and(|p| p.operation == Operation::Cancel && p.id == r.request_id)
                    && (c.state == State::CancelRequested || c.state.terminal())
            }
            Operation::View => {
                c.state == self.state.unwrap_or(State::Unavailable)
                    && c.committed == self.committed
                    && c.has_warning == self.has_warning
                    && self.visible(epoch)
                    && self.pending.as_ref().is_some_and(|p| {
                        p.operation == Operation::View && p.id == r.request_id && p.epoch == epoch
                    })
                    && c.state.terminal()
                    && c.state != State::Unavailable
                    && c.lines.len() <= 8
                    && c.lines.iter().all(|s| s.len() <= 16_384)
                    && c.warning.len() <= 4096
            }
        }
    }
    /// Returns true only for a matching acknowledged Start/Cancel; the actual
    /// UI consumes only its captured unchanged typed command, never menu input.
    pub fn accept(&mut self, r: &Response, epoch: u64, now: u64) -> Option<bool> {
        if !self.admits(r, epoch) {
            return None;
        }
        if r.kind == "error" {
            let p = self.pending.take()?;
            if p.operation == Operation::Start {
                self.rejected = true;
                self.uncertain = false;
            }
            self.status = r.error.chars().take(300).collect();
            return Some(false);
        }
        let c = r.maintenance.as_ref()?;
        if self.job_id.is_empty() {
            self.job_id = c.job_id.clone();
        }
        let consume = matches!(c.operation, Operation::Start | Operation::Cancel)
            && self
                .pending
                .as_ref()
                .is_some_and(|p| p.operation == c.operation && p.id == r.request_id);
        if self.pending.as_ref().is_some_and(|p| p.id == r.request_id) {
            self.pending = None;
        }
        if c.operation == Operation::View {
            self.lines = c.lines.clone();
            self.warning = c.warning.clone();
            self.committed = c.committed;
        } else if !self.state.is_some_and(State::terminal) {
            self.state = Some(c.state);
            self.committed = c.committed;
            self.has_warning = c.has_warning;
            if c.state.terminal() {
                self.completed_at = Some(now);
                self.uncertain = false;
            }
        }
        self.status = match self.state.unwrap_or(c.state) {
            State::Running => "作业正在后台运行；可关闭此窗继续聊天",
            State::CancelRequested => "已请求取消；已提交的备份不会撤回",
            State::Complete if self.committed == Some(true) => "备份已提交到服务器；不会自动重跑",
            State::Complete => "数据库与配置检查已完成",
            State::Cancelled => "作业已取消；未提交新的可用备份",
            State::Failed => "作业未完成；可明确查看诊断",
            State::Unavailable => "当前权限无法披露结果；这不表示备份未提交",
        }
        .into();
        Some(consume)
    }
}
pub fn exclusive(r: &Response) -> bool {
    // Reuse the audited sibling-field predicate, but composition itself is also
    // a sibling here. The outer dispatcher rejects this payload before every
    // other workflow even if its kind/request identity is forged.
    r.secret_admin.is_none() && crate::composition::exclusive_shared(r) && r.composition.is_none()
}

#[cfg(test)]
mod tests {
    use super::*;
    fn job(kind: Kind) -> Job {
        Job::new(
            "admin".into(),
            7,
            Origin::synthetic("fixture", 9),
            "lobby".into(),
            kind,
            4,
            Some("/backup".into()),
        )
    }
    fn response(j: &Job, operation: Operation, state: State, id: &str) -> Response {
        let committed = match state {
            State::Complete => Some(j.kind == Kind::Backup),
            State::Cancelled | State::Failed => Some(false),
            _ => None,
        };
        Response {
            kind: "maintenance".into(),
            request_id: id.into(),
            room: j.room.clone(),
            maintenance: Some(Receipt {
                operation,
                job_id: "1".into(),
                start_id: j.start_id.clone(),
                kind: j.kind,
                origin_room: j.room.clone(),
                state,
                committed,
                has_warning: false,
                lines: Vec::new(),
                warning: String::new(),
            }),
            ..Response::default()
        }
    }
    fn started(j: &mut Job) {
        j.begin(Operation::Start, "start".into(), 4, 10).unwrap();
        let r = response(j, Operation::Start, State::Running, "start");
        assert_eq!(j.accept(&r, 4, 11), Some(true));
    }
    #[test]
    fn maintenance_completion_is_nonprivate_and_result_requires_current_visible_read() {
        let mut j = job(Kind::Backup);
        assert!(j.confirming);
        started(&mut j);
        let mut done = response(&j, Operation::Complete, State::Complete, "start");
        done.maintenance
            .as_mut()
            .unwrap()
            .lines
            .push("private-server-path".into());
        assert!(!j.admits(&done, 4));
        done.maintenance.as_mut().unwrap().lines.clear();
        assert_eq!(j.accept(&done, 4, 20), Some(false));
        assert!(j.lines.is_empty());
        let request = j.begin(Operation::View, "view".into(), 4, 21).unwrap();
        assert_eq!(request.job_id, "1");
        let mut view = response(&j, Operation::View, State::Complete, "view");
        view.maintenance.as_mut().unwrap().lines = vec!["private-server-path".into()];
        assert!(j.accept(&view, 4, 22).is_some());
        assert_eq!(j.lines, ["private-server-path"]);
        assert!(!format!("{view:?}").contains("private-server-path"));
        j.sync(23, 5);
        assert!(j.lines.is_empty());
        assert!(!j.visible(5));
        assert!(!j.admits(&view, 5));
        j.open(6);
        assert!(
            j.begin(Operation::View, "fresh-view".into(), 6, 24)
                .is_some()
        );
        assert!(!j.admits(&view, 6));
    }
    #[test]
    fn maintenance_scope_origin_owner_generation_and_mixed_frames_cannot_project() {
        let mut j = job(Kind::Check);
        started(&mut j);
        assert!(j.bound("admin", 7, Some(&Origin::synthetic("fixture", 9))));
        assert!(!j.bound("admin", 8, Some(&Origin::synthetic("fixture", 9))));
        assert!(!j.bound("other", 7, Some(&Origin::synthetic("fixture", 9))));
        assert!(!j.bound("admin", 7, Some(&Origin::synthetic("other-host", 9))));
        assert!(!j.bound("admin", 7, None));
        let base = response(&j, Operation::Complete, State::Complete, "start");
        for variation in 0..5 {
            let mut r = base.clone();
            match variation {
                0 => r.kind = "state".into(),
                1 => r.room = "dm:bob".into(),
                2 => r.maintenance.as_mut().unwrap().start_id = "old-start".into(),
                3 => r.profile = Some(crate::profile::Profile::default()),
                _ => r.error = "mixed-error".into(),
            }
            assert!(!j.admits(&r, 4));
            assert!(j.accept(&r, 4, 20).is_none());
            assert_eq!(j.state, Some(State::Running));
        }
        let mut unstarted = job(Kind::Check);
        let r = response(&unstarted, Operation::Start, State::Running, "");
        assert!(!unstarted.admits(&r, 4));
        assert!(unstarted.accept(&r, 4, 20).is_none());
    }
    #[test]
    fn maintenance_cancel_is_requested_and_late_ack_cannot_regress_committed_job() {
        let mut j = job(Kind::Backup);
        started(&mut j);
        j.begin(Operation::Cancel, "cancel".into(), 4, 12).unwrap();
        let done = response(&j, Operation::Complete, State::Complete, "start");
        j.accept(&done, 4, 13).unwrap();
        assert_eq!(j.committed, Some(true));
        let cancel = response(&j, Operation::Cancel, State::CancelRequested, "cancel");
        j.accept(&cancel, 4, 14).unwrap();
        assert_eq!(j.state, Some(State::Complete));
        assert_eq!(j.committed, Some(true));
        assert!(j.pending.is_none());
        j.begin(Operation::View, "view-new".into(), 4, 15).unwrap();
        assert!(j.accept(&cancel, 4, 16).is_none());
        assert_eq!(j.pending.as_ref().unwrap().id, "view-new");
        let contradict = response(&j, Operation::Complete, State::Failed, "start");
        assert!(!j.admits(&contradict, 4));
    }
    #[test]
    fn maintenance_unknown_never_restarts_and_expiry_drops_private_copy() {
        let mut j = job(Kind::Backup);
        j.begin(Operation::Start, "start".into(), 4, 0).unwrap();
        j.sync(30_000, 4);
        assert!(j.uncertain && j.active());
        assert!(
            j.begin(Operation::Start, "retry".into(), 4, 31_000)
                .is_none()
        );
        let done = response(&j, Operation::Complete, State::Complete, "start");
        j.accept(&done, 4, 32_000).unwrap();
        assert!(!j.uncertain);
        j.lines.push("private path".into());
        j.warning = "private warning".into();
        j.sync(332_000, 4);
        assert!(j.expired && j.lines.is_empty() && j.warning.is_empty() && !j.active());
        assert!(
            j.begin(Operation::View, "late".into(), 4, 332_001)
                .is_none()
        );
    }
    #[test]
    fn maintenance_error_gate_rejects_global_effects_and_preserves_other_pending() {
        let mut j = job(Kind::Check);
        j.begin(Operation::Start, "start".into(), 4, 0).unwrap();
        let mut error = Response {
            kind: "error".into(),
            request_id: "start".into(),
            error: "denied".into(),
            ..Response::default()
        };
        error.preferences = Some(crate::preferences::Preferences::default());
        assert!(!j.admits(&error, 4));
        assert!(j.pending.is_some());
        error.preferences = None;
        error.request_id = "old".into();
        assert!(!j.admits(&error, 4));
        error.request_id = "start".into();
        assert_eq!(j.accept(&error, 4, 2), Some(false));
        assert!(j.rejected && j.pending.is_none());
        assert_eq!(j.opening_input.as_deref(), Some("/backup"));
    }
    #[test]
    fn maintenance_receipt_outcome_kind_and_private_warning_are_exact() {
        let mut j = job(Kind::Backup);
        started(&mut j);
        let mut r = response(&j, Operation::Complete, State::Complete, "start");
        r.maintenance.as_mut().unwrap().committed = Some(false);
        assert!(!j.admits(&r, 4));
        r.maintenance.as_mut().unwrap().committed = Some(true);
        r.maintenance.as_mut().unwrap().has_warning = true;
        assert!(j.accept(&r, 4, 2).is_some());
        j.begin(Operation::View, "read".into(), 4, 3).unwrap();
        let mut view = response(&j, Operation::View, State::Complete, "read");
        view.maintenance.as_mut().unwrap().lines = vec!["path".into()];
        assert!(!j.admits(&view, 4));
        view.maintenance.as_mut().unwrap().has_warning = true;
        view.maintenance.as_mut().unwrap().warning = "audit failed; already committed".into();
        assert!(j.admits(&view, 4));
    }
}
