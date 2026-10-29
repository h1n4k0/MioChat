//! Private, snapshot-bound administrative reads. Never a chat message/draft.
use crate::{
    profile::Profile,
    session::{Panel, Response},
};
use serde::Deserialize;
#[derive(Clone, Default, Deserialize)]
pub struct DiagnosticRow {
    pub id: i64,
    pub at: i64,
    pub time: String,
    pub kind: String,
    pub detail: String,
}
impl std::fmt::Debug for DiagnosticRow {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("DiagnosticRow")
            .field("id", &self.id)
            .field("at", &self.at)
            .field("kind", &"[private]")
            .field("detail", &"[private]")
            .finish()
    }
}
/// Shared by the actual early ACK gate; a mislabeled private sibling is private.
pub fn requires_private_ticket(response: &Response, matching_read_pending: bool) -> bool {
    response
        .panel
        .as_ref()
        .is_some_and(|p| p.kind == "moderation" || p.moderation.is_some())
        || matching_read_pending && !crate::information::is_plain_error_response(response)
}
/// Shared by the actual renderer and ReadTicket, not merely a kind tag.
pub fn private_panel_payload(p: &Panel) -> bool {
    p.kind == "moderation"
        && p.moderation.is_some()
        && p.statistics.is_none()
        && p.community.is_none()
        && p.rooms.is_empty()
        && p.members.is_empty()
        && p.details.is_none()
        && p.server_time.is_empty()
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct LoginRow {
    pub ip: String,
    pub client: String,
    pub ok: bool,
    pub at: i64,
    pub time: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct IPAccount {
    pub name: String,
    #[serde(default)]
    pub note: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct SanctionRow {
    pub id: i64,
    pub kind: String,
    pub target: String,
    pub room: String,
    pub expires: i64,
    pub reason: String,
    pub revoked: bool,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct AuditRow {
    pub id: i64,
    pub at: i64,
    pub time: String,
    pub actor: String,
    pub action: String,
    pub target: String,
    pub detail: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct RecallRow {
    pub id: i64,
    pub sender: String,
    pub room: String,
    pub body: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct ReportRow {
    pub id: i64,
    pub user: String,
    pub message_id: i64,
    pub reason: String,
    pub status: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct ReportedMessage {
    pub report_id: i64,
    pub message_id: i64,
    pub sender: String,
    pub body: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct RevisionRow {
    pub id: i64,
    pub actor: String,
    pub action: String,
    pub body: String,
    pub at: i64,
    pub time: String,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct RevisionPage {
    pub message_id: i64,
    pub after: i64,
    pub until: i64,
    pub next_after: i64,
    #[serde(default)]
    pub rows: Vec<RevisionRow>,
}
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct ReadBounds {
    pub after: i64,
    pub until: Option<i64>,
}
impl RevisionPage {
    pub fn accepts(&self, id: i64, bounds: ReadBounds) -> bool {
        self.message_id == id
            && self.after == bounds.after
            && self.after >= 0
            && self.until >= self.after
            && bounds.until.is_none_or(|v| v == self.until)
            && self.rows.len() <= 100
            && self
                .rows
                .iter()
                .map(|r| r.id)
                .scan(self.after, |previous, id| {
                    let ok = id > *previous && id <= self.until;
                    *previous = id;
                    Some(ok)
                })
                .all(|ok| ok)
            && (self.next_after == 0
                || self.next_after < self.until
                    && self.rows.last().is_some_and(|r| r.id == self.next_after))
    }
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct ModerationView {
    #[serde(default)]
    pub diagnostics: Vec<DiagnosticRow>,
    pub health: Option<crate::information::Statistics>,
    #[serde(default)]
    pub sanctions: Vec<SanctionRow>,
    #[serde(default)]
    pub audits: Vec<AuditRow>,
    #[serde(default)]
    pub recalls: Vec<RecallRow>,
    #[serde(default)]
    pub reports: Vec<ReportRow>,
    pub reported: Option<ReportedMessage>,
    pub revisions: Option<RevisionPage>,
    pub viewer: String,
    pub command: String,
    pub requested: String,
    pub canonical: String,
    pub origin_scope: String,
    pub profile: Option<Profile>,
    #[serde(default)]
    pub logins: Vec<LoginRow>,
    #[serde(default)]
    pub users: Vec<IPAccount>,
    pub window: usize,
    pub explanation: String,
    pub audited: bool,
}
impl ModerationView {
    fn payload_matches(&self, command: &str) -> bool {
        (command == "/errors" || self.diagnostics.is_empty())
            && (command == "/health" || self.health.is_none())
            && (command == "/whois" || self.profile.is_none())
            && (matches!(command, "/ip" | "/logins") || self.logins.is_empty())
            && (command == "/ipusers" || self.users.is_empty())
            && (command == "/bans" || self.sanctions.is_empty())
            && (command == "/audit" || self.audits.is_empty())
            && (command == "/recalls" || self.recalls.is_empty())
            && (command == "/reports" || self.reports.is_empty())
            && (command == "/reportview" || self.reported.is_none())
            && (command == "/revisions" || self.revisions.is_none())
    }
    pub fn count(&self) -> usize {
        self.diagnostics.len()
            + self.logins.len()
            + self.users.len()
            + self.sanctions.len()
            + self.audits.len()
            + self.recalls.len()
            + self.reports.len()
            + self.revisions.as_ref().map_or(0, |p| p.rows.len())
    }
}
/// Only the current authorized typed page can produce a next-page request.
pub fn revision_next(panel: &Panel, owner: &str, room: &str) -> Option<(String, ReadBounds)> {
    let v = panel.moderation.as_ref()?;
    let p = v.revisions.as_ref()?;
    if panel.kind != "moderation"
        || panel.scope != room
        || v.origin_scope != room
        || v.viewer != owner
        || owner.is_empty()
        || v.command != "/revisions"
        || v.canonical != p.message_id.to_string()
        || p.next_after <= 0
        || !p.accepts(
            p.message_id,
            ReadBounds {
                after: p.after,
                until: Some(p.until),
            },
        )
        || !panel
            .actions
            .iter()
            .any(|a| a.command == "local:revisions-next")
    {
        return None;
    }
    Some((
        format!("/revisions {}", p.message_id),
        ReadBounds {
            after: p.next_after,
            until: Some(p.until),
        },
    ))
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReadIntent {
    pub command: String,
    pub requested: String,
}
impl ReadIntent {
    pub fn parse(text: &str) -> Option<Self> {
        let text = text.trim();
        let (command, arg) = text.split_once(' ').unwrap_or((text, ""));
        let command = command.to_ascii_lowercase();
        matches!(
            command.as_str(),
            "/admin"
                | "/whois"
                | "/ip"
                | "/ipusers"
                | "/logins"
                | "/bans"
                | "/audit"
                | "/recalls"
                | "/reports"
                | "/reportview"
                | "/revisions"
                | "/errors"
                | "/health"
        )
        .then(|| Self {
            command,
            requested: arg.trim().into(),
        })
    }
    pub fn form_command(&self, target: &str) -> Result<String, &'static str> {
        let target = target.trim();
        if target.is_empty() || target.chars().any(char::is_whitespace) {
            return Err("请输入一个完整用户名、#UID 或单个 IP；不会发送聊天消息。");
        }
        if !matches!(
            self.command.as_str(),
            "/whois" | "/ip" | "/logins" | "/ipusers" | "/reportview" | "/revisions"
        ) {
            return Err("此查询无需输入目标。");
        }
        if matches!(self.command.as_str(), "/reportview" | "/revisions")
            && target.parse::<i64>().map_or(true, |id| id <= 0)
        {
            return Err("请输入正整数编号。");
        }
        Ok(format!("{} {}", self.command, target))
    }
    pub fn title(&self) -> &'static str {
        match self.command.as_str() {
            "/ipusers" => "查询同 IP 账户",
            "/whois" => "查询账户详情",
            "/reportview" => "审计读取一条举报",
            "/revisions" => "授权读取消息修订",
            _ => "查询登录记录",
        }
    }
    pub fn label(&self) -> &'static str {
        if self.command == "/reportview" {
            "举报编号（不是消息编号）"
        } else if self.command == "/revisions" {
            "消息编号"
        } else if self.command == "/ipusers" {
            "单个 IP（不是 CIDR）"
        } else {
            "用户名或 #UID"
        }
    }
}
#[derive(Clone, Debug)]
pub struct ReadTicket {
    pub bounds: ReadBounds,
    pub intent: ReadIntent,
    pub owner: String,
    pub room: String,
    pub generation: u64,
    pub epoch: u64,
}
impl ReadTicket {
    pub fn unbound(text: &str) -> Option<Self> {
        ReadIntent::parse(text).map(|intent| Self {
            bounds: ReadBounds::default(),
            intent,
            owner: String::new(),
            room: String::new(),
            generation: 0,
            epoch: 0,
        })
    }
    pub fn bind(&mut self, owner: &str, room: &str, generation: u64, epoch: u64) {
        self.owner = owner.into();
        self.room = room.into();
        self.generation = generation;
        self.epoch = epoch;
    }
    pub fn accepts(
        &self,
        r: &Response,
        owner: &str,
        room: &str,
        generation: u64,
        epoch: u64,
    ) -> bool {
        if r.configuration_receipt.is_some()
            || r.management_receipt.is_some()
            || r.sanction_receipt.is_some()
            || r.report_receipt.is_some()
            || r.guess_receipt.is_some()
            || r.word_receipt.is_some()
            || r.tic_receipt.is_some()
            || r.community_receipt.is_some()
            || r.direct_target.is_some()
            || r.device_key.is_some()
            || r.key_reminder
            || !r.rooms.is_empty()
        {
            return false;
        }
        if self.owner.is_empty()
            || self.owner != owner
            || self.room != room
            || self.generation != generation
            || self.epoch != epoch
            || r.room != self.room
            || r.kind != "action"
            || r.replace
            || r.clear_view
            || r.draft.is_some()
            || r.message.is_some()
            || !r.messages.is_empty()
            || r.profile.is_some()
            || r.center.is_some()
            || r.preferences.is_some()
            || !r.form.is_empty()
            || !r.mention.is_empty()
        {
            return false;
        }
        let Some(p) = r
            .panel
            .as_ref()
            .filter(|p| private_panel_payload(p) && p.scope == self.room)
        else {
            return false;
        };
        let Some(v) = p.moderation.as_ref() else {
            return false;
        };
        if !v.payload_matches(&self.intent.command)
            || v.viewer != self.owner
            || v.command != self.intent.command
            || v.requested != self.intent.requested
            || v.origin_scope != self.room
        {
            return false;
        }
        match self.intent.command.as_str() {
            "/errors" => {
                v.window == 100
                    && v.count() <= 100
                    && v.canonical.is_empty()
                    && !v.audited
                    && p.actions.is_empty()
                    && p.lines.is_empty()
                    && v.diagnostics
                        .iter()
                        .map(|r| r.id)
                        .scan(i64::MAX, |prev, id| {
                            let ok = id > 0 && id < *prev;
                            *prev = id;
                            Some(ok)
                        })
                        .all(|ok| ok)
            }
            "/health" => {
                v.health.is_some()
                    && v.window == 0
                    && v.canonical.is_empty()
                    && !v.audited
                    && p.actions.is_empty()
                    && p.lines.is_empty()
            }
            "/bans" | "/audit" | "/recalls" | "/reports" => {
                v.window == 100 && v.count() <= 100 && v.canonical.is_empty()
            }
            "/reportview" => self.intent.requested.parse::<i64>().ok().is_some_and(|id| {
                id > 0
                    && v.canonical == id.to_string()
                    && v.audited
                    && v.reported
                        .as_ref()
                        .is_some_and(|p| p.report_id == id && p.message_id > 0)
            }),
            "/revisions" => self.intent.requested.parse::<i64>().ok().is_some_and(|id| {
                id > 0
                    && v.canonical == id.to_string()
                    && v.window == 100
                    && v.revisions
                        .as_ref()
                        .is_some_and(|p| p.accepts(id, self.bounds))
            }),

            "/admin" => {
                v.canonical.is_empty()
                    && v.profile.is_none()
                    && v.logins.is_empty()
                    && v.users.is_empty()
            }
            "/whois" => {
                !v.canonical.is_empty()
                    && v.profile
                        .as_ref()
                        .is_some_and(|p| p.viewer == self.owner && p.name == v.canonical)
                    && v.logins.is_empty()
                    && v.users.is_empty()
            }
            "/ip" | "/logins" => {
                !v.canonical.is_empty()
                    && (self.intent.requested.is_empty() && v.canonical == self.owner
                        || !self.intent.requested.is_empty())
                    && v.window == 100
                    && v.logins.len() <= 100
                    && v.profile.is_none()
                    && v.users.is_empty()
            }
            "/ipusers" => {
                !v.canonical.is_empty()
                    && v.audited
                    && v.window == 200
                    && v.users.len() <= 200
                    && v.profile.is_none()
                    && v.logins.is_empty()
            }
            _ => false,
        }
    }
}
/// Called by the real state handler, including same-room roster/role refreshes.
/// An old response cannot restore a previously-cleared private result.
pub fn invalidate(epoch: &mut u64, panel: &mut Option<Panel>) {
    *epoch = epoch.wrapping_add(1);
    if panel
        .as_ref()
        .is_some_and(|p| p.kind == "moderation" || p.moderation.is_some())
    {
        *panel = None;
    }
}
/// Explicit dismissal always advances the private-result epoch, even when the
/// visible panel is a generic menu that launched an in-flight sensitive read.
pub fn dismiss(epoch: &mut u64, panel: &mut Option<Panel>) {
    invalidate(epoch, panel);
    *panel = None;
}

#[cfg(test)]
mod tests {
    use super::*;
    fn response() -> Response {
        serde_json::from_str(r#"{"type":"action","room":"lobby","panel":{"kind":"moderation","scope":"lobby","title":"private","moderation":{"viewer":"owner","command":"/ip","requested":"","canonical":"owner","origin_scope":"lobby","window":100,"explanation":"latest100","audited":false}}}"#).unwrap()
    }
    #[test]
    fn private_ack_owner_target_scope_generation_and_same_room_epoch() {
        let mut t = ReadTicket::unbound(" /IP ").unwrap();
        t.bind("owner", "lobby", 4, 7);
        let r = response();
        assert!(t.accepts(&r, "owner", "lobby", 4, 7));
        for (owner, room, g, e) in [
            ("other", "lobby", 4, 7),
            ("owner", "dm:peer", 4, 7),
            ("owner", "lobby", 5, 7),
            ("owner", "lobby", 4, 8),
        ] {
            assert!(!t.accepts(&r, owner, room, g, e));
        }
        let mut changed = r.clone();
        changed
            .panel
            .as_mut()
            .unwrap()
            .moderation
            .as_mut()
            .unwrap()
            .requested = "other".into();
        assert!(!t.accepts(&changed, "owner", "lobby", 4, 7));
        changed = r.clone();
        changed.profile = Some(Profile::default());
        assert!(!t.accepts(&changed, "owner", "lobby", 4, 7));
        let mut panel = r.panel.clone();
        let mut epoch = 7;
        invalidate(&mut epoch, &mut panel);
        assert!(panel.is_none());
        assert!(!t.accepts(&r, "owner", "lobby", 4, epoch));
    }
    #[test]
    fn read_form_is_separate_and_bound() {
        use crate::direct_message::{DirectMessageForm, FormPurpose, FormSlot};
        let mut f = DirectMessageForm::new("owner".into(), "lobby".into(), 4);
        f.purpose = FormPurpose::Moderation(ReadIntent::parse("/whois").unwrap());
        f.target = "#42".into();
        f.body = "ordinary chat draft must not be submitted".into();
        assert_eq!(f.command("owner", "lobby", 4).unwrap(), "/whois #42");
        assert!(f.has_target() && !f.has_body());
        assert_eq!(f.initial_slot(), FormSlot::Target);
        assert!(!f.slot_enabled(FormSlot::Body));
        assert!(f.command("owner", "dm:peer", 4).is_err());
        assert!(f.command("other", "lobby", 4).is_err());
        f.begin("id".into());
        assert!(f.command("owner", "lobby", 4).is_err());
        assert!(!f.finish("old", "owner", 4, true));
        assert!(f.finish("id", "owner", 4, true));
    }
    #[test]
    fn generic_menu_dismissal_rejects_inflight_sensitive_result() {
        for _close_route in ["escape", "tab", "close-button"] {
            let mut panel = Some(Panel {
                kind: "menu".into(),
                ..Default::default()
            });
            let mut epoch = 6;
            // Actual submission invalidates old private results but retains the menu.
            invalidate(&mut epoch, &mut panel);
            assert_eq!(panel.as_ref().unwrap().kind, "menu");
            let mut ticket = ReadTicket::unbound("/ip").unwrap();
            ticket.bind("owner", "lobby", 4, epoch);
            dismiss(&mut epoch, &mut panel);
            assert!(panel.is_none());
            assert!(!ticket.accepts(&response(), "owner", "lobby", 4, epoch));
        }
    }
    #[test]
    fn revision_bounds_zero_next_and_private_payload_do_not_cross_routes() {
        let mut request = crate::session::Request::simple("command", "empty");
        request.read_until = Some(0);
        let json = serde_json::to_string(&request).unwrap();
        assert!(json.contains("\"read_until\":0"));
        request.read_until = None;
        assert!(
            !serde_json::to_string(&request)
                .unwrap()
                .contains("read_until")
        );
        let page = RevisionPage {
            message_id: 42,
            after: 0,
            until: 300,
            next_after: 100,
            rows: vec![
                RevisionRow {
                    id: 10,
                    ..Default::default()
                },
                RevisionRow {
                    id: 100,
                    ..Default::default()
                },
            ],
        };
        assert!(page.accepts(42, ReadBounds::default()));
        assert!(!page.accepts(43, ReadBounds::default()));
        assert!(!page.accepts(
            42,
            ReadBounds {
                after: 0,
                until: Some(0)
            }
        ));
        let mut panel = Panel {
            kind: "moderation".into(),
            scope: "lobby".into(),
            moderation: Some(ModerationView {
                viewer: "owner".into(),
                origin_scope: "lobby".into(),
                command: "/revisions".into(),
                requested: "42".into(),
                canonical: "42".into(),
                window: 100,
                revisions: Some(page),
                ..Default::default()
            }),
            actions: vec![crate::session::PanelAction {
                command: "local:revisions-next".into(),
                ..Default::default()
            }],
            ..Default::default()
        };
        let (command, bounds) = revision_next(&panel, "owner", "lobby").unwrap();
        assert_eq!(command, "/revisions 42");
        assert_eq!(
            bounds,
            ReadBounds {
                after: 100,
                until: Some(300)
            }
        );
        assert!(revision_next(&panel, "other", "lobby").is_none());
        assert!(revision_next(&panel, "owner", "dm:peer").is_none());
        let mut ticket = ReadTicket::unbound("/revisions 42").unwrap();
        ticket.bind("owner", "lobby", 4, 7);
        let mut response = Response {
            kind: "action".into(),
            room: "lobby".into(),
            panel: Some(panel.clone()),
            ..Default::default()
        };
        assert!(ticket.accepts(&response, "owner", "lobby", 4, 7));
        ticket.bounds = bounds;
        assert!(!ticket.accepts(&response, "owner", "lobby", 4, 7));
        panel.moderation.as_mut().unwrap().reported = Some(ReportedMessage {
            report_id: 4,
            message_id: 42,
            body: "wrong-route-body".into(),
            ..Default::default()
        });
        response.panel = Some(panel);
        ticket.bounds = ReadBounds::default();
        assert!(!ticket.accepts(&response, "owner", "lobby", 4, 7));
    }
    #[test]
    fn report_id_is_not_message_id_and_does_not_grant_revision_ack() {
        let mut ticket = ReadTicket::unbound("/reportview 8001").unwrap();
        ticket.bind("owner", "lobby", 4, 7);
        let mut r = Response {
            kind: "action".into(),
            room: "lobby".into(),
            panel: Some(Panel {
                kind: "moderation".into(),
                scope: "lobby".into(),
                moderation: Some(ModerationView {
                    viewer: "owner".into(),
                    origin_scope: "lobby".into(),
                    command: "/reportview".into(),
                    requested: "8001".into(),
                    canonical: "8001".into(),
                    audited: true,
                    reported: Some(ReportedMessage {
                        report_id: 8001,
                        message_id: 42,
                        sender: "peer".into(),
                        body: "single current body".into(),
                    }),
                    ..Default::default()
                }),
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(ticket.accepts(&r, "owner", "lobby", 4, 7));
        r.panel
            .as_mut()
            .unwrap()
            .moderation
            .as_mut()
            .unwrap()
            .reported
            .as_mut()
            .unwrap()
            .report_id = 42;
        assert!(!ticket.accepts(&r, "owner", "lobby", 4, 7));
    }
    #[test]
    fn replacing_a_menu_or_input_flow_invalidates_pending_private_query() {
        // State composition plus static wiring checks cover entrypoints; this is
        // intentionally not a claim that GUI mouse/key events were dispatched.
        let mut epoch = 20;
        let mut panel = Some(Panel {
            kind: "menu".into(),
            ..Default::default()
        });
        let mut ticket = ReadTicket::unbound("/ip").unwrap();
        ticket.bind("owner", "lobby", 4, epoch);
        dismiss(&mut epoch, &mut panel); // begin_feature_navigation delegates here.
        panel = Some(Panel {
            title: "new message menu or recovery view".into(),
            ..Default::default()
        });
        assert!(panel.is_some());
        assert!(!ticket.accepts(&response(), "owner", "lobby", 4, epoch));
    }

    #[test]

    fn private_ticket_rejects_public_sibling() {
        let r: Response = serde_json::from_str(r#"{"type":"action","room":"lobby","panel":{"kind":"moderation","scope":"lobby","title":"mixed","server_time":"unrelated payload","moderation":{"viewer":"owner","command":"/admin","requested":"","canonical":"","origin_scope":"lobby","window":0,"explanation":"private","audited":false}}}"#).unwrap();
        let mut t = ReadTicket::unbound("/admin").unwrap();
        t.bind("owner", "lobby", 1, 1);
        assert!(
            !t.accepts(&r, "owner", "lobby", 1, 1),
            "PRIVATE_ACCEPTED_PUBLIC_SIBLING"
        );
    }

    #[test]
    fn operations_reads_are_private_exclusive_ordered_and_debug_redacted() {
        for command in ["/errors", "/health"] {
            let mut t = ReadTicket::unbound(command).unwrap();
            t.bind("owner", "lobby", 4, 7);
            let mut r = response();
            let p = r.panel.as_mut().unwrap();
            let v = p.moderation.as_mut().unwrap();
            v.command = command.into();
            v.canonical.clear();
            v.logins.clear();
            if command == "/errors" {
                v.diagnostics = vec![
                    DiagnosticRow {
                        id: 2,
                        kind: "synthetic-kind".into(),
                        detail: "synthetic-private-detail".into(),
                        ..Default::default()
                    },
                    DiagnosticRow {
                        id: 1,
                        ..Default::default()
                    },
                ];
                let debug = format!("{v:?}");
                assert!(
                    !debug.contains("synthetic-private-detail")
                        && !debug.contains("synthetic-kind")
                );
            } else {
                v.window = 0;
                v.health = Some(Default::default());
            }
            assert!(private_panel_payload(r.panel.as_ref().unwrap()));
            assert!(t.accepts(&r, "owner", "lobby", 4, 7));
            for (owner, room, g, e) in [
                ("other", "lobby", 4, 7),
                ("owner", "dm:bob", 4, 7),
                ("owner", "lobby", 5, 7),
                ("owner", "lobby", 4, 8),
            ] {
                assert!(!t.accepts(&r, owner, room, g, e));
            }
            let mut mixed = r.clone();
            mixed.panel.as_mut().unwrap().statistics = Some(Default::default());
            assert!(!private_panel_payload(mixed.panel.as_ref().unwrap()));
            assert!(!t.accepts(&mixed, "owner", "lobby", 4, 7));
            let mut mixed = r.clone();
            mixed
                .panel
                .as_mut()
                .unwrap()
                .actions
                .push(crate::session::PanelAction::default());
            assert!(!t.accepts(&mixed, "owner", "lobby", 4, 7));
            let mut mixed = r.clone();
            let v = mixed.panel.as_mut().unwrap().moderation.as_mut().unwrap();
            if command == "/errors" {
                v.diagnostics.reverse();
            } else {
                v.diagnostics.push(DiagnosticRow {
                    id: 1,
                    ..Default::default()
                });
            }
            assert!(!t.accepts(&mixed, "owner", "lobby", 4, 7));
            let mut epoch = 7;
            let mut panel = r.panel.clone();
            dismiss(&mut epoch, &mut panel);
            assert!(panel.is_none());
            assert!(!t.accepts(&r, "owner", "lobby", 4, epoch));
        }
        let mut failure: Response = serde_json::from_str(
            r#"{"type":"error","request_id":"current","error":"synthetic denied"}"#,
        )
        .unwrap();
        assert!(!requires_private_ticket(&failure, true));
        for field in ["profile", "center", "preferences"] {
            let mut mixed = failure.clone();
            match field {
                "profile" => mixed.profile = Some(Default::default()),
                "center" => mixed.center = Some(Default::default()),
                _ => mixed.preferences = Some(Default::default()),
            }
            assert!(requires_private_ticket(&mixed, true));
            mixed.kind = "state".into();
            assert!(requires_private_ticket(&mixed, true));
        }
        failure.kind = "state".into();
        assert!(requires_private_ticket(&failure, true));
        let mut old = ReadTicket::unbound("/ip").unwrap();
        old.bind("owner", "lobby", 4, 7);
        let mut r = response();
        r.panel
            .as_mut()
            .unwrap()
            .moderation
            .as_mut()
            .unwrap()
            .health = Some(Default::default());
        assert!(!old.accepts(&r, "owner", "lobby", 4, 7));
    }
}
