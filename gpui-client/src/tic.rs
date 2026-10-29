use serde::Deserialize;
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TicAction {
    Challenge,
    Accept,
    Move,
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TicIntent {
    pub action: TicAction,
    pub object_id: i64,
    pub cell: u64,
    pub room: String,
    pub revision: Option<i64>,
    pub requested: String,
}
impl TicIntent {
    pub fn from_command(command: &str) -> Option<Self> {
        let parts: Vec<_> = command.split_whitespace().collect();
        if !parts.first()?.eq_ignore_ascii_case("/ttt") {
            return None;
        }
        let mut i = Self {
            action: TicAction::Challenge,
            object_id: 0,
            cell: 0,
            room: String::new(),
            revision: None,
            requested: String::new(),
        };
        match parts.len() {
            2 => i.requested = parts[1].into(),
            3 => {
                if parts[1] == "accept" {
                    i.action = TicAction::Accept;
                    i.object_id = parts[2].parse().ok()?;
                } else {
                    i.action = TicAction::Move;
                    i.object_id = parts[1].parse().ok()?;
                    i.cell = parts[2].parse().ok()?;
                }
            }
            _ => return None,
        };
        Some(i)
    }
    pub fn operation(&self) -> &'static str {
        match self.action {
            TicAction::Challenge => "challenge",
            TicAction::Accept => "accept",
            TicAction::Move => "move",
        }
    }
    pub fn title(&self) -> &'static str {
        match self.action {
            TicAction::Challenge => "确认发起井字棋挑战",
            TicAction::Accept => "确认接受挑战",
            TicAction::Move => "确认落子",
        }
    }
    pub fn command(&self, target: &str) -> Result<String, &'static str> {
        if self.room.is_empty() {
            return Err("请重新打开频道列表或棋盘核对目标。");
        }
        if self.action == TicAction::Challenge {
            let target = target.trim();
            if target.is_empty()
                || target.chars().any(char::is_whitespace)
                || target.chars().count() > 64
            {
                return Err("请输入完整用户名或 #UID。");
            }
            return Ok(format!("/ttt {target}"));
        }
        if self.object_id <= 0 || self.revision.is_none_or(|v| v < 0) {
            return Err("棋盘版本不可用，请重新打开详情。");
        }
        if self.action == TicAction::Accept {
            return Ok(format!("/ttt accept {}", self.object_id));
        }
        if !(1..=9).contains(&self.cell) {
            return Err("格子编号为1–9。");
        }
        Ok(format!("/ttt {} {}", self.object_id, self.cell))
    }
    pub fn bind_submission(&self, command: &str) -> Option<Self> {
        let parsed = Self::from_command(command)?;
        if (parsed.action, parsed.object_id, parsed.cell)
            != (self.action, self.object_id, self.cell)
        {
            return None;
        }
        let mut bound = self.clone();
        bound.requested = parsed.requested;
        Some(bound)
    }
    pub fn accepts_ack(&self, r: &crate::session::Response, origin: &str) -> bool {
        if r.configuration_receipt.is_some() || r.management_receipt.is_some() {
            return false;
        }
        !origin.is_empty()
            && r.room == origin
            && !r.replace
            && !r.clear_view
            && r.draft.is_none()
            && r.message.is_none()
            && r.messages.is_empty()
            && r.panel.as_ref().is_none_or(|p| p.scope == origin)
            && r.tic_receipt.as_ref().is_some_and(|a| {
                a.committed
                    && a.operation == self.operation()
                    && a.origin_scope == origin
                    && a.object_id > 0
                    && (self.object_id == 0 || a.object_id == self.object_id)
                    && !a.room.is_empty()
                    && (self.room.is_empty() || a.room == self.room)
                    && a.cell == self.cell
                    && if self.action == TicAction::Challenge {
                        a.requested == self.requested && !a.canonical.is_empty() && a.revision == 0
                    } else {
                        self.revision.is_none_or(|v| a.revision > v)
                    }
            })
    }
}
#[derive(Clone, Debug, Default, Deserialize)]
#[serde(default)]
pub struct TicReceipt {
    pub operation: String,
    pub object_id: i64,
    pub room: String,
    pub origin_scope: String,
    pub revision: i64,
    pub cell: u64,
    pub requested: String,
    pub canonical: String,
    pub committed: bool,
}

/// Only an empty cell in a fresh actionable nine-cell board yields a move.
pub fn board_move(detail: &crate::community::ObjectDetail, cell: usize) -> Option<TicIntent> {
    let g = detail.game.as_ref()?;
    if detail.card.kind != "ttt"
        || detail.card.closed
        || detail.card.unavailable
        || !g.can_move
        || g.cells.len() != 9
        || !(1..=9).contains(&cell)
        || !g.cells[cell - 1].is_empty()
    {
        return None;
    }
    Some(TicIntent {
        action: TicAction::Move,
        object_id: detail.card.id,
        cell: cell as u64,
        room: detail.card.room.clone(),
        revision: Some(g.revision),
        requested: String::new(),
    })
}

