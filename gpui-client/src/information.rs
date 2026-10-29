use crate::session::Response;
use serde::Deserialize;

#[derive(Clone, Debug, Default, Deserialize)]
pub struct Statistics {
    pub version: String,
    pub users: u64,
    pub messages: u64,
    pub active_sessions: u64,
    pub minute_messages: u64,
    pub hour_limited: u64,
    pub heap_bytes: u64,
    pub goroutines: u64,
    pub uptime_seconds: u64,
    pub online_sessions: u64,
    pub database_bytes: Option<u64>,
    pub free_disk_bytes: Option<u64>,
    #[serde(default)]
    pub storage_note: String,
}
impl Statistics {
    pub fn rows(&self) -> Vec<(String, String)> {
        let mut rows = vec![
            ("MioChat".into(), self.version.clone()),
            (
                "账户 / 消息".into(),
                format!("{} / {}", self.users, self.messages),
            ),
            (
                "活跃 / 在线会话".into(),
                format!("{} / {}", self.active_sessions, self.online_sessions),
            ),
            ("最近一分钟消息".into(), self.minute_messages.to_string()),
            ("本小时限流".into(), self.hour_limited.to_string()),
            (
                "Go 堆".into(),
                format!("{:.1} MiB", self.heap_bytes as f64 / 1_048_576.0),
            ),
            ("Goroutine".into(), self.goroutines.to_string()),
            ("运行时间".into(), format!("{} 秒", self.uptime_seconds)),
        ];
        if let Some(bytes) = self.database_bytes {
            rows.push((
                "数据库主文件（不含 WAL）".into(),
                format!("{:.1} MiB", bytes as f64 / 1_048_576.0),
            ));
        }
        if let Some(bytes) = self.free_disk_bytes {
            rows.push((
                "可用磁盘".into(),
                format!("{:.2} GiB", bytes as f64 / 1_073_741_824.0),
            ));
        }
        rows
    }
}
pub fn is_information_command(command: &str) -> bool {
    if crate::word::is_view(command) || crate::guess::is_view(command) {
        return true;
    }
    command.split_whitespace().next().is_some_and(|c| {
        matches!(
            c.to_ascii_lowercase().as_str(),
            "/stats"
                | "/time"
                | "/art"
                | "/board"
                | "/gallery"
                | "/events"
                | "/results"
                | "/participants"
                | "/daily"
                | "/object"
                | "/commands"
                | "/menu"
                | "/help"
        )
    })
}
/// Error handling may preserve input, never smuggle another projected update.
pub fn is_plain_error_response(r: &Response) -> bool {
    r.kind == "error"
        && r.attachment.is_none()
        && r.dot.is_none()
        && r.completion.is_none()
        && r.secret_admin.is_none()
        && r.maintenance.is_none()
        && r.composition.is_none()
        && r.panel.is_none()
        && r.profile.is_none()
        && r.center.is_none()
        && r.preferences.is_none()
        && r.device_key.is_none()
        && !r.key_reminder
        && r.rooms.is_empty()
        && r.message.is_none()
        && r.messages.is_empty()
        && r.draft.is_none()
        && !r.draft_changed
        && !r.replace
        && !r.clear_view
        && r.form.is_empty()
        && r.mention.is_empty()
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
pub fn information_panel_payload(p: &crate::session::Panel) -> bool {
    if p.moderation.is_some() || !p.rooms.is_empty() || !p.members.is_empty() || p.details.is_some()
    {
        return false;
    }
    match p.kind.as_str() {
        "stats" => p.community.is_none() && p.server_time.is_empty(),
        "time" => p.community.is_none() && p.statistics.is_none(),
        "art" | "menu" => {
            p.community.is_none() && p.statistics.is_none() && p.server_time.is_empty()
        }
        "community" => p.statistics.is_none() && p.server_time.is_empty(),
        _ => false,
    }
}
pub fn accepts_information_ack(
    response: &Response,
    requested_room: &str,
    current_room: &str,
) -> bool {
    if response.composition.is_some()
        || response.configuration_receipt.is_some()
        || response.management_receipt.is_some()
        || response.sanction_receipt.is_some()
        || response.report_receipt.is_some()
        || response.guess_receipt.is_some()
        || response.word_receipt.is_some()
        || response.tic_receipt.is_some()
        || response.community_receipt.is_some()
        || response.direct_target.is_some()
        || response.profile.is_some()
        || response.center.is_some()
        || response.preferences.is_some()
        || response.device_key.is_some()
        || response.key_reminder
        || !response.rooms.is_empty()
        || !response.form.is_empty()
        || !response.mention.is_empty()
    {
        return false;
    }
    response.kind == "action"
        && requested_room == current_room
        && response.room == requested_room
        && !response.replace
        && !response.clear_view
        && response.draft.is_none()
        && response.message.is_none()
        && response.messages.is_empty()
        && response
            .panel
            .as_ref()
            .is_some_and(information_panel_payload)
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn information_ack_never_retargets_or_mutates_chat() {
        let mut response: Response = serde_json::from_str(r#"{"type":"action","room":"lobby","panel":{"kind":"time","title":"时间","server_time":"2026-10-27 22:00:00 CST"}}"#).unwrap();
        assert!(accepts_information_ack(&response, "lobby", "lobby"));
        assert!(!accepts_information_ack(&response, "lobby", "dm:peer"));
        response.clear_view = true;
        assert!(!accepts_information_ack(&response, "lobby", "lobby"));
        assert!(is_information_command(" /STATS "));
        assert!(!is_information_command("/health"));
    }
    #[test]
    fn unknown_storage_is_not_presented_as_zero() {
        let statistics = Statistics::default();
        assert_eq!(statistics.rows().len(), 8);
        let statistics = Statistics {
            database_bytes: Some(0),
            free_disk_bytes: Some(1 << 30),
            ..statistics
        };
        let rows = statistics.rows();
        assert_eq!(rows.len(), 10);
        assert_eq!(rows[8].1, "0.0 MiB");
        assert_eq!(rows[9].1, "1.00 GiB");
    }

    #[test]

    fn public_information_rejects_private_sibling() {
        let r: Response = serde_json::from_str(r#"{"type":"action","room":"lobby","panel":{"kind":"stats","title":"mixed","moderation":{"viewer":"owner","command":"/admin","requested":"","canonical":"","origin_scope":"lobby","window":0,"explanation":"private","audited":false}}}"#).unwrap();
        assert!(
            !accepts_information_ack(&r, "lobby", "lobby"),
            "PUBLIC_ACCEPTED_PRIVATE_SIBLING"
        );
    }

    #[test]
    fn information_family_rejects_mixed_projections_and_unrelated_effects() {
        for kind in ["stats", "time", "art", "menu", "community"] {
            let mut r: Response =
                serde_json::from_str(r#"{"type":"action","room":"lobby"}"#).unwrap();
            r.panel = Some(crate::session::Panel {
                kind: kind.into(),
                ..Default::default()
            });
            assert!(accepts_information_ack(&r, "lobby", "lobby"));
            r.panel.as_mut().unwrap().moderation = Some(Default::default());
            assert!(!information_panel_payload(r.panel.as_ref().unwrap()));
            assert!(!accepts_information_ack(&r, "lobby", "lobby"));
            r.panel.as_mut().unwrap().moderation = None;
            r.profile = Some(Default::default());
            assert!(!accepts_information_ack(&r, "lobby", "lobby"));
        }
    }
}
