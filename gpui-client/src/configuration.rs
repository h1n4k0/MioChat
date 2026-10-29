//! Global settings are a single, scope-bound audited transaction.
use crate::session::Response;
use serde::Deserialize;
pub const UNCERTAIN: &str = "配置提交结果待确认；请核对审计和当前策略，勿直接重复提交。";
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConfigurationIntent {
    pub command: String,
    pub requested_command: String,
    pub changes: Vec<ConfigurationChange>,
}
#[derive(Clone, Debug, Default, Eq, PartialEq, Deserialize)]
pub struct ConfigurationChange {
    pub key: String,
    pub value: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct ConfigurationReceipt {
    pub command: String,
    pub requested_command: String,
    pub actor: String,
    pub origin_scope: String,
    pub changes: Vec<ConfigurationChange>,
    pub committed: bool,
}
// Go splitArgument splits the first ASCII space, not an arbitrary whitespace token.
pub fn split_argument(value: &str) -> (&str, &str) {
    value
        .trim()
        .split_once(' ')
        .map_or((value.trim(), ""), |(a, b)| (a, b.trim()))
}
fn valid(key: &str, value: &str) -> bool {
    match key {
        "registration" => matches!(value, "open" | "closed" | "invite"),
        "registration_quota" => value.parse::<i64>().is_ok_and(|n| (1..=100).contains(&n)),
        "maintenance" => matches!(value, "on" | "off"),
        "maintenance_notice" => true, // cleanText is authoritative on the server.
        "retention" => value == "permanent",
        _ => false,
    }
}
impl ConfigurationIntent {
    pub fn form(command: &str) -> Option<Self> {
        matches!(command, "/config" | "/maintenance").then(|| Self {
            command: command.into(),
            requested_command: String::new(),
            changes: Vec::new(),
        })
    }
    pub fn parse(command: &str) -> Option<Self> {
        let (head, arg) = split_argument(command);
        let mut intent = Self::form(&head.to_ascii_lowercase())?;
        let (key, value) = split_argument(arg);
        if intent.command == "/config" {
            if !valid(key, value) {
                return None;
            }
            intent.changes.push(ConfigurationChange {
                key: key.into(),
                value: value.into(),
            });
        } else {
            if !valid("maintenance", key) {
                return None;
            }
            intent.changes.push(ConfigurationChange {
                key: "maintenance".into(),
                value: key.into(),
            });
            if !value.is_empty() {
                intent.changes.push(ConfigurationChange {
                    key: "maintenance_notice".into(),
                    value: value.into(),
                });
            }
        }
        intent.requested_command = command.into();
        Some(intent)
    }
    pub fn title(&self) -> &'static str {
        if self.command == "/config" {
            "确认全站配置"
        } else {
            "确认维护模式"
        }
    }
    pub fn target_label(&self) -> &'static str {
        if self.command == "/config" {
            "配置键（见下方允许列表）"
        } else {
            "维护状态：on / off"
        }
    }
    pub fn body_label(&self) -> &'static str {
        if self.command == "/config" {
            "值（仅 maintenance_notice 可为空）"
        } else {
            "可选公告（空白保留原公告）"
        }
    }
    pub fn explanation(&self) -> &'static str {
        "仅管理员/站长。registration=open/closed/invite；registration_quota=1–100；maintenance=on/off；maintenance_notice=公告（服务端清理后最多240字）；retention=permanent。维护不踢人，普通成员不可发送，管理员豁免。/maintenance 空公告保留原值；/config maintenance_notice 空值明确清空。提交会写审计，不改变当前聊天草稿或回复。"
    }
    pub fn form_command(&self, target: &str, body: &str) -> Result<String, &'static str> {
        let target = target.trim();
        if target.is_empty() || target.chars().any(char::is_whitespace) {
            return Err("请填写允许的配置键或 on/off。");
        }
        let command = format!("{} {target} {}", self.command, body.trim());
        let command = command.trim_end().to_string();
        Self::parse(&command).ok_or("请核对配置键、取值和配额范围；永久保留不可关闭。")?;
        Ok(command)
    }
    pub fn accepts(&self, r: &Response, owner: &str, origin: &str) -> bool {
        if owner.is_empty()
            || origin.is_empty()
            || r.kind != "action"
            || r.room != origin
            || r.replace
            || r.clear_view
            || r.draft_changed
            || r.draft.is_some()
            || r.message.is_some()
            || !r.messages.is_empty()
            || r.panel.is_some()
            || r.profile.is_some()
            || r.center.is_some()
            || r.preferences.is_some()
            || r.device_key.is_some()
            || r.key_reminder
            || !r.rooms.is_empty()
            || !r.form.is_empty()
            || !r.mention.is_empty()
            || r.management_receipt.is_some()
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
        let Some(v) = r.configuration_receipt.as_ref() else {
            return false;
        };
        v.committed
            && v.command == self.command
            && v.requested_command == self.requested_command
            && v.actor == owner
            && v.origin_scope == origin
            && !self.changes.is_empty()
            && v.changes.len() == self.changes.len()
            && v.changes
                .iter()
                .zip(&self.changes)
                .all(|(actual, requested)| {
                    actual.key == requested.key
                        && if actual.key == "maintenance_notice" {
                            actual.value.chars().count() <= 240 // Do not invent a second Unicode/ANSI cleaner.
                        } else {
                            actual.value == requested.value
                        }
                })
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::direct_message::{DirectMessageForm, FormPurpose, FormSlot};
    #[test]
    fn configuration_form_is_bound_and_empty_notice_semantics_are_distinct() {
        for (head, key, body, count) in [
            ("/config", "maintenance_notice", "", 1),
            ("/maintenance", "on", "", 1),
            ("/maintenance", "off", "公告", 2),
            ("/config", "registration_quota", "+001", 1),
        ] {
            let mut f = DirectMessageForm::new("owner".into(), "dm:peer".into(), 7);
            f.purpose = FormPurpose::Configuration(ConfigurationIntent::form(head).unwrap());
            f.target = key.into();
            f.body = body.into();
            assert!(f.has_target() && f.has_body() && !f.has_extra());
            assert_eq!(f.next_slot(FormSlot::Target, false), FormSlot::Body);
            assert_eq!(f.next_slot(FormSlot::Body, false), FormSlot::Target);
            let command = f.command("owner", "dm:peer", 7).unwrap();
            assert_eq!(
                ConfigurationIntent::parse(&command).unwrap().changes.len(),
                count
            );
            assert!(f.command("other", "dm:peer", 7).is_err());
            assert!(f.command("owner", "other", 7).is_err());
            assert!(f.command("owner", "dm:peer", 8).is_err());
            assert!(f.begin("id".into()));
            assert!(!f.begin("twice".into()));
            assert!(!f.finish("id", "owner", 7, false));
            assert_eq!(f.body, body);
        }
        for bad in [
            "/config",
            "/config retention 1",
            "/config registration_quota 101",
            "/config registration_quota 0",
            "/config\tregistration closed",
            "/config registration\tclosed",
            "/maintenance ON",
        ] {
            assert!(ConfigurationIntent::parse(bad).is_none(), "{bad}");
        }
        assert!(ConfigurationIntent::parse("/CONFIG registration invite").is_some());
        assert!(ConfigurationIntent::parse("/maintenance on \n公告").is_some());
    }
    #[test]
    fn configuration_ack_binds_exact_transaction_and_rejects_other_projections() {
        let i = ConfigurationIntent::parse("/maintenance on \u{1b}[31m公告").unwrap();
        let mut r = Response {
            kind: "action".into(),
            room: "lobby".into(),
            configuration_receipt: Some(ConfigurationReceipt {
                command: i.command.clone(),
                requested_command: i.requested_command.clone(),
                actor: "owner".into(),
                origin_scope: "lobby".into(),
                changes: vec![
                    ConfigurationChange {
                        key: "maintenance".into(),
                        value: "on".into(),
                    },
                    ConfigurationChange {
                        key: "maintenance_notice".into(),
                        value: "公告".into(),
                    },
                ],
                committed: true,
            }),
            ..Default::default()
        };
        assert!(i.accepts(&r, "owner", "lobby"));
        assert!(!i.accepts(&r, "other", "lobby"));
        assert!(!i.accepts(&r, "owner", "dm:peer"));
        let good = r.clone();
        r.configuration_receipt.as_mut().unwrap().changes[0].value = "off".into();
        assert!(!i.accepts(&r, "owner", "lobby"));
        r = good.clone();
        r.configuration_receipt.as_mut().unwrap().changes.pop();
        assert!(!i.accepts(&r, "owner", "lobby"));
        r = good.clone();
        r.configuration_receipt.as_mut().unwrap().requested_command = "/maintenance on".into();
        assert!(!i.accepts(&r, "owner", "lobby"));
        r = good.clone();
        r.preferences = Some(Default::default());
        assert!(!i.accepts(&r, "owner", "lobby"));
        r = good;
        r.management_receipt = Some(Default::default());
        assert!(!i.accepts(&r, "owner", "lobby"));
    }
}
