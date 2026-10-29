//! Explicit, session-bound account/channel moderation; never a chat draft.
use crate::session::Response;
use serde::Deserialize;
pub const UNCERTAIN: &str = "管理操作结果待确认；请重新连接核对制裁列表和审计，勿直接重复提交。";
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SanctionIntent {
    pub self_ip_force: Option<bool>,
    pub command: String,
    pub requested_command: String,
    pub requested_target: String,
    pub room: String,
    pub remove: bool,
    pub permanent: bool,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct SanctionReceipt {
    pub self_ip_force: Option<bool>,
    pub command: String,
    pub requested_command: String,
    pub actor: String,
    pub origin_scope: String,
    pub requested_target: String,
    pub target: String,
    pub kind: String,
    pub room: String,
    pub expires: i64,
    pub remove: bool,
    pub committed: bool,
}
impl SanctionIntent {
    pub fn form(command: &str, origin: &str) -> Option<Self> {
        if !matches!(
            command,
            "/mute" | "/unmute" | "/gmute" | "/ungmute" | "/ban" | "/unban" | "/banip" | "/unbanip"
        ) {
            return None;
        }
        let room = if matches!(command, "/mute" | "/unmute") {
            if origin.is_empty() || origin.starts_with("dm:") {
                return None;
            }
            origin.to_string()
        } else {
            String::new()
        };
        Some(Self {
            self_ip_force: if command == "/banip" {
                Some(false)
            } else {
                None
            },
            command: command.into(),
            requested_command: String::new(),
            requested_target: String::new(),
            room,
            remove: command.starts_with("/un"),
            permanent: true,
        })
    }
    pub fn parse(command: &str, origin: &str) -> Option<Self> {
        let mut fields = command.split_whitespace();
        let head = fields.next()?.to_ascii_lowercase();
        let mut i = Self::form(&head, origin)?;
        i.self_ip_force = None; // Explicit composer command retains manual --force syntax.
        i.requested_target = fields.next()?.into();
        i.requested_command = command.into();
        let duration = fields.next().unwrap_or("permanent");
        i.permanent = i.remove || matches!(duration, "permanent" | "forever" | "0");
        Some(i)
    }
    pub fn is_ip(&self) -> bool {
        matches!(self.command.as_str(), "/banip" | "/unbanip")
    }
    pub fn target_label(&self) -> &'static str {
        if self.is_ip() {
            "目标IP或CIDR（仅聊天室）"
        } else {
            "目标用户名或 #UID"
        }
    }
    pub fn title(&self) -> &'static str {
        if self.remove {
            "确认解除管理限制"
        } else {
            "确认施加管理限制"
        }
    }
    pub fn explanation(&self) -> String {
        if self.is_ip() {
            format!(
                "操作 {} · 仅聊天室的IP/网络范围，不修改系统防火墙或管理SSH。{} 当前连接也在范围内时必须明确确认，成功后可能在回执送达前断线；先核对结果，勿直接重试。",
                self.command,
                if self.remove {
                    "解除对应范围的限制，不删除审计。"
                } else {
                    "期限生效后限制该范围内的聊天室连接。"
                }
            )
        } else {
            format!(
                "操作 {} · {} · {}。目标必须为低于可管理权限的账户，提交后写入审计；封号可能立即断开目标的聊天室连接。",
                self.command,
                if self.room.is_empty() {
                    "全站范围".into()
                } else {
                    format!("仅频道 #{}", self.room)
                },
                if self.remove {
                    "解除已有对应限制，不删除审计"
                } else {
                    "永久或按指定期限生效"
                }
            )
        }
    }
    pub fn form_command(
        &self,
        target: &str,
        duration: &str,
        reason: &str,
    ) -> Result<String, &'static str> {
        let target = target.trim();
        if target.is_empty() || target.chars().any(char::is_whitespace) {
            return Err("请输入一个用户名或 #UID。");
        }
        if self.remove {
            return Ok(format!("{} {target}", self.command));
        }
        let duration = duration.trim();
        if duration.is_empty() || duration.chars().any(char::is_whitespace) {
            return Err("请填写一个期限，如 10m、7d 或 permanent。");
        }
        let reason = reason.split_whitespace().collect::<Vec<_>>().join(" ");
        if reason.is_empty() || reason.chars().count() > 240 {
            return Err("请填写不超过240字符的原因。");
        }
        Ok(format!("{} {target} {duration} {reason}", self.command))
    }
    pub fn accepts(&self, r: &Response, user: &str, origin: &str) -> bool {
        if r.configuration_receipt.is_some() || r.management_receipt.is_some() {
            return false;
        }
        if user.is_empty()
            || r.kind != "action"
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
            || r.report_receipt.is_some()
            || r.word_receipt.is_some()
            || r.guess_receipt.is_some()
            || r.tic_receipt.is_some()
            || r.community_receipt.is_some()
            || r.direct_target.is_some()
        {
            return false;
        }
        let Some(v) = r.sanction_receipt.as_ref() else {
            return false;
        };
        v.committed
            && v.self_ip_force == self.self_ip_force
            && v.actor == user
            && v.origin_scope == origin
            && v.command == self.command
            && v.requested_command == self.requested_command
            && v.requested_target == self.requested_target
            && !v.target.is_empty()
            && v.room == self.room
            && v.remove == self.remove
            && v.kind
                == if self.is_ip() {
                    "ip"
                } else if self.command.contains("mute") {
                    "mute"
                } else {
                    "user"
                }
            && if self.permanent {
                v.expires == 0
            } else {
                v.expires > 0
            }
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn form_scope_slots_and_pending_are_bound() {
        use crate::direct_message::{DirectMessageForm, FormPurpose, FormSlot};
        let mut f = DirectMessageForm::new("owner".into(), "lobby".into(), 4);
        f.purpose = FormPurpose::Sanction(SanctionIntent::form("/mute", "lobby").unwrap());
        f.target = "İuser".into();
        f.extra = "7d".into();
        f.body = "synthetic reason".into();
        assert!(f.has_target() && f.has_extra() && f.has_body());
        assert_eq!(f.limit(FormSlot::Body), 240);
        assert_eq!(
            f.command("owner", "lobby", 4).unwrap(),
            "/mute İuser 7d synthetic reason"
        );
        assert!(f.command("owner", "elsewhere", 4).is_err());
        assert!(f.command("other", "lobby", 4).is_err());
        assert!(f.command("owner", "lobby", 5).is_err());
        assert!(f.begin("one".into()));
        assert!(!f.begin("again".into()));
        assert!(!f.finish("old", "owner", 4, true));
        assert!(!f.finish("one", "owner", 4, false));
        assert_eq!(f.body, "synthetic reason");
        f.purpose = FormPurpose::Sanction(SanctionIntent::form("/unmute", "lobby").unwrap());
        assert!(!f.has_body() && !f.has_extra());
        assert_eq!(f.command("owner", "lobby", 4).unwrap(), "/unmute İuser");
        assert!(SanctionIntent::form("/mute", "dm:peer").is_none());
        assert!(SanctionIntent::form("/gmute", "dm:peer").is_some());
    }
    #[test]
    fn receipt_binds_canonical_identity_origin_kind_expiry_and_no_projection() {
        let i = SanctionIntent::parse("/ban İuser 7d reason", "dm:peer").unwrap();
        let mut r = Response {
            kind: "action".into(),
            room: "dm:peer".into(),
            sanction_receipt: Some(SanctionReceipt {
                command: "/ban".into(),
                requested_command: i.requested_command.clone(),
                actor: "owner".into(),
                origin_scope: "dm:peer".into(),
                requested_target: "İuser".into(),
                target: "iuser".into(),
                kind: "user".into(),
                expires: 42,
                committed: true,
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(i.accepts(&r, "owner", "dm:peer"));
        assert!(!i.accepts(&r, "other", "dm:peer"));
        assert!(!i.accepts(&r, "owner", "lobby"));
        r.sanction_receipt.as_mut().unwrap().requested_target = "other".into();
        assert!(!i.accepts(&r, "owner", "dm:peer"));
        r.sanction_receipt.as_mut().unwrap().requested_target = "İuser".into();
        r.sanction_receipt.as_mut().unwrap().expires = 0;
        assert!(!i.accepts(&r, "owner", "dm:peer"));
        r.sanction_receipt.as_mut().unwrap().expires = 42;
        r.profile = Some(crate::profile::Profile::default());
        assert!(!i.accepts(&r, "owner", "dm:peer"));
    }
    #[test]
    fn native_self_force_is_bound_checkbox_not_reason_text_or_manual_command() {
        use crate::{
            direct_message::{DirectMessageForm, FormPurpose},
            session::Request,
        };
        let mut f = DirectMessageForm::new("owner".into(), "lobby".into(), 7);
        f.purpose = FormPurpose::Sanction(SanctionIntent::form("/banip", "lobby").unwrap());
        f.target = "192.0.2.0/24".into();
        f.extra = "1h".into();
        f.body = "typed --force is not native confirmation".into();
        let command = f.command("owner", "lobby", 7).unwrap();
        let i = f
            .sanction_submission(true, "owner", "lobby", 7, &command)
            .unwrap();
        assert_eq!(i.self_ip_force, Some(false));
        let mut request = Request::simple("command", "id");
        request.body = &command;
        request.self_ip_force = i.self_ip_force;
        assert_eq!(
            serde_json::to_value(&request).unwrap()["self_ip_force"],
            false
        );
        assert!(
            SanctionIntent::parse(&command, "lobby")
                .unwrap()
                .self_ip_force
                .is_none()
        );
        assert!(
            f.sanction_submission(false, "owner", "lobby", 7, &command)
                .is_none()
        );
        assert!(
            f.sanction_submission(true, "other", "lobby", 7, &command)
                .is_none()
        );
        assert!(
            f.sanction_submission(true, "owner", "other", 7, &command)
                .is_none()
        );
        assert!(
            f.sanction_submission(true, "owner", "lobby", 8, &command)
                .is_none()
        );
        assert!(
            f.sanction_submission(true, "owner", "lobby", 7, "/banip 198.51.100.0/24 1h other")
                .is_none()
        );
        if let FormPurpose::Sanction(i) = &mut f.purpose {
            i.self_ip_force = Some(true)
        };
        let i = f
            .sanction_submission(true, "owner", "lobby", 7, &command)
            .unwrap();
        assert_eq!(i.self_ip_force, Some(true));
        assert!(f.begin("send".into()));
        assert!(
            f.sanction_submission(true, "owner", "lobby", 7, &command)
                .is_none()
        );
        assert!(!f.finish("old", "owner", 7, true));
        assert!(!f.finish("send", "owner", 7, false));
        f.purpose = FormPurpose::Direct;
        assert!(
            f.sanction_submission(true, "owner", "lobby", 7, &command)
                .is_none()
        );
    }
    #[test]
    fn ip_receipt_requires_exact_optional_force_and_canonical_ip_kind() {
        let mut i =
            SanctionIntent::parse("/banip ::ffff:198.51.100.0/120 1h fixture", "lobby").unwrap();
        i.self_ip_force = Some(false);
        let mut r = Response {
            kind: "action".into(),
            room: "lobby".into(),
            sanction_receipt: Some(SanctionReceipt {
                command: "/banip".into(),
                requested_command: i.requested_command.clone(),
                actor: "owner".into(),
                origin_scope: "lobby".into(),
                requested_target: i.requested_target.clone(),
                target: "198.51.100.0/24".into(),
                kind: "ip".into(),
                self_ip_force: Some(false),
                expires: 42,
                committed: true,
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(i.accepts(&r, "owner", "lobby"));
        for force in [None, Some(true)] {
            r.sanction_receipt.as_mut().unwrap().self_ip_force = force;
            assert!(!i.accepts(&r, "owner", "lobby"));
        }
        r.sanction_receipt.as_mut().unwrap().self_ip_force = Some(false);
        r.sanction_receipt.as_mut().unwrap().kind = "user".into();
        assert!(!i.accepts(&r, "owner", "lobby"));
    }
}