pub fn board_move_still_current(
    intent: &TicIntent,
    panel: &crate::session::Panel,
    current: &str,
) -> bool {
    panel.scope == current
        && panel
            .community
            .as_ref()
            .and_then(|v| v.detail.as_ref())
            .and_then(|d| board_move(d, intent.cell as usize))
            .is_some_and(|i| i == *intent)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        community::{GameView, ObjectCard, ObjectDetail},
        session::{Request, Response},
    };
    #[test]
    fn zero_revision_serializes_and_actual_global_revision_may_jump() {
        let mut request = Request::simple("command", "move");
        request.tic_version = Some(0);
        assert_eq!(serde_json::to_value(&request).unwrap()["tic_version"], 0);
        request.tic_version = None;
        assert!(
            serde_json::to_value(&request)
                .unwrap()
                .get("tic_version")
                .is_none()
        );
        let i = TicIntent {
            action: TicAction::Move,
            object_id: 5,
            cell: 1,
            room: "private".into(),
            revision: Some(7),
            requested: String::new(),
        };
        let mut r = Response {
            room: "dm:bob".into(),
            tic_receipt: Some(TicReceipt {
                operation: "move".into(),
                object_id: 5,
                cell: 1,
                room: "private".into(),
                origin_scope: "dm:bob".into(),
                revision: 30,
                committed: true,
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(i.accepts_ack(&r, "dm:bob"));
        r.tic_receipt.as_mut().unwrap().cell = 2;
        assert!(!i.accepts_ack(&r, "dm:bob"));
        r.tic_receipt.as_mut().unwrap().cell = 1;
        r.tic_receipt.as_mut().unwrap().revision = 7;
        assert!(!i.accepts_ack(&r, "dm:bob"));
        let parsed:Response=serde_json::from_str(r#"{"type":"action","tic_receipt":{"operation":"accept","object_id":1,"room":"lobby","origin_scope":"lobby","revision":1,"cell":0,"committed":true}}"#).unwrap();
        assert!(parsed.tic_receipt.unwrap().requested.is_empty());
    }
    #[test]
    fn actual_board_move_helper_captures_id_room_revision_and_only_empty_cells() {
        let mut detail = ObjectDetail {
            card: ObjectCard {
                id: 9,
                kind: "ttt".into(),
                room: "original-room".into(),
                ..Default::default()
            },
            game: Some(GameView {
                cells: vec![String::new(); 9],
                can_move: true,
                revision: 17,
                ..Default::default()
            }),
            ..Default::default()
        };
        let captured = board_move(&detail, 2).unwrap();
        let mut panel = crate::session::Panel {
            scope: "origin-dm".into(),
            community: Some(crate::community::CommunityView {
                detail: Some(detail.clone()),
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(board_move_still_current(&captured, &panel, "origin-dm"));
        assert!(!board_move_still_current(&captured, &panel, "new-room"));
        panel
            .community
            .as_mut()
            .unwrap()
            .detail
            .as_mut()
            .unwrap()
            .game
            .as_mut()
            .unwrap()
            .revision = 18;
        assert!(!board_move_still_current(&captured, &panel, "origin-dm"));
        for cell in 1..=9 {
            let i = board_move(&detail, cell).unwrap();
            assert_eq!(
                (i.object_id, i.cell, i.revision),
                (9, cell as u64, Some(17))
            );
            assert_eq!(i.room, "original-room");
            assert_eq!(i.command("").unwrap(), format!("/ttt 9 {cell}"));
        }
        assert!(board_move(&detail, 0).is_none() && board_move(&detail, 10).is_none());
        detail.game.as_mut().unwrap().cells[0] = "X".into();
        assert!(board_move(&detail, 1).is_none());
        detail.card.closed = true;
        assert!(board_move(&detail, 2).is_none());
        detail.card.closed = false;
        detail.game.as_mut().unwrap().can_move = false;
        assert!(board_move(&detail, 2).is_none());
    }
    #[test]
    fn challenge_ack_uses_exact_requested_and_server_canonical_not_client_lowercase() {
        let i = TicIntent {
            action: TicAction::Challenge,
            object_id: 0,
            cell: 0,
            room: "room-a".into(),
            revision: None,
            requested: "İuser".into(),
        };
        let good = Response {
            room: "dm:peer".into(),
            tic_receipt: Some(TicReceipt {
                operation: "challenge".into(),
                object_id: 12,
                room: "room-a".into(),
                origin_scope: "dm:peer".into(),
                requested: "İuser".into(),
                canonical: "iuser".into(),
                committed: true,
                ..Default::default()
            }),
            ..Default::default()
        };
        assert!(i.accepts_ack(&good, "dm:peer"));
        let mut wrong = good.clone();
        wrong.tic_receipt.as_mut().unwrap().room = "room-b".into();
        assert!(!i.accepts_ack(&wrong, "dm:peer"));
        assert_eq!(i.bind_submission("/ttt Other").unwrap().requested, "Other");
        assert!(i.bind_submission("/ttt 12 3").is_none());
    }
}
