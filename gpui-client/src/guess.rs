use serde::Deserialize;
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GuessAction {
    Start,
    Guess,
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GuessIntent {
    pub action: GuessAction,
    pub object_id: i64,
    pub room: String,
    pub maximum: u64,
    pub requested: String,
}
pub fn is_view(command: &str) -> bool {
    let mut words = command.split_whitespace();
    words
        .next()
        .is_some_and(|c| c.eq_ignore_ascii_case("/guess"))
        && words.next().is_none()
}
impl GuessIntent {
    pub fn from_command(command: &str) -> Option<Self> {
        let command = command.trim();
        let (name, arg) = command.split_once(char::is_whitespace)?;
        if !name.eq_ignore_ascii_case("/guess") {
            return None;
        }
        let arg = arg.trim();
        if arg.is_empty() {
            return None;
        }
        let (first, rest) = arg.split_once(char::is_whitespace).unwrap_or((arg, ""));
        let start = first == "start";
        Some(Self {
            action: if start {
                GuessAction::Start
            } else {
                GuessAction::Guess
            },
            object_id: 0,
            room: String::new(),
            maximum: if start {
                if rest.trim().is_empty() {
                    100
                } else {
                    rest.trim().parse().unwrap_or(0)
                }
            } else {
                0
            },
            requested: if start {
                String::new()
            } else {
                arg.to_string()
            },
        })
    }
    pub fn title(&self) -> &'static str {
        if self.action == GuessAction::Start {
            "确认开始猜数字"
        } else {
            "确认提交猜测"
        }
    }
    pub fn body_label(&self) -> &'static str {
        if self.action == GuessAction::Start {
            "数字上限（2–10000，默认100）"
        } else {
            "你的数字（猜测对频道可见）"
        }
    }
    pub fn command(&self, body: &str) -> Result<String, &'static str> {
        if self.room.is_empty() {
            return Err("请重新打开游戏概览，核对目标频道。");
        }
        let body = body.trim();
        if self.action == GuessAction::Start {
            let maximum = if body.is_empty() {
                100
            } else {
                body.parse::<u64>().map_err(|_| "请输入数字上限2–10000。")?
            };
            if !(2..=10000).contains(&maximum) {
                return Err("数字上限范围2–10000。");
            }
            return Ok(format!("/guess start {maximum}"));
        }
        if self.object_id <= 0 || !(2..=10000).contains(&self.maximum) {
            return Err("游戏编号或范围已不可用，请刷新。");
        }
        let value = body
            .parse::<u64>()
            .map_err(|_| "请输入游戏范围内的整数。")?;
        if value == 0 || value > self.maximum || body.chars().count() > 24 {
            return Err("请输入游戏范围内的整数。");
        }
        // Preserve the original lexical-win semantics, including 05/+5 inputs.
        Ok(format!("/guess {body}"))
    }
    pub fn bind_submission(&self, command: &str) -> Option<Self> {
        let parsed = Self::from_command(command)?;
        if self.action != parsed.action {
            return None;
        }
        let mut bound = self.clone();
        bound.requested = parsed.requested;
        if self.action == GuessAction::Start {
            bound.maximum = parsed.maximum;
        }
        Some(bound)
    }
    pub fn accepts_ack(&self, r: &crate::session::Response, origin: &str) -> bool {
        if r.configuration_receipt.is_some() || r.management_receipt.is_some() {
            return false;
        }
        !origin.is_empty()
            && (self.action != GuessAction::Start || (2..=10000).contains(&self.maximum))
            && r.room == origin
            && !r.replace
            && !r.clear_view
            && r.draft.is_none()
            && r.message.is_none()
            && r.messages.is_empty()
            && r.panel.as_ref().is_none_or(|p| p.scope == origin)
            && r.guess_receipt.as_ref().is_some_and(|a| {
                a.committed
                    && a.operation
                        == if self.action == GuessAction::Start {
                            "start"
                        } else {
                            "guess"
                        }
                    && a.object_id > 0
                    && (self.object_id == 0 || a.object_id == self.object_id)
                    && !a.room.is_empty()
                    && (self.room.is_empty() || a.room == self.room)
                    && a.origin_scope == origin
                    && (2..=10000).contains(&a.maximum)
                    && (self.maximum == 0 || self.maximum == a.maximum)
                    && if self.action == GuessAction::Start {
                        a.value.is_empty()
                            && a.requested.is_empty()
                            && a.feedback.is_empty()
                            && !a.closed
                    } else {
                        a.requested == self.requested
                            && !a.value.is_empty()
                            && !a.feedback.is_empty()
                    }
            })
    }
}
#[derive(Clone, Debug, Default, Deserialize)]
#[serde(default)]
pub struct GuessReceipt {
    pub operation: String,
    pub object_id: i64,
    pub room: String,
    pub origin_scope: String,
    pub requested: String,
    pub value: String,
    pub maximum: u64,
    pub closed: bool,
    pub feedback: String,
    pub committed: bool,
}

