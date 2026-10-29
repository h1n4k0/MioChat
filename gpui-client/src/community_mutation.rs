use serde::Deserialize;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CommunityAction {
    Poll,
    Vote,
    Event,
    Rsvp,
    Unrsvp,
    Close,
    Comment,
}
impl CommunityAction {
    pub fn parse(command: &str) -> Option<Self> {
        match command
            .split_whitespace()
            .next()?
            .to_ascii_lowercase()
            .as_str()
        {
            "/poll" => Some(Self::Poll),
            "/vote" => Some(Self::Vote),
            "/event" => Some(Self::Event),
            "/rsvp" => Some(Self::Rsvp),
            "/unrsvp" => Some(Self::Unrsvp),
            "/close" => Some(Self::Close),
            "/comment" => Some(Self::Comment),
            _ => None,
        }
    }
    pub fn command(self) -> &'static str {
        match self {
            Self::Poll => "/poll",
            Self::Vote => "/vote",
            Self::Event => "/event",
            Self::Rsvp => "/rsvp",
            Self::Unrsvp => "/unrsvp",
            Self::Close => "/close",
            Self::Comment => "/comment",
        }
    }
    pub fn title(self) -> &'static str {
        match self {
            Self::Poll => "确认创建投票",
            Self::Vote => "确认投票（可改投）",
            Self::Event => "确认创建活动",
            Self::Rsvp => "确认报名",
            Self::Unrsvp => "确认取消报名",
            Self::Close => "确认结束条目",
            Self::Comment => "确认发表评论",
        }
    }
    pub fn body_label(self) -> &'static str {
        match self {
            Self::Poll => "选项：每行一个，共2–10项，每项最多80字符",
            Self::Event => "活动介绍（最多800字符）",
            _ => "评论（最多800字符）",
        }
    }
    pub fn creates(self) -> bool {
        matches!(self, Self::Poll | Self::Event)
    }
    pub fn has_body(self) -> bool {
        self.creates() || self == Self::Comment
    }
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CommunityIntent {
    pub action: CommunityAction,
    pub object_id: i64,
    pub choice: u64,
    pub room: String,
}
impl CommunityIntent {
    /// Typed command correlation does not invent an object room. Native forms
    /// replace this with their captured, immutable typed panel destination.
    pub fn from_command(command: &str) -> Option<Self> {
        let action = CommunityAction::parse(command)?;
        let mut words = command.split_whitespace().skip(1);
        let object_id = if action.creates() {
            0
        } else {
            words.next()?.parse::<i64>().ok()?
        };
        let choice = if action == CommunityAction::Vote {
            words.next()?.parse::<u64>().ok()?
        } else {
            0
        };
        Some(Self {
            action,
            object_id,
            choice,
            room: String::new(),
        })
    }
    pub fn command(&self, title: &str, extra: &str, body: &str) -> Result<String, &'static str> {
        if self.room.is_empty() {
            return Err("请重新打开条目或频道列表，核对目标。");
        }
        let title = title.trim();
        let body = body.trim();
        if self.action.creates()
            && (title.is_empty()
                || title.chars().count() > 100
                || title.contains(['|', '\n', '\r']))
        {
            return Err("标题须为1–100字符，不能包含换行或竖线。");
        }
        match self.action {
            CommunityAction::Poll => {
                let options: Vec<_> = body.lines().map(str::trim).collect();
                if !(2..=10).contains(&options.len())
                    || options
                        .iter()
                        .any(|s| s.is_empty() || s.chars().count() > 80 || s.contains('|'))
                {
                    return Err("请输入2–10个非空选项，每行一个，最多80字符且不能含竖线。");
                }
                Ok(format!("/poll {title}|{}", options.join("|")))
            }
            CommunityAction::Event => {
                if extra.len() != 16
                    || extra.contains(['|', '\n', '\r'])
                    || body.chars().count() > 800
                {
                    return Err("时间格式为北京时间 YYYY-MM-DD HH:MM；介绍最多800字符。");
                }
                Ok(format!("/event {title}|{extra}|{body}"))
            }
            _ => {
                if self.object_id <= 0 {
                    return Err("条目编号无效，请重新打开详情。");
                }
                match self.action {
                    CommunityAction::Vote => {
                        if !(1..=10).contains(&self.choice) {
                            return Err("无效选项编号。");
                        }
                        Ok(format!("/vote {} {}", self.object_id, self.choice))
                    }
                    CommunityAction::Comment => {
                        if body.is_empty() || body.chars().count() > 800 {
                            return Err("评论不能为空且最多800字符。");
                        }
                        Ok(format!("/comment {} {body}", self.object_id))
                    }
                    _ => Ok(format!("{} {}", self.action.command(), self.object_id)),
                }
            }
        }
    }
    pub fn accepts_ack(&self, response: &crate::session::Response, origin: &str) -> bool {
        if response.configuration_receipt.is_some() || response.management_receipt.is_some() {
            return false;
        }
        !origin.is_empty()
            && response.room == origin
            && !response.replace
            && !response.clear_view
            && response.draft.is_none()
            && response.message.is_none()
            && response.messages.is_empty()
            && response.panel.as_ref().is_none_or(|p| p.scope == origin)
            && response.community_receipt.as_ref().is_some_and(|r| {
                r.committed
                    && r.command == self.action.command()
                    && r.origin_scope == origin
                    && r.object_id > 0
                    && (self.object_id == 0 || r.object_id == self.object_id)
                    && !r.room.is_empty()
                    && (self.room.is_empty() || self.room == r.room)
            })
    }
}
#[derive(Clone, Debug, Default, Deserialize)]
pub struct CommunityReceipt {
    pub command: String,
    pub object_id: i64,
    pub room: String,
    pub origin_scope: String,
    pub committed: bool,
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::direct_message::{DirectMessageForm, FormPurpose, FormSlot};
    #[test]
    fn native_event_fields_scope_pending_and_hidden_fields_are_separate() {
        let intent = CommunityIntent {
            action: CommunityAction::Event,
            object_id: 0,
            choice: 0,
            room: "channel-a".into(),
        };
        let mut f = DirectMessageForm::new("alice".into(), "dm:bob".into(), 3);
        f.purpose = FormPurpose::Community(intent);
        f.target = "中文活动".into();
        f.extra = "2099-01-01 20:00".into();
        f.body = "说明|保留竖线".into();
        assert_eq!(f.initial_slot(), FormSlot::Target);
        assert_eq!(f.next_slot(FormSlot::Target, false), FormSlot::Extra);
        assert_eq!(f.next_slot(FormSlot::Extra, false), FormSlot::Body);
        assert_eq!(f.next_slot(FormSlot::Body, false), FormSlot::Target);
        assert_eq!(f.next_slot(FormSlot::Target, true), FormSlot::Body);
        assert_eq!(
            f.command("alice", "dm:bob", 3).unwrap(),
            "/event 中文活动|2099-01-01 20:00|说明|保留竖线"
        );
        assert!(f.command("alice", "lobby", 3).is_err());
        assert!(f.begin("once".into()));
        assert!(!f.begin("twice".into()));
        f.extra = "2099-01-02 20:00".into();
        assert!(!f.finish("once", "alice", 3, true));
        assert_eq!(f.extra, "2099-01-02 20:00");
        f.purpose = FormPurpose::Community(CommunityIntent {
            action: CommunityAction::Close,
            object_id: 17,
            choice: 0,
            room: "channel-a".into(),
        });
        assert!(!f.slot_enabled(FormSlot::Extra));
        assert!(!f.slot_enabled(FormSlot::Target));
        assert!(!f.has_body());
        assert_eq!(f.limit(FormSlot::Body), 0);
    }
    #[test]
    fn poll_grammar_and_channel_roundtrip_ack_binding() {
        let i = CommunityIntent {
            action: CommunityAction::Poll,
            object_id: 0,
            choice: 0,
            room: "a".into(),
        };
        assert_eq!(
            i.command("题目", "", "第一\n第二").unwrap(),
            "/poll 题目|第一|第二"
        );
        for bad in ["one", "one\n\ntwo", "one|bad\ntwo"] {
            assert!(i.command("Q", "", bad).is_err());
        }
        assert!(i.command("Q|bad", "", "one\ntwo").is_err());
        let mut response = crate::session::Response {
            room: "dm:bob".into(),
            community_receipt: Some(CommunityReceipt {
                command: "/poll".into(),
                object_id: 19,
                room: "a".into(),
                origin_scope: "dm:bob".into(),
                committed: true,
            }),
            ..Default::default()
        };
        assert!(i.accepts_ack(&response, "dm:bob"));
        response.community_receipt.as_mut().unwrap().room = "b".into();
        assert!(!i.accepts_ack(&response, "dm:bob"));
        response.community_receipt.as_mut().unwrap().room = "a".into();
        response.community_receipt.as_mut().unwrap().committed = false;
        assert!(!i.accepts_ack(&response, "dm:bob"));
    }
}
