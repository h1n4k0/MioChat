//! Scope-bound settings, role assignment and transient disconnect requests.
use crate::session::Response;
use serde::Deserialize;
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ManagementIntent {
    pub command: String,
    pub requested_command: String,
    pub requested_target: String,
    pub room: String,
    pub value: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct ManagementReceipt {
    pub command: String,
    pub requested_command: String,
    pub actor: String,
    pub origin_scope: String,
    pub requested_target: String,
    pub target: String,
    pub room: String,
    pub value: String,
    pub committed: bool,
}
impl ManagementIntent {
    pub fn form(command: &str, origin: &str) -> Option<Self> {
        if !matches!(command, "/slowmode" | "/kick" | "/role") {
            return None;
        }
        let room = if command == "/slowmode" {
            if origin.is_empty() || origin.starts_with("dm:") {
                return None;
            }
            origin.to_string()
        } else {
            String::new()
        };
        Some(Self {
            command: command.into(),
            requested_command: String::new(),
            requested_target: String::new(),
            room,
            value: String::new(),
        })
    }
    pub fn parse(command: &str, origin: &str) -> Option<Self> {
        let f = command.split_whitespace().collect::<Vec<_>>();
        let head = f.first()?.to_ascii_lowercase();
        let mut i = Self::form(&head, origin)?;
        match head.as_str() {
            "/slowmode" if f.len() == 2 => {
                let n = f[1].parse::<i64>().ok()?;
                if !(0..=3600).contains(&n) {
                    return None;
                }
                i.requested_target = i.room.clone();
                i.value = n.to_string();
            }
            "/kick" if f.len() == 2 => i.requested_target = f[1].into(),
            "/role" if f.len() == 3 && matches!(f[2], "admin" | "moderator" | "member") => {
                i.requested_target = f[1].into();
                i.value = f[2].into();
            }
            _ => return None,
        }
        i.requested_command = command.into();
        Some(i)
    }
    pub fn title(&self) -> &'static str {
        match self.command.as_str() {
            "/slowmode" => "确认频道慢速模式",
            "/role" => "确认全站角色变更",
            _ => "确认断开当前连接",
        }
    }
    pub fn target_label(&self) -> &'static str {
        if self.command == "/slowmode" {
            "秒数0–3600（0关闭）"
        } else {
            "目标用户名或 #UID"
        }
    }
    pub fn explanation(&self) -> String {
        match self.command.as_str(){
        "/slowmode"=>format!("固定频道 #{}；0关闭，最多3600秒。频道管理者豁免慢速限制，不作用于私聊。",self.room),
        "/role"=>"仅站长可把非站长账户设为 admin、moderator 或 member；提交后审计并刷新权限，不等于封禁。".into(),
        _=>"仅对低于自己权限的账户发出当前聊天室连接断开请求；不是封禁，不撤销凭据，允许再次登录。离线账户也可写入本次审计。".into(),
    }
    }
    pub fn form_command(&self, target: &str, extra: &str) -> Result<String, &'static str> {
        let target = target.trim();
        if target.is_empty() || target.chars().any(char::is_whitespace) {
            return Err("请填写一个有效目标或秒数。");
        }
        let command = if self.command == "/role" {
            format!("{} {target} {}", self.command, extra.trim())
        } else {
            format!("{} {target}", self.command)
        };
        if Self::parse(
            &command,
            if self.room.is_empty() {
                "lobby"
            } else {
                &self.room
            },
        )
        .is_none()
        {
            return Err("请核对秒数0–3600，或角色admin/moderator/member。");
        }
        Ok(command)
    }
    pub fn accepts(&self, r: &Response, owner: &str, origin: &str) -> bool {
        if owner.is_empty()
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
            || r.configuration_receipt.is_some()
            || r.sanction_receipt.is_some()
            || r.report_receipt.is_some()
            || r.word_receipt.is_some()
            || r.guess_receipt.is_some()
            || r.tic_receipt.is_some()
            || r.community_receipt.is_some()
            || r.direct_target.is_some()
        {
            return false;
        }
        let Some(v) = r.management_receipt.as_ref() else {
            return false;
        };
        v.committed
            && v.command == self.command
            && v.requested_command == self.requested_command
            && v.actor == owner
            && v.origin_scope == origin
            && v.requested_target == self.requested_target
            && !v.target.is_empty()
            && v.room == self.room
            && v.value == self.value
            && (self.command != "/slowmode" || v.target == self.room)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn three_management_forms_validate_scope_slots_and_preserve_pending_input() {
        use crate::direct_message::{DirectMessageForm, FormPurpose, FormSlot};
        for (cmd, target, extra) in [
            ("/slowmode", "+000", ""),
            ("/kick", "#42", ""),
            ("/role", "İUser", "moderator"),
        ] {
            let mut f = DirectMessageForm::new("owner".into(), "lobby".into(), 7);
            f.purpose = FormPurpose::Management(ManagementIntent::form(cmd, "lobby").unwrap());
            f.target = target.into();
            f.extra = extra.into();
            assert!(f.has_target() && !f.has_body());
            assert_eq!(f.has_extra(), cmd == "/role");
            assert_eq!(f.limit(FormSlot::Body), 0);
            assert!(
                ManagementIntent::parse(&f.command("owner", "lobby", 7).unwrap(), "lobby")
                    .is_some()
            );
            assert!(f.command("owner", "other", 7).is_err());
            assert!(f.command("other", "lobby", 7).is_err());
            assert!(f.command("owner", "lobby", 8).is_err());
            assert!(f.begin("id".into()));
            assert!(!f.begin("twice".into()));
            assert!(!f.finish("id", "owner", 7, false));
            assert_eq!(f.target, target);
        }
        assert!(ManagementIntent::form("/slowmode", "dm:peer").is_none());
        for s in [
            "/slowmode -1",
            "/slowmode 3601",
            "/slowmode 1 2",
            "/kick",
            "/role peer owner",
            "/role peer member extra",
        ] {
            assert!(ManagementIntent::parse(s, "lobby").is_none());
        }
        for s in [
            "/slowmode 0",
            "/slowmode 3600",
            "/role peer admin",
            "/role peer member",
        ] {
            assert!(ManagementIntent::parse(s, "lobby").is_some());
        }
    }
    #[test]
    fn management_receipt_requires_exact_scope_operation_value_and_unmixed_projection() {
        let i = ManagementIntent::parse("/role İUser moderator", "dm:peer").unwrap();
        let mut r = Response {
            kind: "action".into(),
            room: "dm:peer".into(),
            management_receipt: Some(ManagementReceipt {
                command: i.command.clone(),
                requested_command: i.requested_command.clone(),
                actor: "owner".into(),
                origin_scope: "dm:peer".into(),
                requested_target: "İUser".into(),
                target: "iuser".into(),
                value: "moderator".into(),
                committed: true,
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(i.accepts(&r, "owner", "dm:peer"));
        assert!(!i.accepts(&r, "other", "dm:peer"));
        assert!(!i.accepts(&r, "owner", "lobby"));
        let good = r.clone();
        r.management_receipt.as_mut().unwrap().value = "admin".into();
        assert!(!i.accepts(&r, "owner", "dm:peer"));
        r = good.clone();
        r.management_receipt.as_mut().unwrap().requested_target = "other".into();
        assert!(!i.accepts(&r, "owner", "dm:peer"));
        r = good.clone();
        r.management_receipt.as_mut().unwrap().command = "/kick".into();
        assert!(!i.accepts(&r, "owner", "dm:peer"));
        r = good.clone();
        r.sanction_receipt = Some(Default::default());
        assert!(!i.accepts(&r, "owner", "dm:peer"));
        r = good;
        r.profile = Some(Default::default());
        assert!(!i.accepts(&r, "owner", "dm:peer"));
    }
}
