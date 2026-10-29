//! One-time generated administrative secrets. No persistence, retry, recovery,
//! or generic panel projection. String clearing is not secure memory erasure.
use crate::{composition::Origin, session::Response};
use serde::{Deserialize, Serialize};
use std::fmt;

pub const UNKNOWN: &str = "此前管理凭据操作可能已经提交；不会自动重试，请先在原服务器核验。";
pub const COMMITTED: &str =
    "此前管理凭据操作已经提交；一次性结果已清除或不可显示，不会自动重新生成。";
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Kind {
    Resetpass,
    Invitecode,
}
impl Kind {
    pub fn from_command(command: &str) -> Option<Self> {
        match command {
            "/resetpass" => Some(Self::Resetpass),
            "/invitecode" => Some(Self::Invitecode),
            _ => None,
        }
    }
    pub fn title(self) -> &'static str {
        match self {
            Self::Resetpass => "确认重置临时密码",
            Self::Invitecode => "确认生成一次性邀请码",
        }
    }
    pub fn explanation(self) -> &'static str {
        match self {
            Self::Resetpass => {
                "确认后将替换目标密码、撤销其原有会话与设备密钥，并要求下次登录改密。仅显示一次，不会自动重试。"
            }
            Self::Invitecode => {
                "确认后按指定期限与次数创建邀请码；仅显示一次，不会自动发送或分发。"
            }
        }
    }
    pub fn data(self, target: &str, body: &str) -> Result<Data, &'static str> {
        let target = target.trim();
        if target.is_empty() || target.len() > 256 || target.chars().any(char::is_whitespace) {
            return Err("请明确输入一个账户或期限，不含空白。");
        }
        match self {
            Self::Resetpass if body.is_empty() => Ok(Data {
                kind: self,
                target: target.into(),
                duration: String::new(),
                uses: 0,
            }),
            Self::Resetpass => Err("重置密码不能附带其它输入。"),
            Self::Invitecode => {
                let uses = body.trim().parse::<u32>().map_err(|_| "使用次数为1–100")?;
                if !(1..=100).contains(&uses) || target.len() > 64 {
                    return Err("使用次数为1–100，期限最多64字节");
                }
                Ok(Data {
                    kind: self,
                    target: String::new(),
                    duration: target.into(),
                    uses,
                })
            }
        }
    }
    pub fn command(self, target: &str, body: &str) -> Result<String, &'static str> {
        let data = self.data(target, body)?;
        Ok(match self {
            Self::Resetpass => format!("/resetpass {}", data.target),
            Self::Invitecode => format!("/invitecode {} {}", data.duration, data.uses),
        })
    }
}
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct Data {
    pub kind: Kind,
    pub target: String,
    pub duration: String,
    pub uses: u32,
}
#[derive(Clone, Deserialize)]
pub struct Receipt {
    pub kind: Kind,
    pub requested_target: String,
    pub target: String,
    pub duration: String,
    pub uses: u32,
    pub expires: i64,
    pub state: String,
    pub committed: Option<bool>,
    #[serde(default)]
    pub value: String,
}
impl fmt::Debug for Receipt {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("SecretAdminReceipt")
            .field("kind", &self.kind)
            .field("state", &self.state)
            .field("committed", &self.committed)
            .finish_non_exhaustive()
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum State {
    Pending,
    Shown,
    Hidden,
    Unknown,
    Rejected,
}
pub struct Binding {
    pub owner: String,
    pub room: String,
    pub generation: u64,
    pub origin: Origin,
    pub epoch: u64,
}
pub struct Attempt {
    pub data: Data,
    pub id: String,
    pub owner: String,
    pub room: String,
    pub generation: u64,
    pub origin: Origin,
    pub epoch: u64,
    pub started: u64,
    pub completed: Option<u64>,
    pub state: State,
    pub committed: Option<bool>,
    value: String,
    pub revealed: bool,
    pub canonical_target: String,
    pub expires: i64,
    pub opening_input: Option<String>,
}
impl Attempt {
    pub fn new(
        data: Data,
        id: String,
        binding: Binding,
        started: u64,
        opening_input: Option<String>,
    ) -> Self {
        let Binding {
            owner,
            room,
            generation,
            origin,
            epoch,
        } = binding;
        Self {
            data,
            id,
            owner,
            room,
            generation,
            origin,
            epoch,
            started,
            completed: None,
            state: State::Pending,
            committed: None,
            value: String::new(),
            revealed: false,
            canonical_target: String::new(),
            expires: 0,
            opening_input,
        }
    }
    pub fn bound(
        &self,
        owner: &str,
        room: &str,
        generation: u64,
        origin: Option<&Origin>,
        epoch: u64,
    ) -> bool {
        self.owner == owner
            && self.room == room
            && self.generation == generation
            && origin == Some(&self.origin)
            && self.epoch == epoch
    }
    pub fn notice(&self) -> Option<&'static str> {
        if self.committed == Some(true) {
            Some(COMMITTED)
        } else if self.state != State::Rejected {
            Some(UNKNOWN)
        } else {
            None
        }
    }
    pub fn visible(&self, now: u64) -> bool {
        self.state == State::Shown
            && self
                .completed
                .is_some_and(|at| now.saturating_sub(at) < 60_000)
            && !self.value.is_empty()
    }
    pub fn expire(&mut self, now: u64) -> bool {
        if self.state == State::Pending && now.saturating_sub(self.started) >= 30_000 {
            self.state = State::Unknown;
            self.value.clear();
            return true;
        }
        if self.state == State::Shown && !self.visible(now) {
            self.value.clear();
            self.revealed = false;
            self.state = State::Hidden;
            return true;
        }
        false
    }
    pub fn value(&self, now: u64) -> Option<&str> {
        self.visible(now).then_some(self.value.as_str())
    }
    pub fn admits(&self, r: &Response) -> bool {
        if self.state != State::Pending || r.request_id != self.id {
            return false;
        }
        if crate::information::is_plain_error_response(r) {
            return true;
        }
        let Some(v) = r.secret_admin.as_ref() else {
            return false;
        };
        if r.kind != "secret_admin"
            || r.room != self.room
            || !exclusive(r)
            || v.kind != self.data.kind
            || v.requested_target != self.data.target
            || v.duration != self.data.duration
            || v.uses != self.data.uses
        {
            return false;
        }
        match v.state.as_str() {
            "unknown" => {
                v.committed.is_none() && v.value.is_empty() && v.target.is_empty() && v.expires == 0
            }
            "shown" | "committed_hidden" => {
                v.committed == Some(true)
                    && (if v.kind == Kind::Resetpass {
                        !v.target.is_empty() && v.target.len() <= 256 && v.expires == 0
                    } else {
                        v.target.is_empty() && v.expires >= 0
                    })
                    && (if v.state == "shown" {
                        v.value.len() == 32
                            && v.value
                                .bytes()
                                .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
                    } else {
                        v.value.is_empty()
                    })
            }
            _ => false,
        }
    }
    pub fn receive(&mut self, r: &Response, now: u64) -> bool {
        if !self.admits(r) {
            return false;
        }
        self.completed = Some(now);
        if r.kind == "error" {
            self.state = State::Rejected;
            return true;
        }
        let v = r.secret_admin.as_ref().expect("admitted receipt");
        self.committed = v.committed;
        self.state = match v.state.as_str() {
            "shown" => State::Shown,
            "committed_hidden" => State::Hidden,
            _ => State::Unknown,
        };
        self.canonical_target = v.target.clone();
        self.expires = v.expires;
        self.value = v.value.clone();
        true
    }
}
pub fn exclusive(r: &Response) -> bool {
    r.composition.is_none()
        && r.maintenance.is_none()
        && crate::composition::exclusive_shared(r)
        && r.status.is_empty()
        && r.capabilities.is_empty()
        && r.protocol == 0
        && r.registration_mode.is_empty()
        && r.online == 0
}
pub fn discard(attempt: &mut Option<Attempt>, notice: &mut Option<String>) {
    if let Some(a) = attempt.as_ref()
        && let Some(message) = a.notice()
    {
        *notice = Some(message.into());
    }
    *attempt = None;
}

