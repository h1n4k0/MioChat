//! Explicit submission of one visible message to administrators; no context grant.
use crate::session::Response;
use serde::Deserialize;
pub const UNCERTAIN: &str =
    "举报或处理请求可能已提交，结果待确认；请重新连接核对举报记录或联系管理员，不要直接重复操作。";
pub const DISCLOSURE: &str = "提交举报将授权管理员查看所举报的这一条当前消息，不含私聊上下文或引用父消息。请核对消息编号与原因；聊天草稿不变。";
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ReportAction {
    Report,
    Resolve,
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReportIntent {
    pub action: ReportAction,
    pub id: i64,
    pub status: String,
    pub reason: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct ReportReceipt {
    pub operation: String,
    pub actor: String,
    pub origin_scope: String,
    pub report_id: i64,
    pub message_id: i64,
    pub requested_reason: String,
    pub reason: String,
    pub status: String,
    pub committed: bool,
}
impl ReportIntent {
    pub fn report_form(id: i64) -> Self {
        Self {
            action: ReportAction::Report,
            id,
            status: String::new(),
            reason: String::new(),
        }
    }
    pub fn parse(command: &str) -> Option<Self> {
        let (head, arg) = command.trim().split_once(' ')?;
        let (id, value) = arg.trim().split_once(' ')?;
        let id = id.parse::<i64>().ok()?;
        if id <= 0 {
            return None;
        };
        let value = value.trim();
        if head.eq_ignore_ascii_case("/report") && !value.is_empty() {
            Some(Self {
                action: ReportAction::Report,
                id,
                status: String::new(),
                reason: value.into(),
            })
        } else if head.eq_ignore_ascii_case("/resolve") && matches!(value, "resolved" | "rejected")
        {
            Some(Self {
                action: ReportAction::Resolve,
                id,
                status: value.into(),
                reason: String::new(),
            })
        } else {
            None
        }
    }
    pub fn has_body(&self) -> bool {
        self.action == ReportAction::Report
    }
    pub fn title(&self) -> &'static str {
        if self.action == ReportAction::Report {
            "确认提交举报"
        } else {
            "确认设置举报状态"
        }
    }
    pub fn command(&self, target: &str, body: &str) -> Result<String, &'static str> {
        if self.action == ReportAction::Resolve {
            if self.id <= 0 || !matches!(self.status.as_str(), "resolved" | "rejected") {
                return Err("无效的举报处理目标。");
            }
            return Ok(format!("/resolve {} {}", self.id, self.status));
        }
        let id = target
            .trim()
            .parse::<i64>()
            .map_err(|_| "请输入正整数消息编号。");
        let id = id?;
        if id <= 0 {
            return Err("请输入正整数消息编号。");
        }
        let reason = body.trim();
        if reason.is_empty() || reason.chars().count() > 240 {
            return Err("举报原因不能为空，最多240字符。");
        }
        Ok(format!("/report {id} {reason}"))
    }
    pub fn accepts(&self, r: &Response, user: &str, origin: &str) -> bool {
        if r.configuration_receipt.is_some() || r.management_receipt.is_some() {
            return false;
        }
        if r.kind != "action"
            || r.room != origin
            || r.replace
            || r.clear_view
            || r.draft.is_some()
            || r.message.is_some()
            || !r.messages.is_empty()
            || r.panel.is_some()
            || r.profile.is_some()
            || r.center.is_some()
            || r.preferences.is_some()
            || !r.form.is_empty()
            || !r.mention.is_empty()
            || r.word_receipt.is_some()
            || r.guess_receipt.is_some()
            || r.tic_receipt.is_some()
            || r.community_receipt.is_some()
            || r.direct_target.is_some()
        {
            return false;
        }
        let Some(v) = r.report_receipt.as_ref() else {
            return false;
        };
        if user.is_empty() || v.actor != user || v.origin_scope != origin || !v.committed {
            return false;
        }
        match self.action {
            ReportAction::Report => {
                v.operation == "report"
                    && v.report_id > 0
                    && v.message_id == self.id
                    && v.requested_reason == self.reason
                    && !v.reason.is_empty()
                    && v.reason.chars().count() <= 240
                    && v.status == "open"
            }
            ReportAction::Resolve => {
                v.operation == "resolve"
                    && v.report_id == self.id
                    && v.message_id == 0
                    && v.status == self.status
                    && v.requested_reason.is_empty()
                    && v.reason.is_empty()
            }
        }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn exact_report_and_resolution_receipts_never_authorize_other_projection() {
        let i = ReportIntent::parse("/report 42 synthetic reason").unwrap();
        let mut r = Response {
            kind: "action".into(),
            room: "dm:peer".into(),
            report_receipt: Some(ReportReceipt {
                operation: "report".into(),
                actor: "alice".into(),
                origin_scope: "dm:peer".into(),
                report_id: 8001,
                message_id: 42,
                requested_reason: "synthetic reason".into(),
                reason: "synthetic reason".into(),
                status: "open".into(),
                committed: true,
            }),
            ..Default::default()
        };
        assert!(i.accepts(&r, "alice", "dm:peer"));
        assert!(!i.accepts(&r, "other", "dm:peer"));
        r.report_receipt.as_mut().unwrap().message_id = 43;
        assert!(!i.accepts(&r, "alice", "dm:peer"));
        r.report_receipt.as_mut().unwrap().message_id = 42;
        r.profile = Some(crate::profile::Profile::default());
        assert!(!i.accepts(&r, "alice", "dm:peer"));
        let i = ReportIntent::parse("/resolve 8001 rejected").unwrap();
        r.profile = None;
        r.report_receipt = Some(ReportReceipt {
            operation: "resolve".into(),
            actor: "alice".into(),
            origin_scope: "dm:peer".into(),
            report_id: 8001,
            status: "rejected".into(),
            committed: true,
            ..Default::default()
        });
        assert!(i.accepts(&r, "alice", "dm:peer"));
        r.report_receipt.as_mut().unwrap().status = "resolved".into();
        assert!(!i.accepts(&r, "alice", "dm:peer"));
    }
    #[test]
    fn report_form_bound_inputs_and_fixed_resolution_have_distinct_routes() {
        use crate::direct_message::{DirectMessageForm, FormPurpose, FormSlot};
        let mut f = DirectMessageForm::new("alice".into(), "dm:peer".into(), 7);
        f.purpose = FormPurpose::Report(ReportIntent::report_form(42));
        f.target = "42".into();
        f.body = "report one current message".into();
        assert!(f.has_target() && f.has_body());
        assert_eq!(
            f.command("alice", "dm:peer", 7).unwrap(),
            "/report 42 report one current message"
        );
        assert!(f.command("alice", "lobby", 7).is_err());
        assert!(DISCLOSURE.contains("不含私聊上下文"));
        f.purpose = FormPurpose::Report(ReportIntent::parse("/resolve 8001 rejected").unwrap());
        assert!(!f.has_target() && !f.has_body());
        assert_eq!(f.limit(FormSlot::Body), 0);
        assert_eq!(
            f.command("alice", "dm:peer", 7).unwrap(),
            "/resolve 8001 rejected"
        );
        assert!(f.begin("id".into()));
        assert!(!f.begin("again".into()));
        assert!(!f.finish("old", "alice", 7, true));
        assert!(f.finish("id", "alice", 7, true));
    }
}
