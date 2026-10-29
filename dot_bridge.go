package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

// Private inherited-pipe protocol, never a public listener or a GUI command.
// The process supervisor must connect this only to the separately authenticated
// adapter. No caller identity is accepted from MCP arguments; Python derives it
// after mTLS+OAuth checks and the product resolves the exact pre-provisioned link.
// This entry point is not started by main, and no plugin/credential is provisioned.
type dotBridgeRequest struct {
	ID             string                `json:"id"`
	Method         string                `json:"method"`
	Identity       dotAdapterIdentity    `json:"identity"`
	BindingRef     string                `json:"binding_ref,omitempty"`
	OperationRef   string                `json:"operation_ref,omitempty"`
	ReadRef        string                `json:"read_ref,omitempty"`
	ReplyRef       string                `json:"reply_ref,omitempty"`
	Content        string                `json:"content,omitempty"`
	IdempotencyKey string                `json:"idempotency_key,omitempty"`
	Subscription   *dotSubscriptionInput `json:"subscription,omitempty"`
	SubscriptionID string                `json:"subscription_id,omitempty"`
	CallbackHash   string                `json:"callback_hash,omitempty"`
	LeaseID        string                `json:"lease_id,omitempty"`
	Outcome        string                `json:"outcome,omitempty"`
}
type dotBridgeReply struct {
	ID     string `json:"id"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type dotSubscriptionPermit struct {
	dotBindingView
	AuthEpoch              int64  `json:"owner_auth_epoch"`
	RoomEpoch              int64  `json:"room_acl_epoch"`
	PreviousDeliveryHandle string `json:"previous_delivery_handle,omitempty"`
}

func (s *Store) dotResolveIdentity(ctx context.Context, issuer, subject string) (map[string]any, error) {
	var out map[string]any
	err := s.dotTransaction(ctx, func(q queryer) error {
		if e := dotFeatureQ(q); e != nil {
			return e
		}
		var name, profile, tenant string
		var uid, epoch, after int64
		var active int
		if e := q.QueryRow(`SELECT m.name,l.owner_uid,l.auth_epoch,l.profile_id,l.tokens_after,l.active FROM dot_oauth_links l JOIN user_meta m ON m.uid=l.owner_uid WHERE l.issuer=? AND l.subject=?`, issuer, subject).Scan(&name, &uid, &epoch, &profile, &after, &active); e != nil {
			return errDotUnavailable
		}
		u, e := mutationActorQ(q, name)
		if e != nil {
			return errDotUnavailable
		}
		if e = q.QueryRow(`SELECT value FROM config WHERE key='dot_installation_id'`).Scan(&tenant); e != nil || tenant == "" {
			return errDotUnavailable
		}
		if u.UID != uid || u.AuthEpoch != epoch || active != 1 || profile == "" {
			return errDotUnavailable
		}
		out = map[string]any{"tenant_id": tenant, "user_uid": strconv.FormatInt(uid, 10), "profile_id": profile, "auth_epoch": epoch, "tokens_valid_after": after, "active": true}
		return nil
	})
	return out, err
}
func (s *Store) dotBridgePermit(ctx context.Context, identity dotAdapterIdentity, ref string) (dotSubscriptionPermit, error) {
	var out dotSubscriptionPermit
	err := s.dotTransaction(ctx, func(q queryer) error {
		if e := dotFeatureQ(q); e != nil {
			return e
		}
		u, _, e := dotAdapterActorQ(q, identity)
		if e != nil {
			return e
		}
		b, e := dotBindingQ(q, ref)
		if e != nil || dotOwned(b, u) != nil {
			return errDotUnavailable
		}
		if e = dotLiveQ(q, b, b.Status == "ACTIVE"); e != nil {
			return e
		}
		if b.Status != "ACTIVE" && b.Status != "AWAITING_SUBSCRIPTION" {
			return errDotUnavailable
		}
		out = dotSubscriptionPermit{dotBindingView: b.dotBindingView, AuthEpoch: b.AuthEpoch, RoomEpoch: b.RoomEpoch}
		if b.Status == "ACTIVE" {
			if e = q.QueryRow(`SELECT delivery_handle FROM dot_subscriptions WHERE binding_ref=? AND generation=? AND active=1 AND expires>?`, ref, b.Generation, time.Now().Unix()).Scan(&out.PreviousDeliveryHandle); e != nil {
				return e
			}
		}
		return nil
	})
	return out, err
}
func (s *Store) dotBridgeDispatch(ctx context.Context, in dotBridgeRequest) (any, error) {
	switch in.Method {
	case "identity.resolve":
		return s.dotResolveIdentity(ctx, in.Identity.Issuer, in.Identity.Subject)
	case "profile":
		return s.dotAdapterProfile(ctx, in.Identity)
	case "bindings":
		b, e := s.dotAdapterBindings(ctx, in.Identity)
		return map[string]any{"bindings": b}, e
	case "context":
		return s.dotReadContext(ctx, in.Identity, in.ReadRef)
	case "propose":
		return s.dotProposeReply(ctx, in.Identity, in.ReplyRef, in.Content, in.IdempotencyKey)
	case "operation":
		return s.dotGetOperation(ctx, in.Identity, in.OperationRef)
	case "subscription.prepare":
		return s.dotBridgePermit(ctx, in.Identity, in.BindingRef)
	case "subscription.commit":
		if in.Subscription == nil {
			return nil, errDotUnavailable
		}
		return map[string]any{}, s.dotSubscribe(ctx, in.Identity, *in.Subscription)
	case "subscription.stop":
		return map[string]any{}, s.dotUnsubscribe(ctx, in.Identity, in.BindingRef, in.SubscriptionID, in.CallbackHash)
	case "delivery.bindings":
		return s.dotPendingBindings(ctx)
	case "delivery.claim":
		return s.dotClaimDelivery(ctx, in.BindingRef)
	case "delivery.finish":
		return s.dotFinishDelivery(ctx, in.LeaseID, in.Outcome)
	case "delivery.recover":
		return map[string]any{}, s.dotRecoverInterrupted(ctx)
	default:
		return nil, errDotUnavailable
	}
}
func (s *Store) serveDotPrivatePipes(ctx context.Context, input io.Reader, output io.Writer) error {
	if ctx == nil || input == nil || output == nil {
		return errDotUnavailable
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		if e := ctx.Err(); e != nil {
			return e
		}
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		var in dotBridgeRequest
		if e := decoder.Decode(&in); e != nil {
			return e
		}
		if in.ID == "" || len(in.ID) > 128 {
			return errDotUnavailable
		}
		var extra any
		if e := decoder.Decode(&extra); !errors.Is(e, io.EOF) {
			return errDotUnavailable
		}
		result, e := s.dotBridgeDispatch(ctx, in)
		reply := dotBridgeReply{ID: in.ID, Result: result}
		if e != nil {
			reply.Result = nil
			reply.Error = "PRODUCT_DENIED_OR_UNCERTAIN"
		}
		if e = encoder.Encode(reply); e != nil {
			return e
		}
	}
	return scanner.Err()
}

func (s *Store) dotPendingBindings(ctx context.Context) ([]string, error) {
	out := make([]string, 0)
	err := s.dotTransaction(ctx, func(q queryer) error {
		if e := dotFeatureQ(q); e != nil {
			return e
		}
		rows, e := q.Query(`SELECT b.ref FROM dot_bindings b WHERE b.status='ACTIVE' AND EXISTS(SELECT 1 FROM dot_events e WHERE e.binding_ref=b.ref AND e.status='PENDING' AND e.expires>? AND e.next_attempt<=?) ORDER BY b.ref LIMIT 64`, time.Now().Unix(), time.Now().Unix())
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var ref string
			if e = rows.Scan(&ref); e != nil {
				return e
			}
			out = append(out, ref)
		}
		return rows.Err()
	})
	return out, err
}
