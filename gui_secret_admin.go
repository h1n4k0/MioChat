package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type guiSecretAdminRequest struct {
	Kind     string `json:"kind"`
	Target   string `json:"target"`
	Duration string `json:"duration"`
	Uses     int    `json:"uses"`
}
type guiSecretAdminReceipt struct {
	Kind            string `json:"kind"`
	RequestedTarget string `json:"requested_target"`
	Target          string `json:"target"`
	Duration        string `json:"duration"`
	Uses            int    `json:"uses"`
	Expires         int64  `json:"expires"`
	State           string `json:"state"`
	Committed       *bool  `json:"committed,omitempty"`
	Value           string `json:"value,omitempty"`
}

func (r guiSecretAdminReceipt) String() string {
	return fmt.Sprintf("guiSecretAdminReceipt{kind:%s state:%s}", r.Kind, r.State)
}
func (r guiSecretAdminReceipt) GoString() string { return r.String() }

func guiSecretAdminRequestAction(ctx context.Context, store *Store, hub *Hub, user User, scope guiScope, sessionID string, request guiRequest) (guiResponse, error) {
	in := request.SecretAdmin
	if in == nil || request.Type != "secret_admin" || request.Maintenance != nil || !guiMaintenanceOnly(request, scope) || ctx == nil || sessionID == "" {
		return guiResponse{}, errors.New("无效的一次性管理凭据请求")
	}
	var out secretAdminOutcome
	var err error
	switch in.Kind {
	case "resetpass":
		if strings.TrimSpace(in.Target) == "" || len(in.Target) > 256 || in.Duration != "" || in.Uses != 0 {
			return guiResponse{}, errors.New("重置密码须明确单个目标账户")
		}
		out, err = store.ResetPasswordForSession(ctx, user.Name, in.Target, sessionID)
	case "invitecode":
		if in.Target != "" || in.Duration == "" || len(in.Duration) > 64 {
			return guiResponse{}, errors.New("邀请码须明确期限和使用次数")
		}
		out, err = store.InviteCodeForSession(ctx, user.Name, in.Duration, in.Uses, sessionID)
	default:
		return guiResponse{}, errors.New("未知的一次性管理凭据操作")
	}
	receipt := &guiSecretAdminReceipt{Kind: in.Kind, RequestedTarget: in.Target, Duration: in.Duration, Uses: in.Uses}
	response := guiResponse{Type: "secret_admin", RequestID: request.RequestID, Room: scope.key(), SecretAdmin: receipt}
	if err != nil {
		var uncertain secretAdminCommitError
		if errors.As(err, &uncertain) {
			receipt.State = "unknown"
			return response, nil
		}
		return guiResponse{}, err
	}
	// Applied policy is independent of ACK delivery, including a blocked writer.
	if out.kind == "resetpass" {
		hub.Enforce()
	}
	committed := true
	receipt.Committed = &committed
	receipt.Target, receipt.Expires = out.target, out.expires
	receipt.State = "committed_hidden"
	if store.secretAdminDeliveryAllowed(ctx, out, sessionID) {
		receipt.State, receipt.Value = "shown", out.secret
	}
	return response, nil
}
