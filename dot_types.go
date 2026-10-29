package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const dotAssistantSender = "[dot]" // impossible validName: cannot impersonate a human login
const dotEventTTL int64 = 900
const dotMaxBodyRunes = 800 // matches the current product's message size, without silent truncation

var errDotDisabled = errors.New("dot 助手默认关闭；尚未配置真实连接")
var errDotUnavailable = errors.New("dot 请求不可用、已过期或当前无权访问")
var errDotStorage = errors.New("dot 必要写入或事务读回不一致；已回滚，请勿自动重发")

type dotPolicyDenied struct{ reason string }

func (e *dotPolicyDenied) Error() string { return "dot 已暂停：" + e.reason }

type guiDotRequest struct {
	Operation          string `json:"operation"`
	Room               string `json:"room"`
	BindingRef         string `json:"binding_ref,omitempty"`
	OperationRef       string `json:"operation_ref,omitempty"`
	ExpectedGeneration *int64 `json:"expected_generation,omitempty"`
	ReviewToken        string `json:"review_token,omitempty"`
	BodySHA256         string `json:"body_sha256,omitempty"`
	AudienceSHA256     string `json:"audience_sha256,omitempty"`
	SourceID           int64  `json:"source_id,omitempty"`
	OwnerOrigin        string `json:"owner_origin"`
	EditGeneration     uint64 `json:"edit_generation"`
	Confirmed          bool   `json:"confirmed,omitempty"`
}

type dotBindingView struct {
	Ref        string `json:"binding_ref"`
	Room       string `json:"room"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	Generation int64  `json:"generation"`
	OwnerUID   int64  `json:"owner_uid"`
}

type dotOperationView struct {
	Ref        string `json:"operation_ref"`
	BindingRef string `json:"binding_ref"`
	Status     string `json:"status"`
	SourceID   int64  `json:"source_id"`
	MessageID  int64  `json:"message_id,omitempty"`
}

type dotReviewView struct {
	dotOperationView
	Room           string  `json:"room"`
	Body           string  `json:"body"`
	BodySHA256     string  `json:"body_sha256"`
	AudienceSHA256 string  `json:"audience_sha256"`
	AudienceLabel  string  `json:"audience_label"`
	AudienceUIDs   []int64 `json:"audience_uids"`
	Generation     int64   `json:"generation"`
	OwnerUID       int64   `json:"owner_uid"`
	ReviewToken    string  `json:"review_token"`
	Expires        int64   `json:"expires"`
}

type guiDotReceipt struct {
	Operation      string             `json:"operation"`
	OwnerOrigin    string             `json:"owner_origin"`
	EditGeneration uint64             `json:"edit_generation"`
	Bindings       []dotBindingView   `json:"bindings,omitempty"`
	Binding        *dotBindingView    `json:"binding,omitempty"`
	Pending        []dotOperationView `json:"pending,omitempty"`
	Review         *dotReviewView     `json:"review,omitempty"`
	Result         *dotOperationView  `json:"result,omitempty"`
}

type dotBinding struct {
	dotBindingView
	OwnerName                                string
	AuthEpoch, RoomEpoch, Watermark, Created int64
}

type dotOperation struct {
	dotOperationView
	Generation                                               int64
	EventRef, Idem, Body, BodyHash, SourceHash, AudienceHash string
	RoomEpoch, Created, Expires                              int64
}

type dotEvent struct {
	Ref, BindingRef, SourceHash, ReadRef, ReplyRef, Status, LeaseID string
	Generation, SourceID, Created, Expires, Attempts, NextAttempt   int64
}

// Constructed ONLY after the separate mTLS+OAuth adapter checks. No GUI JSON
// endpoint accepts these fields. Every use re-resolves the explicit link in SQL.
type dotAdapterIdentity struct {
	Issuer   string `json:"issuer"`
	Subject  string `json:"subject"`
	IssuedAt int64  `json:"issued_at"`
	Expires  int64  `json:"expires"`
}

type dotSubscriptionInput struct {
	ID             string `json:"id"`
	BindingRef     string `json:"binding_ref"`
	CallbackHash   string `json:"callback_hash"`
	DeliveryHandle string `json:"delivery_handle"`
	Generation     int64  `json:"generation"`
	Expires        int64  `json:"expires"`
}

type dotDelivery struct {
	LeaseID        string `json:"lease_id"`
	EventID        string `json:"event_id"`
	SubscriptionID string `json:"subscription_id"`
	DeliveryHandle string `json:"delivery_handle"`
	Body           []byte `json:"body"`
}

func dotRef(prefix string) (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(b[:]), nil
}
func dotHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