#[cfg(test)]
mod tests {
    use super::*;
    fn attempt() -> Attempt {
        Attempt::new(
            Kind::Invitecode.data("1d", "2").unwrap(),
            "secret-1".into(),
            Binding {
                owner: "admin".into(),
                room: "lobby".into(),
                generation: 7,
                origin: Origin::synthetic("fixture", 7),
                epoch: 3,
            },
            0,
            None,
        )
    }
    fn response() -> Response {
        serde_json::from_value(serde_json::json!({"type":"secret_admin","request_id":"secret-1","room":"lobby","secret_admin":{"kind":"invitecode","requested_target":"","target":"","duration":"1d","uses":2,"expires":100,"state":"shown","committed":true,"value":"0123456789abcdef0123456789abcdef"}})).unwrap()
    }
    #[test]
    fn generated_secret_is_exclusive_correlated_and_debug_redacted() {
        let mut a = attempt();
        let r = response();
        assert_eq!(r.secret_admin.as_ref().unwrap().value.len(), 32);
        assert!(a.admits(&r));
        assert!(!format!("{r:?}").contains("0123456789abcdef0123456789abcdef"));
        let mut bad = r.clone();
        bad.preferences = Some(Default::default());
        assert!(!a.admits(&bad));
        bad = r.clone();
        bad.kind = "error".into();
        assert!(!a.admits(&bad));
        bad = r.clone();
        bad.request_id = "old".into();
        assert!(!a.receive(&bad, 1));
        assert_eq!(a.state, State::Pending);
        assert!(a.receive(&r, 1));
        assert!(!a.receive(&r, 2));
        assert!(a.value(59_999).is_some());
        assert!(a.expire(60_001));
        assert!(a.value(60_001).is_none());
        assert_eq!(a.notice(), Some(COMMITTED));
    }
    #[test]
    fn generated_secret_binding_and_unknown_never_replay() {
        let a = attempt();
        assert!(a.bound("admin", "lobby", 7, Some(&a.origin), 3));
        assert!(!a.bound("admin", "other", 7, Some(&a.origin), 3));
        assert!(!a.bound("admin", "lobby", 8, Some(&a.origin), 3));
        assert!(!a.bound("other", "lobby", 7, Some(&a.origin), 3));
        assert!(!a.bound("admin", "lobby", 7, None, 3));
        let mut held = Some(a);
        let mut notice = None;
        discard(&mut held, &mut notice);
        assert!(held.is_none());
        assert_eq!(notice.as_deref(), Some(UNKNOWN));
        let mut a = attempt();
        assert!(a.expire(30_000));
        assert!(!a.receive(&response(), 30_001));
        assert!(a.value(30_001).is_none());
    }
}
