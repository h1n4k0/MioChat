package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type guiCompletionRequest struct {
	Kind   string `json:"kind"`
	Prefix string `json:"prefix"`
}
type guiCompletionReceipt struct {
	Kind    string   `json:"kind"`
	Prefix  string   `json:"prefix"`
	Choices []string `json:"choices"`
}

func completionPrefixValid(in *guiCompletionRequest) bool {
	if in == nil || !utf8.ValidString(in.Prefix) || len(in.Prefix) > 256 {
		return false
	}
	for _, c := range in.Prefix {
		if unicode.IsSpace(c) || unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			return false
		}
	}
	switch in.Kind {
	case "command":
		return strings.HasPrefix(in.Prefix, "/")
	case "room":
		return strings.HasPrefix(in.Prefix, "#")
	case "user":
		return !strings.HasPrefix(in.Prefix, "/") && !strings.HasPrefix(in.Prefix, "#")
	default:
		return false
	}
}
func completionRequestOnly(request guiRequest) bool {
	request.Type = ""
	request.RequestID = ""
	request.Room = ""
	request.Completion = nil
	request.ctx = nil
	request.sessionID = ""
	request.connectionIP = ""
	request.communityRoom = ""
	return reflect.DeepEqual(request, guiRequest{})
}

// Same authenticated public-name prefix query as the original TUI complete(),
// not a contacts-only directory. Literal LIKE escaping narrows wildcard input.
func completionDataQ(q queryer, user string, in guiCompletionRequest) ([]string, error) {
	choices := make([]string, 0)
	switch in.Kind {
	case "command":
		for _, c := range commandCatalog {
			if strings.HasPrefix(c.Name, in.Prefix) {
				choices = append(choices, c.Name)
			}
		}
	case "room":
		rooms, err := visibleRoomsQ(q, user, false)
		if err != nil {
			return nil, err
		}
		for _, r := range rooms {
			if strings.HasPrefix(r.Name, strings.TrimPrefix(in.Prefix, "#")) {
				choices = append(choices, "#"+r.Name)
			}
		}
	case "user":
		prefix := strings.TrimPrefix(in.Prefix, "@")
		prefix = strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(prefix)
		rows, err := q.Query(`SELECT name FROM users WHERE name LIKE ? ESCAPE '\' ORDER BY name LIMIT 100`, prefix+"%")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				return nil, err
			}
			if strings.HasPrefix(in.Prefix, "@") {
				name = "@" + name
			}
			choices = append(choices, name)
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("无效补全类型")
	}
	sort.Strings(choices)
	// Do not truncate rooms/commands silently. A too-broad request must be narrowed.
	raw, err := json.Marshal(choices)
	if err != nil {
		return nil, err
	}
	if len(raw) > 32*1024 {
		return nil, errors.New("补全结果过多，请输入更长前缀")
	}
	return choices, nil
}
func guiCompletePrefix(parent context.Context, store *Store, user User, scope guiScope, sessionID string, request guiRequest) (guiResponse, error) {
	var empty guiResponse
	if parent == nil || sessionID == "" || request.Type != "completion" || request.Room != scope.key() || !completionRequestOnly(request) || !completionPrefixValid(request.Completion) {
		return empty, errors.New("补全请求无效或会话已切换")
	}
	ctx, cancel := context.WithTimeout(parent, 300*time.Millisecond)
	defer cancel()
	tx, err := store.readDB().BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	q := scopedQuery{ctx: ctx, db: tx}
	if valid, e := sessionCurrentQ(q, sessionID, user.Name); e != nil || !valid {
		return empty, errCredentialsChanged
	}
	actor, err := mutationActorQ(q, user.Name)
	if err != nil {
		return empty, err
	}
	if scope.peer != "" {
		if _, err = userQ(q, scope.peer); err != nil {
			return empty, errors.New("私聊目标不可用")
		}
	} else if !canReadQ(q, actor.Name, scope.room) {
		return empty, errors.New("无权查看当前频道")
	}
	choices, err := completionDataQ(q, actor.Name, *request.Completion)
	if err != nil {
		return empty, err
	}
	if err = tx.Rollback(); err != nil {
		return empty, err
	}
	return guiResponse{Type: "completion", RequestID: request.RequestID, Room: scope.key(), Completion: &guiCompletionReceipt{Kind: request.Completion.Kind, Prefix: request.Completion.Prefix, Choices: choices}}, nil
}
