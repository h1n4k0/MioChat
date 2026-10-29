use crate::connection::{ConnectionSpec, ssh_args};
use anyhow::{Context as _, Result};
use crossbeam_channel::{Receiver, Sender, bounded};
use serde::{Deserialize, Serialize};
use std::io::{BufRead, BufReader, Read, Write};
use std::path::Path;
use std::process::{Child, Command, Stdio};
use std::thread;

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq)]
pub struct Room {
    pub name: String,
    pub topic: String,
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq)]
pub struct Message {
    pub attachment: Option<crate::attachments::Info>,
    pub id: i64,
    pub room: String,
    pub sender: String,
    #[serde(default)]
    pub recipient: String,
    pub body: String,
    pub kind: String,
    pub at: i64,
    #[serde(default)]
    pub edited: i64,
    #[serde(default)]
    pub recalled: i64,
    #[serde(default)]
    pub reply_to: i64,
    #[serde(default)]
    pub reply_name: String,
    #[serde(default)]
    pub reply_body: String,
    #[serde(default)]
    pub reactions: String,
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct PanelAction {
    pub label: String,
    pub command: String,
    #[serde(default)]
    pub fill: bool,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct Panel {
    pub moderation: Option<crate::moderation::ModerationView>,
    pub community: Option<crate::community::CommunityView>,
    pub statistics: Option<crate::information::Statistics>,
    #[serde(default)]
    pub server_time: String,
    #[serde(default)]
    pub kind: String,
    #[serde(default)]
    pub scope: String,
    #[serde(default)]
    pub rooms: Vec<RoomCard>,
    #[serde(default)]
    pub members: Vec<RoomMember>,
    pub details: Option<RoomCard>,
    pub title: String,
    #[serde(default)]
    pub lines: Vec<String>,
    #[serde(default)]
    pub actions: Vec<PanelAction>,
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct RoomCard {
    #[serde(default)]
    pub can_own: bool,
    #[serde(default)]
    pub can_assign_mods: bool,
    #[serde(default)]
    pub preferences: crate::preferences::RoomPreferences,
    pub name: String,
    pub topic: String,
    pub owner: String,
    pub category: String,
    pub rules: String,
    pub announcement: String,
    pub private: bool,
    pub read_only: bool,
    pub archived: bool,
    pub slow: u32,
    pub can_manage: bool,
    pub can_leave: bool,
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct RoomMember {
    #[serde(default)]
    pub can_mod: bool,
    #[serde(default)]
    pub can_unmod: bool,
    pub name: String,
    pub role: String,
    pub can_remove: bool,
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct Draft {
    pub room: String,
    pub body: String,
    pub version: i64,
    #[serde(default)]
    pub reply_to: i64,
    #[serde(default)]
    pub conflict: bool,
    #[serde(default)]
    pub backup_id: String,
    #[serde(default)]
    pub position: crate::persistence::Position,
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct DeviceKey {
    pub algorithm: String,
    pub fingerprint: String,
    pub bound: bool,
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct DirectTarget {
    pub requested: String,
    pub canonical: String,
}

#[derive(Clone, Debug, Default, Deserialize)]
pub struct Response {
    pub attachment: Option<crate::attachments::Receipt>,
    pub dot: Option<crate::dot::Receipt>,
    pub completion: Option<crate::completion::Receipt>,
    pub secret_admin: Option<crate::secret_admin::Receipt>,
    pub maintenance: Option<crate::maintenance::Receipt>,
    pub composition: Option<crate::composition::Receipt>,
    pub configuration_receipt: Option<crate::configuration::ConfigurationReceipt>,
    pub management_receipt: Option<crate::management::ManagementReceipt>,
    pub sanction_receipt: Option<crate::sanctions::SanctionReceipt>,
    pub report_receipt: Option<crate::report::ReportReceipt>,
    pub guess_receipt: Option<crate::guess::GuessReceipt>,
    #[serde(default)]
    pub word_receipt: Option<crate::word::WordReceipt>,
    pub tic_receipt: Option<crate::tic::TicReceipt>,
    pub community_receipt: Option<crate::community_mutation::CommunityReceipt>,
    #[serde(default)]
    pub direct_target: Option<DirectTarget>,
    #[serde(default)]
    pub clear_view: bool,
    #[serde(default)]
    pub mention: String,
    pub profile: Option<crate::profile::Profile>,
    pub center: Option<crate::center::Center>,
    pub preferences: Option<crate::preferences::Preferences>,
    #[serde(default)]
    pub key_reminder: bool,
    pub device_key: Option<DeviceKey>,
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default)]
    pub request_id: String,
    #[serde(default)]
    pub protocol: u32,
    #[serde(default)]
    pub registration_mode: String,
    #[serde(default)]
    pub user: String,
    #[serde(default)]
    pub room: String,
    #[serde(default)]
    pub rooms: Vec<Room>,
    #[serde(default)]
    pub messages: Vec<Message>,
    pub message: Option<Message>,
    #[serde(default)]
    pub online: usize,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub error: String,
    pub panel: Option<Panel>,
    pub draft: Option<Draft>,
    #[serde(default)]
    pub form: String,
    #[serde(default)]
    pub reply_to: i64,
    #[serde(default)]
    pub replace: bool,
    #[serde(default)]
    pub at_latest: bool,
    #[serde(default)]
    pub draft_changed: bool,
    #[serde(default)]
    pub capabilities: Vec<String>,
}

#[derive(Debug)]
pub enum SessionEvent {
    Response {
        generation: u64,
        response: Box<Response>,
    },
    Error {
        generation: u64,
        message: String,
    },
    Ended {
        generation: u64,
    },
}

#[derive(Serialize)]
pub struct Request<'a> {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub attachment: Option<&'a crate::attachments::Data>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub dot: Option<&'a crate::dot::Data>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub completion: Option<&'a crate::completion::Data>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub secret_admin: Option<&'a crate::secret_admin::Data>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub maintenance: Option<&'a crate::maintenance::MaintenanceRequest>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub composition: Option<&'a crate::composition::CompositionRequest>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub self_ip_force: Option<bool>,
    #[serde(skip_serializing_if = "is_zero")]
    pub read_after: i64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub read_until: Option<i64>,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub word_mode: &'a str,
    #[serde(skip_serializing_if = "is_zero")]
    pub game_id: i64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tic_version: Option<i64>,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub expected_room: &'a str,
    #[serde(rename = "type")]
    pub kind: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub request_id: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub username: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub password: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub new_password: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub confirm: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub invite: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub room: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub body: &'a str,
    #[serde(skip_serializing_if = "is_zero")]
    pub before: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub reply_to: i64,
    #[serde(skip_serializing_if = "is_zero")]
    pub version: i64,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub backup_id: &'a str,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub position: Option<&'a crate::persistence::Position>,
}

fn is_zero(value: &i64) -> bool {
    *value == 0
}

impl<'a> Request<'a> {
    pub fn simple(kind: &'a str, request_id: &'a str) -> Self {
        Self {
            attachment: None,
            dot: None,
            completion: None,
            secret_admin: None,
            maintenance: None,
            composition: None,
            self_ip_force: None,
            read_after: 0,
            read_until: None,
            word_mode: "",
            kind,
            expected_room: "",
            tic_version: None,
            game_id: 0,
            request_id,
            username: "",
            password: "",
            new_password: "",
            confirm: "",
            invite: "",
            room: "",
            body: "",
            before: 0,
            reply_to: 0,
            version: 0,
            backup_id: "",
            position: None,
        }
    }
}

pub struct Session {
    generation: u64,
    writer: Sender<Vec<u8>>,
    child: Option<Child>,
    events: Receiver<SessionEvent>,
}

impl Session {
    pub fn connect_identity(
        spec: &ConnectionSpec,
        known_hosts: &Path,
        identity: &Path,
        passphrase: &str,
        bridge: &Path,
        generation: u64,
    ) -> Result<Self> {
        spec.validate()?;
        anyhow::ensure!(
            bridge.is_file(),
            "device-key SSH bridge is not installed beside the client"
        );
        anyhow::ensure!(
            passphrase.len() <= 4096,
            "device-key passphrase is too long"
        );
        #[derive(Serialize)]
        struct Init<'a> {
            #[serde(rename = "type")]
            kind: &'a str,
            host: &'a str,
            port: u16,
            ssh_user: &'a str,
            known_hosts: &'a Path,
            identity: &'a Path,
            passphrase: &'a str,
        }
        let mut bytes = serde_json::to_vec(&Init {
            kind: "device-key-transport-v1",
            host: &spec.host,
            port: spec.port,
            ssh_user: &spec.ssh_user,
            known_hosts,
            identity,
            passphrase,
        })?;
        bytes.push(b'\n');
        let mut command = Command::new(bridge);
        command
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt;
            command.creation_flags(0x08000000);
        }
        let mut child = command
            .spawn()
            .context("cannot start device-key SSH bridge")?;
        let result = child
            .stdin
            .as_mut()
            .context("bridge stdin unavailable")
            .and_then(|stdin| {
                stdin
                    .write_all(&bytes)
                    .and_then(|_| stdin.flush())
                    .context("cannot initialize device-key transport")
            });
        bytes.fill(0);
        if let Err(error) = result {
            let _ = child.kill();
            let _ = child.wait();
            return Err(error);
        }
        Self::from_child(child, generation)
    }
    pub fn send_persistence(&self, op: &crate::persistence::Operation, id: &str) -> Result<()> {
        if !op.safe_body() {
            anyhow::bail!("{}", crate::word::RESERVED_NOTICE);
        }
        use crate::persistence::Operation;
        let mut request = Request::simple(
            match op {
                Operation::Load { .. } => "draft_load",
                Operation::Save { keep: true, .. } => "draft_keep",
                Operation::Save { .. } => "draft_save",
                Operation::Suspend { .. } => "draft_suspend",
            },
            id,
        );
        request.room = op.room();
        match op {
            Operation::Load { .. } => {}
            Operation::Save {
                version,
                snapshot,
                backup_id,
                ..
            } => {
                request.version = *version;
                request.body = &snapshot.body;
                request.reply_to = snapshot.reply_to;
                request.position = Some(&snapshot.position);
                request.backup_id = backup_id;
            }
            Operation::Suspend { snapshot, .. } => {
                request.body = &snapshot.body;
                request.reply_to = snapshot.reply_to;
            }
        }
        self.send(&request)
    }
    pub fn connect(spec: &ConnectionSpec, known_hosts: &Path, generation: u64) -> Result<Self> {
        let args = ssh_args(spec, known_hosts)?;
        let child = Command::new("ssh")
            .args(args)
            .env("SSH_ASKPASS_REQUIRE", "never")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .context("cannot start OpenSSH; install ssh, ssh-keyscan, and ssh-keygen")?;
        Self::from_child(child, generation)
    }

    /// Explicit local integration harness; no host, SSH files, sockets or
    /// persistent account material. Only enabled by --fixture-driver in the UI.
    pub fn fixture(driver: &Path, generation: u64) -> Result<Self> {
        anyhow::ensure!(driver.is_file(), "fixture driver is not a file");
        let mut command = Command::new(driver);
        command
            .arg("-test.run=^TestGUIFixtureDriver$")
            .env("MIOCHAT_LOCAL_FIXTURE", "synthetic-pipe-v1")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt;
            command.creation_flags(0x08000000);
        }
        let child = command
            .spawn()
            .context("cannot start explicit synthetic fixture")?;
        Self::from_child(child, generation)
    }

    fn from_child(mut child: Child, generation: u64) -> Result<Self> {
        let mut stdin = child.stdin.take().context("SSH stdin unavailable")?;
        let stdout = child.stdout.take().context("SSH stdout unavailable")?;
        let stderr = child.stderr.take().context("SSH stderr unavailable")?;
        let (tx, rx) = bounded(256);
        let (writer_tx, writer_rx) = bounded::<Vec<u8>>(32);
        let writer_events = tx.clone();
        thread::Builder::new()
            .name("miochat-json-writer".into())
            .spawn(move || {
                while let Ok(bytes) = writer_rx.recv() {
                    if let Err(error) = stdin.write_all(&bytes).and_then(|_| stdin.flush()) {
                        let _ = writer_events.send(SessionEvent::Error {
                            generation,
                            message: format!("SSH write failed: {error}"),
                        });
                        return;
                    }
                }
            })?;
        let reader_tx = tx.clone();
        thread::Builder::new()
            .name("miochat-json-reader".into())
            .spawn(move || read_responses(stdout, generation, reader_tx))?;
        thread::Builder::new()
            .name("miochat-ssh-errors".into())
            .spawn(move || {
                let mut reader = BufReader::new(stderr);
                loop {
                    let mut bytes = Vec::new();
                    match reader
                        .by_ref()
                        .take(16 * 1024 + 1)
                        .read_until(b'\n', &mut bytes)
                    {
                        Ok(0) => break,
                        Ok(_) if bytes.len() <= 16 * 1024 => {
                            let line = String::from_utf8_lossy(&bytes);
                            if line.trim().is_empty() {
                                continue;
                            }
                            let _ = tx.send(SessionEvent::Error {
                                generation,
                                message: line.trim().to_string(),
                            });
                        }
                        _ => {
                            let _ = tx.send(SessionEvent::Error {
                                generation,
                                message: "SSH error output exceeded 16 KiB".into(),
                            });
                            break;
                        }
                    }
                }
            })?;
        Ok(Self {
            generation,
            writer: writer_tx,
            child: Some(child),
            events: rx,
        })
    }

    /// Disposable in-process protocol fixture. Never opens a socket or stores credentials.
    pub fn mock(generation: u64) -> Self {
        let (tx, rx) = bounded(256);
        let (writer, requests) = bounded::<Vec<u8>>(32);
        std::thread::spawn(move || {
            let emit = |response: Response| {
                tx.send(SessionEvent::Response {
                    generation,
                    response: Box::new(response),
                })
                .is_ok()
            };
            emit(Response {
                kind: "hello".into(),
                protocol: 1,
                registration_mode: "open".into(),
                ..Default::default()
            });
            let mut user = String::new();
            let mut room = "lobby".to_string();
            let mut messages = vec![
                Message {
                    id: 1,
                    room: "lobby".into(),
                    sender: "体验向导".into(),
                    body: "这是本地模拟环境，不会连接真实聊天室。".into(),
                    kind: "chat".into(),
                    ..Default::default()
                },
                Message {
                    id: 2,
                    room: "lobby".into(),
                    sender: "体验向导".into(),
                    body: "可切换三种样式。通知默认隐藏正文；后台新消息会进入 Windows 通知中心。"
                        .into(),
                    kind: "chat".into(),
                    ..Default::default()
                },
            ];
            let mut next_id = 3;
            let mut dev_allowed = true;
            while let Ok(bytes) = requests.recv() {
                let Ok(request) = serde_json::from_slice::<serde_json::Value>(&bytes) else {
                    continue;
                };
                let get = |key: &str| request[key].as_str().unwrap_or("").to_string();
                let kind = get("type");
                let request_id = get("request_id");
                let state =
                    |room: &str, user: &str, messages: &[Message], id: &str, dev_allowed: bool| {
                        Response {
                            kind: "state".into(),
                            request_id: id.into(),
                            user: user.into(),
                            room: room.into(),
                            online: 2,
                            rooms: if dev_allowed {
                                vec![
                                    Room {
                                        name: "lobby".into(),
                                        topic: "本地大厅".into(),
                                    },
                                    Room {
                                        name: "dev".into(),
                                        topic: "本地研发".into(),
                                    },
                                ]
                            } else {
                                vec![Room {
                                    name: "lobby".into(),
                                    topic: "本地大厅".into(),
                                }]
                            },
                            messages: messages
                                .iter()
                                .filter(|m| m.room == room)
                                .cloned()
                                .collect(),
                            ..Default::default()
                        }
                    };
                match kind.as_str() {
                    "login" | "register" => {
                        user = get("username");
                        if user.is_empty() {
                            user = "本地访客".into();
                        }
                        emit(Response {
                            kind: "auth".into(),
                            request_id,
                            user: user.clone(),
                            ..Default::default()
                        });
                        emit(state(&room, &user, &messages, "", dev_allowed));
                    }
                    "select_room" => {
                        let requested = get("room");
                        if requested != "lobby" && (requested != "dev" || !dev_allowed) {
                            emit(Response {
                                kind: "error".into(),
                                request_id,
                                error: "模拟房间权限不足".into(),
                                ..Default::default()
                            });
                            continue;
                        }
                        room = requested;
                        emit(state(&room, &user, &messages, &request_id, dev_allowed));
                    }
                    "sync" => {
                        emit(state(&room, &user, &messages, &request_id, dev_allowed));
                    }
                    "send_message" | "mock_incoming" | "mock_delayed" => {
                        let body = get("body");
                        if kind == "send_message" && (body == "/fail" || get("room") != room) {
                            emit(Response {
                                kind: "error".into(),
                                request_id,
                                error: "模拟发送被拒绝；草稿已保留。".into(),
                                ..Default::default()
                            });
                            continue;
                        }
                        if kind == "mock_delayed" {
                            std::thread::sleep(std::time::Duration::from_secs(5));
                        }
                        let incoming = kind != "send_message";
                        let message = Message {
                            id: next_id,
                            room: room.clone(),
                            sender: if incoming {
                                "测试同伴".into()
                            } else {
                                user.clone()
                            },
                            body: if incoming {
                                "这是一条本地模拟新消息，未连接真实服务器。".into()
                            } else {
                                body
                            },
                            kind: "chat".into(),
                            at: next_id,
                            ..Default::default()
                        };
                        next_id += 1;
                        messages.push(message.clone());
                        if !incoming {
                            emit(Response {
                                kind: "sent".into(),
                                request_id,
                                message: Some(message.clone()),
                                ..Default::default()
                            });
                        }
                        if !emit(Response {
                            kind: "message".into(),
                            message: Some(message),
                            ..Default::default()
                        }) {
                            break;
                        }
                    }
                    "mock_edit" | "mock_recall" => {
                        if let Some(message) = messages.iter_mut().rev().find(|m| m.room == room) {
                            if kind == "mock_edit" {
                                message.body = "这条本地消息已编辑。".into();
                                message.edited = 1;
                            } else {
                                message.body.clear();
                                message.recalled = 1;
                            }
                            emit(Response {
                                kind: "message_updated".into(),
                                message: Some(message.clone()),
                                ..Default::default()
                            });
                        }
                    }
                    "mock_acl" => {
                        dev_allowed = false;
                        room = "lobby".into();
                        emit(state(&room, &user, &messages, "", false));
                    }
                    "command" => {
                        emit(Response { kind: "error".into(), request_id,
                            error: "此内存外观预览不模拟服务端功能；真实动作已在独立本地 Store/SSH 测试中验证。".into(), ..Default::default() });
                    }
                    "mock_drop" => {
                        let _ = tx.send(SessionEvent::Ended { generation });
                        break;
                    }
                    "disconnect" => {
                        break;
                    }
                    _ => {}
                }
            }
        });
        Self {
            generation,
            writer,
            child: None,
            events: rx,
        }
    }

    pub fn generation(&self) -> u64 {
        self.generation
    }

    pub fn events(&self) -> &Receiver<SessionEvent> {
        &self.events
    }

    pub fn send(&self, request: &Request<'_>) -> Result<()> {
        let mut bytes = serde_json::to_vec(request)?;
        bytes.push(b'\n');
        if bytes.len() > 64 * 1024 {
            anyhow::bail!("Request exceeds 64 KiB");
        }
        self.writer
            .try_send(bytes)
            .context("SSH request queue is full or closed")
    }

    pub fn disconnect(&mut self) {
        let _ = self
            .writer
            .try_send(b"{\"type\":\"disconnect\"}\n".to_vec());
        if let Some(child) = self.child.as_mut() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}

impl Drop for Session {
    fn drop(&mut self) {
        self.disconnect();
    }
}

const MAX_SERVER_FRAME: usize = 1024 * 1024;

fn read_responses(stdout: impl std::io::Read, generation: u64, tx: Sender<SessionEvent>) {
    let mut reader = BufReader::new(stdout);
    loop {
        let mut bytes = Vec::new();
        let read = match reader
            .by_ref()
            .take((MAX_SERVER_FRAME + 1) as u64)
            .read_until(b'\n', &mut bytes)
        {
            Ok(read) => read,
            Err(error) => {
                let _ = tx.send(SessionEvent::Error {
                    generation,
                    message: error.to_string(),
                });
                return;
            }
        };
        if read == 0 {
            break;
        }
        if bytes.len() > MAX_SERVER_FRAME || !bytes.ends_with(b"\n") {
            let _ = tx.send(SessionEvent::Error {
                generation,
                message: "Server frame exceeded 1 MiB".into(),
            });
            return;
        }
        match std::str::from_utf8(&bytes)
            .ok()
            .and_then(|line| serde_json::from_str::<Response>(line).ok())
        {
            Some(response) => {
                if tx
                    .send(SessionEvent::Response {
                        generation,
                        response: Box::new(response),
                    })
                    .is_err()
                {
                    return;
                }
            }
            None => {
                let _ = tx.send(SessionEvent::Error {
                    generation,
                    message: "Invalid server JSON frame".into(),
                });
                return;
            }
        }
    }
    let _ = tx.send(SessionEvent::Ended { generation });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn request_omits_secrets_when_unused() {
        let text = serde_json::to_string(&Request::simple("sync", "r1")).unwrap();
        assert_eq!(text, r#"{"type":"sync","request_id":"r1"}"#);
    }

    #[test]
    fn parses_versioned_hello() {
        let response: Response = serde_json::from_str(
            r#"{"type":"hello","protocol":1,"status":"authentication_required"}"#,
        )
        .unwrap();
        assert_eq!(response.kind, "hello");
        assert_eq!(response.protocol, 1);
    }

    #[test]
    fn native_action_and_quoted_dm_frames_preserve_semantics() {
        let response: Response = serde_json::from_str(r#"{"type":"action","request_id":"a1","room":"dm:bob","reply_to":42,"replace":true,"messages":[{"id":43,"room":"dm:bob","sender":"alice","recipient":"bob","body":"quoted","kind":"chat","at":1,"reply_to":42,"reply_name":"bob","reply_body":"source","reactions":"heart:1"}],"panel":{"title":"Search","actions":[{"label":"Result","command":"/jump 43"}]}}"#).unwrap();
        assert_eq!(response.request_id, "a1");
        assert_eq!(response.messages[0].reply_body, "source");
        assert_eq!(response.messages[0].reactions, "heart:1");
        assert_eq!(response.panel.unwrap().actions[0].command, "/jump 43");
        let mut request = Request::simple("command", "a2");
        request.room = "dm:bob";
        request.body = "/older";
        request.before = 43;
        let value = serde_json::to_value(request).unwrap();
        assert_eq!(value["before"], 43);
        assert!(value.get("password").is_none());
        assert!(value.get("reply_to").is_none());
    }

    #[test]
    fn reset_and_draft_wire_payloads_remain_distinct_from_chat() {
        let reset: Response = serde_json::from_str(r#"{"type":"auth","request_id":"a","user":"fixture","status":"password_reset_required"}"#).unwrap();
        assert!(reset.rooms.is_empty());
        assert!(reset.messages.is_empty());
        let conflict: Response = serde_json::from_str(r#"{"type":"draft_conflict","request_id":"d","room":"dm:peer","draft":{"room":"dm:peer","body":"unsent fixture","version":7,"reply_to":4,"conflict":true,"backup_id":"fixture-session"}}"#).unwrap();
        let draft = conflict.draft.unwrap();
        assert!(draft.conflict);
        assert_eq!(draft.version, 7);
        assert_eq!(draft.backup_id, "fixture-session");
        let mut request = Request::simple("draft_keep", "keep");
        request.room = "dm:peer";
        request.version = draft.version;
        request.backup_id = &draft.backup_id;
        let value = serde_json::to_value(request).unwrap();
        assert_eq!(value["version"], 7);
        for field in ["password", "new_password", "confirm", "invite"] {
            assert!(value.get(field).is_none());
        }
    }
}