/// The real menu/button route captures only a typed panel with matching origin.
pub fn native_intent(
    command: &str,
    panel: &crate::session::Panel,
    current: &str,
) -> Option<GuessIntent> {
    if panel.scope != current {
        return None;
    }
    let view = panel.community.as_ref()?;
    if command == "local:guess-start" {
        if view.room.is_empty() {
            return None;
        }
        return Some(GuessIntent {
            action: GuessAction::Start,
            object_id: 0,
            room: view.room.clone(),
            maximum: 100,
            requested: String::new(),
        });
    }
    let id = command
        .strip_prefix("local:guess-input ")?
        .parse::<i64>()
        .ok()?;
    let d = view.detail.as_ref()?;
    let g = d.game.as_ref()?;
    if d.card.id != id
        || id <= 0
        || d.card.kind != "guess"
        || d.card.closed
        || d.card.unavailable
        || !g.can_guess
        || !(2..=10000).contains(&g.max)
        || d.card.room.is_empty()
    {
        return None;
    }
    Some(GuessIntent {
        action: GuessAction::Guess,
        object_id: id,
        room: d.card.room.clone(),
        maximum: g.max,
        requested: String::new(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        community::{CommunityView, GameView, ObjectCard, ObjectDetail},
        session::{Panel, Response},
    };
    #[test]
    fn numeric_form_limits_legacy_lexical_values_and_view_routing() {
        let start = GuessIntent {
            action: GuessAction::Start,
            object_id: 0,
            room: "room-a".into(),
            maximum: 100,
            requested: String::new(),
        };
        assert_eq!(start.command("").unwrap(), "/guess start 100");
        assert_eq!(start.command("2").unwrap(), "/guess start 2");
        assert_eq!(start.command("10000").unwrap(), "/guess start 10000");
        for bad in ["1", "10001", "not a number"] {
            assert!(start.command(bad).is_err());
        }
        let guess = GuessIntent {
            action: GuessAction::Guess,
            object_id: 7,
            room: "room-a".into(),
            maximum: 10,
            requested: String::new(),
        };
        for value in ["05", "+5", "1", "10"] {
            assert_eq!(guess.command(value).unwrap(), format!("/guess {value}"));
        }
        for bad in ["0", "11", "x"] {
            assert!(guess.command(bad).is_err());
        }
        assert!(is_view("/GUESS "));
        assert!(!is_view("/guess 1"));
        assert!(!crate::information::is_information_command(
            "/guess start 10"
        ));
        assert!(crate::information::is_information_command("/guess"));
        assert!(GuessIntent::from_command("/word start private").is_none());
    }
    #[test]
    fn actual_native_panel_route_keeps_game_id_room_and_current_scope() {
        let mut p = Panel {
            scope: "dm:bob".into(),
            community: Some(CommunityView {
                room: "actual-a".into(),
                detail: Some(ObjectDetail {
                    card: ObjectCard {
                        id: 9,
                        room: "actual-a".into(),
                        kind: "guess".into(),
                        ..Default::default()
                    },
                    game: Some(GameView {
                        max: 10,
                        can_guess: true,
                        ..Default::default()
                    }),
                    ..Default::default()
                }),
                ..Default::default()
            }),
            ..Default::default()
        };
        let i = native_intent("local:guess-input 9", &p, "dm:bob").unwrap();
        assert_eq!((i.object_id, i.maximum), (9, 10));
        assert_eq!(i.room, "actual-a");
        assert!(native_intent("local:guess-input 9", &p, "new-scope").is_none());
        assert!(native_intent("local:guess-input 8", &p, "dm:bob").is_none());
        p.community
            .as_mut()
            .unwrap()
            .detail
            .as_mut()
            .unwrap()
            .card
            .kind = "word".into();
        assert!(native_intent("local:guess-input 9", &p, "dm:bob").is_none());
        let start = native_intent("local:guess-start", &p, "dm:bob").unwrap();
        assert_eq!(start.room, "actual-a");
        assert_eq!(start.bind_submission("/guess start 2").unwrap().maximum, 2);
    }
    #[test]
    fn numeric_receipt_binds_actual_game_origin_value_and_maximum() {
        let i = GuessIntent {
            action: GuessAction::Guess,
            object_id: 7,
            room: "room-a".into(),
            maximum: 10,
            requested: "05".into(),
        };
        let good = Response {
            room: "dm:bob".into(),
            guess_receipt: Some(GuessReceipt {
                operation: "guess".into(),
                object_id: 7,
                room: "room-a".into(),
                origin_scope: "dm:bob".into(),
                requested: "05".into(),
                value: "05".into(),
                maximum: 10,
                feedback: "偏小".into(),
                committed: true,
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(i.accepts_ack(&good, "dm:bob"));
        let mut bad = good.clone();
        bad.guess_receipt.as_mut().unwrap().object_id = 8;
        assert!(!i.accepts_ack(&bad, "dm:bob"));
        let mut bad = good.clone();
        bad.guess_receipt.as_mut().unwrap().room = "room-b".into();
        assert!(!i.accepts_ack(&bad, "dm:bob"));
        let mut bad = good.clone();
        bad.guess_receipt.as_mut().unwrap().requested = "5".into();
        assert!(!i.accepts_ack(&bad, "dm:bob"));
        let mut bad = good.clone();
        bad.guess_receipt.as_mut().unwrap().maximum = 20;
        assert!(!i.accepts_ack(&bad, "dm:bob"));
        assert!(!i.accepts_ack(&good, "lobby"));
        let start = GuessIntent::from_command("/guess start 2").unwrap();
        let receipt = Response {
            room: "lobby".into(),
            guess_receipt: Some(GuessReceipt {
                operation: "start".into(),
                object_id: 8,
                room: "lobby".into(),
                origin_scope: "lobby".into(),
                maximum: 2,
                committed: true,
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(start.accepts_ack(&receipt, "lobby"));
        assert!(
            !GuessIntent::from_command("/guess start bad")
                .unwrap()
                .accepts_ack(&receipt, "lobby")
        );
    }
}
