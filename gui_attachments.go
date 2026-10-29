package main

import (
	"context"
	"errors"
	"reflect"
	"time"
)

func attachmentRequestValid(in guiAttachmentRequest) bool {
	x := in
	x.Op = ""
	switch in.Op {
	case "intent":
		x.IntentKey = ""
		x.Filename = ""
		x.MIME = ""
		x.Size = 0
		x.SHA256 = ""
		x.Caption = ""
	case "chunk":
		if !attachmentIDValid(in.UploadID) {
			return false
		}
		x.UploadID = ""
		x.Offset = 0
		x.DataB64 = ""
	case "commit", "cancel":
		if !attachmentIDValid(in.UploadID) {
			return false
		}
		x.UploadID = ""
	case "status":
		if (in.UploadID == "") == (in.IntentKey == "") {
			return false
		}
		if in.UploadID != "" && !attachmentIDValid(in.UploadID) {
			return false
		}
		if in.IntentKey != "" && !attachmentKeyValid(in.IntentKey) {
			return false
		}
		x.UploadID = ""
		x.IntentKey = ""
	case "fetch":
		if in.MessageID <= 0 || in.Offset < 0 {
			return false
		}
		x.MessageID = 0
		x.Offset = 0
	case "reference":
		if in.MessageID <= 0 || !attachmentKeyValid(in.IntentKey) {
			return false
		}
		x.MessageID = 0
		x.IntentKey = ""
	case "limits":
	default:
		return false
	}
	return x == (guiAttachmentRequest{})
}

func attachmentRequestOnly(r guiRequest) bool {
	r.Type = ""
	r.RequestID = ""
	r.Room = ""
	r.Attachment = nil
	r.ctx = nil
	r.sessionID = ""
	r.connectionIP = ""
	r.communityRoom = ""
	return reflect.DeepEqual(r, guiRequest{})
}

func attachmentErrorReceipt(in *guiAttachmentRequest, scope, code string) *guiAttachmentReceipt {
	r := &guiAttachmentReceipt{Status: "error", Code: code, Room: scope}
	if in != nil {
		switch in.Op {
		case "intent", "chunk", "commit", "cancel", "status", "fetch", "limits", "reference":
			r.Op = in.Op
		}
		r.IntentKey = in.IntentKey
		r.UploadID = in.UploadID
		if in.Op == "reference" {
			r.SourceMessageID = in.MessageID
		}
	}
	return r
}

func guiAttachmentOperation(ctx context.Context, store *Store, user User, scope guiScope, session string, request guiRequest) guiResponse {
	out := guiResponse{Type: "attachment", RequestID: request.RequestID, Room: scope.key()}
	receipt := *attachmentErrorReceipt(request.Attachment, scope.key(), "request_invalid")
	out.Attachment = &receipt
	if request.Type != "attachment" || request.Attachment == nil || request.Room != scope.key() || !attachmentRequestOnly(request) || !attachmentRequestValid(*request.Attachment) {
		return out
	}
	receipt, err := store.AttachmentForSession(ctx, user, session, scope.key(), *request.Attachment)
	if err != nil {
		code := "storage_unavailable"
		var typed attachmentError
		if errors.As(err, &typed) {
			code = typed.code
		} else if errors.Is(err, errCredentialsChanged) {
			code = "session_changed"
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			code = "interrupted"
		}
		receipt = *attachmentErrorReceipt(request.Attachment, scope.key(), code)
	}
	out.Attachment = &receipt
	return out
}

// Attachment transfer has a separate bounded frame budget; it must not exhaust
// the chat/navigation bucket. A fixed-size chunk implies <=815 writes/item.
type attachmentFrameBucket struct {
	tokens float64
	at     time.Time
}

func (b *attachmentFrameBucket) Allow() bool {
	now := time.Now()
	if b.at.IsZero() {
		b.tokens = 16
	} else {
		b.tokens = min(16, b.tokens+now.Sub(b.at).Seconds()*32)
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
