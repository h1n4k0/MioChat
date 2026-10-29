use crate::session::Response;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SimpleAction {
    Me,
    Roll,
    Coin,
    EightBall,
    Shrug,
}
impl SimpleAction {
    pub fn parse(command: &str) -> Option<Self> {
        match command
            .split_whitespace()
            .next()?
            .to_ascii_lowercase()
            .as_str()
        {
            "/me" => Some(Self::Me),
            "/roll" => Some(Self::Roll),
            "/coin" => Some(Self::Coin),
            "/8ball" => Some(Self::EightBall),
            "/shrug" => Some(Self::Shrug),
            _ => None,
        }
    }
    pub fn title(self) -> &'static str {
        match self {
            Self::Me => "发送动作",
            Self::Roll => "掷骰并发送",
            Self::Coin => "抛硬币并发送",
            Self::EightBall => "询问八号球并发送",
            Self::Shrug => "发送耸肩表情",
        }
    }
    pub fn input_label(self) -> &'static str {
        match self {
            Self::Roll => "骰子面数（2–1000000）",
            Self::EightBall => "你的问题",
            _ => "动作内容",
        }
    }
    pub fn has_body(self) -> bool {
        matches!(self, Self::Me | Self::Roll | Self::EightBall)
    }
    pub fn command(self, body: &str) -> Result<String, &'static str> {
        let body = body.trim();
        match self {
            Self::Me | Self::EightBall => {
                if body.is_empty() || body.chars().count() > 800 {
                    return Err("内容不能为空，且最多800字符；生成后的消息也须不超过800字符。");
                }
                Ok(format!(
                    "{} {body}",
                    if self == Self::Me { "/me" } else { "/8ball" }
                ))
            }
            Self::Roll => {
                let sides = if body.is_empty() {
                    6
                } else {
                    body.parse::<u64>()
                        .map_err(|_| "请输入2–1000000之间的整数。")?
                };
                if !(2..=1_000_000).contains(&sides) {
                    return Err("面数范围：2–1000000。");
                }
                Ok(format!("/roll {sides}"))
            }
            Self::Coin => Ok("/coin".into()),
            Self::Shrug => Ok("/shrug".into()),
        }
    }
    pub fn accepts_ack(self, response: &Response, owner: &str, origin: &str) -> bool {
        if response.configuration_receipt.is_some() || response.management_receipt.is_some() {
            return false;
        }
        !owner.is_empty()
            && !origin.is_empty()
            && response.room == origin
            && !response.replace
            && !response.clear_view
            && response.draft.is_none()
            && response.message.as_ref().is_some_and(|message| {
                message.id > 0
                    && message.sender == owner
                    && message.room == origin
                    && message.kind
                        == if self == Self::Shrug {
                            "chat"
                        } else {
                            "action"
                        }
                    && if let Some(peer) = origin.strip_prefix("dm:") {
                        message.recipient == peer
                    } else {
                        message.recipient.is_empty()
                    }
            })
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{direct_message::DirectMessageForm, session::Message};
    #[test]
    fn action_form_keeps_scope_and_rejects_boundaries_duplicate_and_late_ack() {
        let mut form = DirectMessageForm::new("alice".into(), "dm:bob".into(), 7);
        form.purpose = crate::direct_message::FormPurpose::Simple(SimpleAction::Roll);
        form.body = "6".into();
        assert_eq!(form.command("alice", "dm:bob", 7).unwrap(), "/roll 6");
        assert!(form.command("alice", "lobby", 7).is_err());
        assert!(form.command("alice", "dm:bob", 8).is_err());
        assert!(form.begin("pending".into()));
        assert!(form.command("alice", "dm:bob", 7).is_err());
        form.body = "20".into();
        assert!(!form.finish("pending", "alice", 7, true));
        assert_eq!(form.body, "20");
        for value in ["1", "1000001", "-1", "18446744073709551616", "3 4"] {
            assert!(SimpleAction::Roll.command(value).is_err());
        }
        assert_eq!(SimpleAction::Roll.command("").unwrap(), "/roll 6");
        assert!(SimpleAction::EightBall.command("").is_err());
        assert_eq!(SimpleAction::Me.command("动作").unwrap(), "/me 动作");
        assert!(!SimpleAction::Coin.has_body() && !SimpleAction::Shrug.has_body());
        assert!(!form.routes_input(false, true, "alice", 7));
    }
    #[test]
    fn simple_ack_is_bound_to_sender_kind_and_original_public_or_private_scope() {
        let good = Response {
            room: "dm:bob".into(),
            message: Some(Message {
                id: 1,
                sender: "alice".into(),
                recipient: "bob".into(),
                room: "dm:bob".into(),
                kind: "action".into(),
                ..Message::default()
            }),
            ..Response::default()
        };
        assert!(SimpleAction::Roll.accepts_ack(&good, "alice", "dm:bob"));
        assert!(!SimpleAction::Roll.accepts_ack(&good, "alice", "lobby"));
        assert!(!SimpleAction::Roll.accepts_ack(&good, "mallory", "dm:bob"));
        assert!(!SimpleAction::Shrug.accepts_ack(&good, "alice", "dm:bob"));
    }
}
