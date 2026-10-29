package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func challengeQ(q queryer, user, target, room string) (int64, string, error) {
	u, err := resolveUserQ(q, target)
	if err != nil {
		return 0, "", err
	}
	if u.Name == user || !canReadQ(q, u.Name, room) {
		return 0, "", errors.New("对方无法接受此挑战")
	}
	blocked, err := blockedPolicyQ(q, u.Name, user)
	if err != nil {
		return 0, "", errors.New("暂时无法核验挑战接收策略")
	}
	if blocked {
		return 0, "", errors.New("对方无法接受此挑战")
	}
	game := TicTacToe{X: user, O: u.Name, Turn: user}
	data, err := json.Marshal(game)
	if err != nil {
		return 0, "", err
	}
	id, err := newObjectQ(q, user, room, "ttt", user+" vs "+u.Name, string(data))
	return id, u.Name, err
}

func ticMoveQ(q queryer, user string, id int64, cell int, accept bool) (TicTacToe, error) {
	var game TicTacToe
	o, e := objectQ(q, id)
	if e != nil || o.Kind != "ttt" || !canReadQ(q, user, o.Room) {
		return game, errors.New("对局不存在或没有权限")
	}
	if e = json.Unmarshal([]byte(o.Payload), &game); e != nil {
		return game, e
	}
	if o.Closed {
		return game, errors.New("对局已结束")
	}
	if user != game.X && user != game.O {
		return game, errors.New("你不是对局参与者")
	}
	if e = canSendQ(q, user, o.Room, ""); e != nil {
		return game, e
	}
	if accept {
		if user != game.O || game.Accepted {
			return game, errors.New("仅被邀请者可接受一次")
		}
		game.Accepted = true
	} else {
		if !game.Accepted {
			return game, errors.New("等待对方接受邀请")
		}
		if game.Turn != user {
			return game, errors.New("还没轮到你")
		}
		if cell < 1 || cell > 9 || game.Cells[cell-1] != "" {
			return game, errors.New("请选择 1–9 中的空位")
		}
		symbol := "X"
		if user == game.O {
			symbol = "O"
		}
		game.Cells[cell-1] = symbol
		for _, line := range [][3]int{{0, 1, 2}, {3, 4, 5}, {6, 7, 8}, {0, 3, 6}, {1, 4, 7}, {2, 5, 8}, {0, 4, 8}, {2, 4, 6}} {
			if game.Cells[line[0]] != "" && game.Cells[line[0]] == game.Cells[line[1]] && game.Cells[line[1]] == game.Cells[line[2]] {
				game.Winner = user
			}
		}
		full := true
		for _, v := range game.Cells {
			if v == "" {
				full = false
			}
		}
		if full && game.Winner == "" {
			game.Winner = "draw"
		}
		game.Turn = game.X
		if user == game.X {
			game.Turn = game.O
		}
	}
	data, _ := json.Marshal(game)
	if _, e = q.Exec(`UPDATE objects SET payload=?,closed=? WHERE id=?`, string(data), game.Winner != "", id); e != nil {
		return game, e
	}
	if game.Winner != "" {
		for _, n := range []string{game.X, game.O} {
			if _, e = q.Exec(`INSERT OR IGNORE INTO badges VALUES(?,'棋手',?)`, n, time.Now().Unix()); e != nil {
				return game, e
			}
		}
	}
	if _, e = q.Exec(`INSERT INTO object_events VALUES(NULL,?,?,?,?)`, id, user, fmt.Sprintf("move=%d accept=%v", cell, accept), time.Now().Unix()); e != nil {
		return game, e
	}
	return game, nil
}
