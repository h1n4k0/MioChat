package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

const guiSubsystemName = "miochat-gui-v1"
const guiProtocolVersion = 1
const guiMaxFrameBytes = 64 * 1024

type guiRequest struct {
	Dot          *guiDotRequest         `json:"dot,omitempty"`
	Attachment   *guiAttachmentRequest  `json:"attachment,omitempty"`
	Completion   *guiCompletionRequest  `json:"completion,omitempty"`
	SecretAdmin  *guiSecretAdminRequest `json:"secret_admin,omitempty"`
	Maintenance  *guiMaintenanceRequest `json:"maintenance,omitempty"`
	Composition  *guiCompositionRequest `json:"composition,omitempty"`
	SelfIPForce  *bool                  `json:"self_ip_force,omitempty"`
	ReadAfter    int64                  `json:"read_after,omitempty"`
	ReadUntil    *int64                 `json:"read_until,omitempty"`
	WordMode     string                 `json:"word_mode,omitempty"`
	GameID       int64                  `json:"game_id,omitempty"`
	TicVersion   *int64                 `json:"tic_version,omitempty"`
	ExpectedRoom string                 `json:"expected_room,omitempty"`
	Position     *guiViewPosition       `json:"position,omitempty"`
	Version      int64                  `json:"version,omitempty"`
	BackupID     string                 `json:"backup_id,omitempty"`
	Type         string                 `json:"type"`
	RequestID    string                 `json:"request_id,omitempty"`
	Username     string                 `json:"username,omitempty"`
	Password     string                 `json:"password,omitempty"`
	NewPassword  string                 `json:"new_password,omitempty"`
	Confirm      string                 `json:"confirm,omitempty"`
	Invite       string                 `json:"invite,omitempty"`
	Room         string                 `json:"room,omitempty"`
	Body         string                 `json:"body,omitempty"`
	Before       int64                  `json:"before,omitempty"`
	ReplyTo      int64                  `json:"reply_to,omitempty"`
	// Internal connection context; encoding/json cannot populate this field.
	connectionIP  string
	communityRoom string
	sessionID     string
	ctx           context.Context
}

type guiRoom struct {
	Name  string `json:"name"`
	Topic string `json:"topic"`
}

type guiMessage struct {
	Attachment *attachmentInfo `json:"attachment,omitempty"`
	ID         int64           `json:"id"`
	Room       string          `json:"room"`
	Sender     string          `json:"sender"`
	Recipient  string          `json:"recipient,omitempty"`
	Body       string          `json:"body"`
	Kind       string          `json:"kind"`
	At         int64           `json:"at"`
	Edited     int64           `json:"edited,omitempty"`
	Recalled   int64           `json:"recalled,omitempty"`
	ReplyTo    int64           `json:"reply_to,omitempty"`
	ReplyName  string          `json:"reply_name,omitempty"`
	ReplyBody  string          `json:"reply_body,omitempty"`
	Reactions  string          `json:"reactions,omitempty"`
}

type guiDirectTarget struct {
	Requested string `json:"requested"`
	Canonical string `json:"canonical"`
}

type guiResponse struct {
	Dot                  *guiDotReceipt           `json:"dot,omitempty"`
	Attachment           *guiAttachmentReceipt    `json:"attachment,omitempty"`
	Completion           *guiCompletionReceipt    `json:"completion,omitempty"`
	SecretAdmin          *guiSecretAdminReceipt   `json:"secret_admin,omitempty"`
	Maintenance          *guiMaintenanceReceipt   `json:"maintenance,omitempty"`
	Composition          *guiCompositionReceipt   `json:"composition,omitempty"`
	ManagementReceipt    *guiManagementReceipt    `json:"management_receipt,omitempty"`
	ConfigurationReceipt *guiConfigurationReceipt `json:"configuration_receipt,omitempty"`
	SanctionReceipt      *guiSanctionReceipt      `json:"sanction_receipt,omitempty"`
	ReportReceipt        *guiReportReceipt        `json:"report_receipt,omitempty"`
	WordReceipt          *guiWordReceipt          `json:"word_receipt,omitempty"`
	GuessReceipt         *guiGuessReceipt         `json:"guess_receipt,omitempty"`
	TicReceipt           *guiTicReceipt           `json:"tic_receipt,omitempty"`
	CommunityReceipt     *guiCommunityReceipt     `json:"community_receipt,omitempty"`
	DirectTarget         *guiDirectTarget         `json:"direct_target,omitempty"`
	ClearView            bool                     `json:"clear_view,omitempty"`
	Mention              string                   `json:"mention,omitempty"`
	Profile              *guiProfile              `json:"profile,omitempty"`
	Center               *guiCenter               `json:"center,omitempty"`
	Preferences          *guiPreferences          `json:"preferences,omitempty"`
	KeyReminder          bool                     `json:"key_reminder,omitempty"`
	DeviceKey            *guiDeviceKey            `json:"device_key,omitempty"`
	DraftChanged         bool                     `json:"draft_changed,omitempty"`
	AtLatest             bool                     `json:"at_latest"`
	Draft                *guiDraft                `json:"draft,omitempty"`
	Type                 string                   `json:"type"`
	RequestID            string                   `json:"request_id,omitempty"`
	Protocol             int                      `json:"protocol,omitempty"`
	RegistrationMode     string                   `json:"registration_mode,omitempty"`
	User                 string                   `json:"user,omitempty"`
	Room                 string                   `json:"room,omitempty"`
	Rooms                []guiRoom                `json:"rooms,omitempty"`
	Messages             []guiMessage             `json:"messages,omitempty"`
	Message              *guiMessage              `json:"message,omitempty"`
	Online               int                      `json:"online,omitempty"`
	Status               string                   `json:"status,omitempty"`
	Error                string                   `json:"error,omitempty"`
	Panel                *guiPanel                `json:"panel,omitempty"`
	Form                 string                   `json:"form,omitempty"`
	ReplyTo              int64                    `json:"reply_to,omitempty"`
	Replace              bool                     `json:"replace,omitempty"`
	Capabilities         []string                 `json:"capabilities,omitempty"`
}

type guiWriter struct {
	mu sync.Mutex
	e  *json.Encoder
}

func (w *guiWriter) send(response guiResponse) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.e.Encode(response)
}

func guiMessageFrom(message Message) guiMessage {
	return guiMessage{Attachment: message.Attachment, ID: message.ID, Room: message.Room, Sender: message.Sender, Recipient: message.Recipient, Body: message.Body, Kind: message.Kind, At: message.At, Edited: message.Edited, Recalled: message.Recalled, ReplyTo: message.ReplyTo, ReplyName: message.ReplyName, ReplyBody: message.ReplyBody, Reactions: message.ReactionSummary}
}

func runGUIProtocol(ctx context.Context, input io.Reader, output io.Writer, ip, clientVersion string, closeFn func(), store *Store, hub *Hub) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hub.Attach(store)
	writer := &guiWriter{e: json.NewEncoder(output)}
	maintenance := newGUIMaintenance()
	defer maintenance.stop()
	if err := writer.send(guiResponse{Type: "hello", Protocol: guiProtocolVersion, DeviceKey: guiDeviceKeyInfo(store, guiSignedKey(ctx)), Capabilities: []string{"native-actions-v1", "account-security-v1", "draft-position-v1", "device-keys-v1", "preferences-v1", "control-center-v1", "profile-v1", "presence-v1", "room-management-v1", "composition-cas-v1", "maintenance-jobs-v1", "secret-admin-v1", "completion-v1", attachmentCapability, attachmentReferenceCapability, "dot-owner-review-v1"}, RegistrationMode: store.Config("registration"), Status: "authentication_required"}); err != nil {
		return err
	}
	requests := make(chan guiRequest, 8)
	readErrors := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), guiMaxFrameBytes)
		for scanner.Scan() {
			var request guiRequest
			if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
				readErrors <- fmt.Errorf("invalid JSON request: %w", err)
				return
			}
			select {
			case requests <- request:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			readErrors <- err
		} else {
			readErrors <- io.EOF
		}
	}()

	var user User
	var client *Client
	scope := guiScope{room: "lobby"}
	lastCommunityRoom := "lobby"
	var replyTo int64
	joined := false
	lastPreferences := ""
	centerSubscribed := false
	lastCenter := ""
	profileTarget := ""
	lastProfile := ""
	var limiter bucket
	var attachmentLimiter attachmentFrameBucket
	var passwordVerifiedAt time.Time
	defer func() {
		if joined {
			hub.Leave(client)
		}
	}()

	sendError := func(requestID string, err error) {
		_ = writer.send(guiResponse{Type: "error", RequestID: requestID, Error: err.Error()})
	}
	sendPreferences := func(requestID string, force bool) error {
		p, err := guiPreferencesSnapshot(store, user.Name)
		if err != nil {
			return err
		}
		signature := guiPreferenceSignature(p)
		if !force && signature == lastPreferences {
			return nil
		}
		lastPreferences = signature
		return writer.send(guiResponse{Type: "preferences", RequestID: requestID, Preferences: p})
	}
	sendCenter := func(requestID string, force bool) error {
		if !centerSubscribed {
			return nil
		}
		p, err := guiCenterSnapshot(store, user.Name)
		if err != nil {
			return err
		}
		signature := guiCenterSignature(p)
		if !force && signature == lastCenter {
			return nil
		}
		lastCenter = signature
		return writer.send(guiResponse{Type: "control_center", RequestID: requestID, Center: p})
	}
	sendProfile := func(requestID string, force bool) error {
		if profileTarget == "" {
			return nil
		}
		p, err := guiProfileSnapshot(store, hub, user.Name, profileTarget)
		if err != nil {
			profileTarget = ""
			lastProfile = ""
			if force {
				return err
			}
			return writer.send(guiResponse{Type: "profile_unavailable", Status: "资料已不可用"})
		}
		b, _ := json.Marshal(p)
		signature := string(b)
		if !force && signature == lastProfile {
			return nil
		}
		lastProfile = signature
		return writer.send(guiResponse{Type: "profile", RequestID: requestID, Profile: p})
	}
	sendState := func(requestID string, rosterOnly bool) error {
		if joined {
			refreshed, ok := hub.RefreshClient(client)
			if !ok {
				return errors.New("session is no longer authorized")
			}
			user = refreshed
			maintenance.revalidate(user)
		}
		if user.MustReset {
			return writer.send(guiResponse{Type: "reset_required", RequestID: requestID, User: user.Name, Status: "password_reset_required"})
		}
		status := ""
		if _, err := guiResolveScope(store, user.Name, scope.key()); err != nil {
			scope = guiScope{room: "lobby"}
			lastCommunityRoom = scope.room
			replyTo = 0
			hub.Move(client, scope.room, scope.peer)
			status = "room_access_changed"
		}
		rooms, err := store.VisibleRooms(user.Name, false)
		if err != nil {
			return err
		}
		var history []Message
		if !rosterOnly || status != "" {
			history, err = store.History(scope.room, user.Name, scope.peer, 100)
			if err != nil {
				return err
			}
		} else {
			status = "roster"
		}
		response := guiResponse{Type: "state", RequestID: requestID, User: user.Name, Room: scope.key(), ReplyTo: replyTo, Online: len(hub.Online()), Status: status, AtLatest: true}
		response.Preferences, err = guiPreferencesSnapshot(store, user.Name)
		if err != nil {
			return err
		}
		lastPreferences = guiPreferenceSignature(response.Preferences)
		for _, room := range rooms {
			response.Rooms = append(response.Rooms, guiRoom{Name: room.Name, Topic: room.Topic})
		}
		for _, peer := range store.Recent(user.Name) {
			response.Rooms = append(response.Rooms, guiRoom{Name: "dm:" + peer, Topic: "私信"})
		}
		if scope.peer != "" {
			found := false
			for _, room := range response.Rooms {
				found = found || room.Name == scope.key()
			}
			if !found {
				response.Rooms = append(response.Rooms, guiRoom{Name: scope.key(), Topic: "私信"})
			}
		}
		for _, message := range history {
			response.Messages = append(response.Messages, guiMessageForViewer(message, user.Name))
		}
		return writer.send(response)
	}

	health := time.NewTicker(3 * time.Second)
	defer health.Stop()
	for {
		var events <-chan Event
		if joined {
			events = client.Events
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErrors:
			return err
		case result := <-maintenance.results:
			response, valid := maintenance.finish(ctx, store, result)
			if valid {
				if err := writer.send(response); err != nil {
					return err
				}
			}
		case request := <-requests:
			if len(request.RequestID) == 0 || len(request.RequestID) > 128 {
				return errors.New("request_id must contain 1-128 bytes")
			}
			if request.Attachment != nil && request.Type != "attachment" {
				sendError(request.RequestID, errors.New("附件请求不能混入其它操作"))
				continue
			}
			if (request.Dot != nil && request.Type != "dot") || (request.Attachment != nil && request.Type != "attachment") {
				sendError(request.RequestID, errors.New("typed request 与 operation 不匹配"))
				continue
			}
			if request.Completion != nil && request.Type != "completion" {
				sendError(request.RequestID, errors.New("补全请求不能混入其它操作"))
				continue
			}
			if request.SecretAdmin != nil && request.Type != "secret_admin" {
				sendError(request.RequestID, errors.New("一次性管理凭据请求不能混入其它操作"))
				continue
			}
			if request.Maintenance != nil && request.Type != "maintenance" {
				sendError(request.RequestID, errors.New("维护请求不能混入其它操作"))
				continue
			}
			// One authoritative gate covers all authenticated operations,
			// including actions added in future. Never trust a client role/actor.
			if request.Type != "login" && request.Type != "register" && request.Type != "login_key" && request.Type != "disconnect" {
				if !joined {
					sendError(request.RequestID, errors.New("authentication required"))
					continue
				}
				refreshed, ok := hub.RefreshClient(client)
				if !ok {
					sendError(request.RequestID, errors.New("session is no longer authorized"))
					return nil
				}
				user = refreshed
				maintenance.revalidate(user)
				if user.MustReset && request.Type != "change_password" {
					sendError(request.RequestID, errors.New("请先修改临时密码；当前仅可改密或断开连接"))
					continue
				}
			}
			switch request.Type {
			case "login", "register", "login_key":
				if joined {
					sendError(request.RequestID, errors.New("already authenticated"))
					continue
				}
				register := request.Type == "register"
				viaKey := request.Type == "login_key"
				if !hub.authAllowed(ip, register) {
					sendError(request.RequestID, errors.New("尝试过于频繁，请稍后再试"))
					continue
				}
				select {
				case hub.AuthSlots <- struct{}{}:
				case <-ctx.Done():
					return ctx.Err()
				}
				var err error
				user = User{}
				if viaKey {
					if key := guiSignedKey(ctx); key != nil {
						user, err = store.AuthenticateSSHKey(key)
					} else {
						err = errors.New("本次连接没有已签名的设备密钥")
					}
				} else if register {
					user, err = store.RegisterInvite(request.Username, request.Password, request.Confirm, request.Invite, ip, clientVersion)
				} else {
					user, err = store.Authenticate(request.Username, request.Password)
				}
				<-hub.AuthSlots
				loginName := request.Username
				if viaKey {
					loginName = user.Name
				}
				store.RecordLogin(loginName, ip, clientVersion+"/gpui", err == nil)
				if err != nil {
					sendError(request.RequestID, err)
					continue
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				id, err := secretToken()
				if err != nil {
					sendError(request.RequestID, err)
					continue
				}
				client = &Client{Name: user.Name, Room: scope.room, Peer: scope.peer, Events: make(chan Event, 256), Close: closeFn, ID: id, IP: ip, Version: clientVersion + "/gpui", AuthEpoch: user.AuthEpoch, Ctx: ctx}
				if err = hub.Join(client); err != nil {
					sendError(request.RequestID, err)
					continue
				}
				if viaKey {
					if err = store.AttachSSHSessionKey(client.ID, user.Name, guiSignedKey(ctx)); err != nil {
						hub.Leave(client)
						sendError(request.RequestID, err)
						continue
					}
				} else {
					passwordVerifiedAt = time.Now()
				}
				joined = true
				if state, ok := ctx.Value(sshTransportKey{}).(*sshTransportState); ok {
					state.authenticated.Store(true)
				}
				authStatus := "authenticated"
				if user.MustReset {
					authStatus = "password_reset_required"
				}
				var preferences *guiPreferences
				if !user.MustReset {
					preferences, err = guiPreferencesSnapshot(store, user.Name)
					if err != nil {
						return err
					}
				}
				if err = writer.send(guiResponse{Type: "auth", RequestID: request.RequestID, User: user.Name, Status: authStatus, Preferences: preferences, KeyReminder: !viaKey && preferences.reminder()}); err != nil {
					return err
				}
				if err = sendState("", false); err != nil {
					return err
				}
			case "profile":
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("rate limited"))
					continue
				}
				target := request.Body
				if target == "" {
					target = user.Name
				}
				if _, err := store.Resolve(target); err != nil {
					sendError(request.RequestID, err)
					continue
				}
				profileTarget = target
				if err := sendProfile(request.RequestID, true); err != nil {
					sendError(request.RequestID, err)
				}
			case "control_center":
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("rate limited"))
					continue
				}
				centerSubscribed = true
				if err := sendCenter(request.RequestID, true); err != nil {
					sendError(request.RequestID, err)
				}
			case "preferences":
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("rate limited"))
					continue
				}
				if err := sendPreferences(request.RequestID, true); err != nil {
					sendError(request.RequestID, err)
				}
			case "bind_key":
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("操作过于频繁，请稍后重试"))
					continue
				}
				response, err := guiBindDeviceKey(ctx, store, hub, user, client, &passwordVerifiedAt, request)
				if err != nil {
					sendError(request.RequestID, err)
					continue
				}
				if err = writer.send(response); err != nil {
					return err
				}
				hub.PreferencesChanged(user.Name)
			case "change_password":
				if !limiter.Allow() || !hub.authAllowed(ip, false) {
					sendError(request.RequestID, errors.New("操作过于频繁，请稍后再试"))
					continue
				}
				if request.NewPassword != request.Confirm {
					sendError(request.RequestID, errors.New("新密码不一致"))
					continue
				}
				select {
				case hub.AuthSlots <- struct{}{}:
				case <-ctx.Done():
					return ctx.Err()
				}
				err := store.ChangePasswordForSessionContext(ctx, user.Name, request.Password, request.NewPassword, client.ID)
				<-hub.AuthSlots
				if err != nil {
					sendError(request.RequestID, err)
					continue
				}
				refreshed, err := store.User(user.Name)
				if err != nil {
					return err
				}
				user = refreshed
				maintenance.revalidate(user)
				client.AuthEpoch = user.AuthEpoch
				passwordVerifiedAt = time.Now()
				hub.Enforce()
				if err := writer.send(guiResponse{Type: "password_changed", RequestID: request.RequestID, User: user.Name, Status: "authenticated"}); err != nil {
					return err
				}
				if err := sendState("", false); err != nil {
					return err
				}
			case "select_room":
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("操作过于频繁，请稍候"))
					continue
				}
				if !joined {
					sendError(request.RequestID, errors.New("authentication required"))
					continue
				}
				if refreshed, ok := hub.RefreshClient(client); !ok {
					sendError(request.RequestID, errors.New("会话已失效，请重新登录"))
					return nil
				} else {
					user = refreshed
					maintenance.revalidate(user)
				}
				nextScope, scopeErr := guiResolveScope(store, user.Name, request.Room)
				if scopeErr != nil {
					sendError(request.RequestID, errors.New("你没有频道权限"))
					continue
				}
				scope = nextScope
				if scope.peer == "" {
					lastCommunityRoom = scope.room
				}
				replyTo = 0
				hub.Move(client, scope.room, scope.peer)
				if err := sendState(request.RequestID, false); err != nil {
					sendError(request.RequestID, err)
				}
			case "send_message":
				if !joined {
					sendError(request.RequestID, errors.New("authentication required"))
					continue
				}
				if request.Room != scope.key() {
					sendError(request.RequestID, errors.New("会话已切换，请在当前频道重试"))
					continue
				}
				if refreshed, ok := hub.RefreshClient(client); !ok {
					sendError(request.RequestID, errors.New("会话已失效，请重新登录"))
					return nil
				} else {
					user = refreshed
					maintenance.revalidate(user)
				}
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("操作过于频繁，请稍候"))
					continue
				}
				if utf8.RuneCountInString(request.Body) > 800 {
					sendError(request.RequestID, errors.New("单条消息最多 800 个字符"))
					continue
				}
				if _, err := guiResolveScope(store, user.Name, scope.key()); err != nil {
					sendError(request.RequestID, err)
					_ = sendState("", false)
					continue
				}
				message, err := store.SendForSessionContext(ctx, Message{Room: scope.room, Recipient: scope.peer, Sender: user.Name, Body: request.Body, Kind: "chat", ReplyTo: request.ReplyTo}, client.ID)
				if err != nil {
					sendError(request.RequestID, err)
					continue
				}
				hub.Broadcast(message)
				replyTo = 0
				response := guiResponse{Type: "sent", RequestID: request.RequestID, Message: ptrGUIMessage(guiMessageForViewer(message, user.Name))}
				// A send is already committed. Clearing a matching saved draft
				// is conditional; never erase a concurrent device's update or
				// return a send error that would invite duplicate resubmission.
				dbScope := scopeFor(scope.room, scope.peer)
				saved, readErr := store.PositionWithError(user.Name, dbScope)
				if readErr != nil {
					response.Status = "消息已发送；草稿状态暂未确认"
					response.DraftChanged = true
				} else if saved.Draft != "" {
					if saved.Version == request.Version && saved.Draft == cleanPositionDraft(request.Body) {
						saved.Draft = ""
						saved.ReplyTo = 0
						version, saveErr := store.SavePositionVersioned(user.Name, dbScope, saved)
						if saveErr == nil {
							saved.Version = version
							response.Draft = guiDraftFrom(scope, saved)
						} else {
							response.Status = "消息已发送；草稿更新仍保留，请稍后确认"
							response.DraftChanged = true
						}
					} else {
						response.Status = "消息已发送；另一设备的草稿更新已保留"
						response.DraftChanged = true
					}
				}
				if err := writer.send(response); err != nil {
					return err
				}
			case "attachment":
				if !attachmentLimiter.Allow() || (request.Attachment != nil && request.Attachment.Op == "reference" && !limiter.Allow()) {
					if err := writer.send(guiResponse{Type: "attachment", RequestID: request.RequestID, Room: scope.key(), Attachment: attachmentErrorReceipt(request.Attachment, scope.key(), "rate_limited")}); err != nil {
						return err
					}
					continue
				}
				response := guiAttachmentOperation(ctx, store, user, scope, client.ID, request)
				if response.Attachment != nil && response.Attachment.Status == "committed" && (response.Attachment.Op == "commit" || response.Attachment.Op == "status" || response.Attachment.Op == "reference") {
					if message, err := store.Message(user.Name, response.Attachment.MessageID); err == nil && message.Recalled == 0 && message.Attachment != nil {
						response.Message = ptrGUIMessage(guiMessageForViewer(message, user.Name))
						if response.Attachment.NewCommit {
							hub.Broadcast(message)
						}
					}
				}
				if err := writer.send(response); err != nil {
					return err
				}
			case "dot":
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("操作过于频繁，请稍后重试"))
					continue
				}
				response, err := guiDotRequestAction(ctx, store, hub, user, scope, client.ID, request)
				if err != nil {
					sendError(request.RequestID, err)
					continue
				}
				if err = writer.send(response); err != nil {
					return err
				}
			case "completion":
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("操作过于频繁，请稍后重试"))
					continue
				}
				response, err := guiCompletePrefix(ctx, store, user, scope, client.ID, request)
				if err != nil {
					sendError(request.RequestID, err)
					continue
				}
				if err = writer.send(response); err != nil {
					return err
				}
			case "secret_admin":
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("操作过于频繁，请稍后重试"))
					continue
				}
				response, err := guiSecretAdminRequestAction(ctx, store, hub, user, scope, client.ID, request)
				if err != nil {
					sendError(request.RequestID, err)
					continue
				}
				if err = writer.send(response); err != nil {
					return err
				}
			case "maintenance":
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("操作过于频繁，请稍后重试"))
					continue
				}
				response, err := maintenance.request(ctx, store, user, scope, client.ID, request)
				if err != nil {
					sendError(request.RequestID, err)
					continue
				}
				if err = writer.send(response); err != nil {
					return err
				}
			case "composition":
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("操作过于频繁，请稍后重试"))
					continue
				}
				response, err := guiCompositionRequestAction(ctx, store, hub, user, scope, client.ID, request)
				if err != nil {
					sendError(request.RequestID, err)
					continue
				}
				if err = writer.send(response); err != nil {
					return err
				}
			case "draft_suspend", "view_window", "view_newer":
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("操作过于频繁，请稍后重试"))
					continue
				}
				var response guiResponse
				var err error
				if request.Type == "draft_suspend" {
					response, err = guiSuspendDraft(store, user, client.ID, request)
				} else {
					response, err = guiReadPositionWindow(store, user, scope, request)
				}
				if err != nil {
					sendError(request.RequestID, err)
					continue
				}
				if err := writer.send(response); err != nil {
					return err
				}
			case "draft_load", "draft_save", "draft_keep":
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("操作过于频繁，请稍后重试"))
					continue
				}
				response, err := guiDraftRequest(store, user, scope, client.ID, request)
				if err != nil {
					sendError(request.RequestID, err)
					continue
				}
				if err := writer.send(response); err != nil {
					return err
				}
			case "command":
				if !joined {
					sendError(request.RequestID, errors.New("authentication required"))
					continue
				}
				refreshed, valid := hub.RefreshClient(client)
				if !valid {
					return errors.New("session is no longer authorized")
				}
				user = refreshed
				maintenance.revalidate(user)
				if user.MustReset {
					sendError(request.RequestID, errors.New("password reset required; use the SSH terminal client"))
					continue
				}
				if request.Room != scope.key() {
					sendError(request.RequestID, errors.New("对话已切换，请重试"))
					continue
				}
				if _, err := guiResolveScope(store, user.Name, scope.key()); err != nil {
					sendError(request.RequestID, err)
					_ = sendState("", false)
					continue
				}
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("操作过于频繁，请稍候"))
					continue
				}
				request.communityRoom = lastCommunityRoom
				request.connectionIP = client.IP
				request.sessionID = client.ID
				request.ctx = ctx
				result, err := guiExecuteAction(store, hub, user, scope, replyTo, request)
				if err != nil {
					sendError(request.RequestID, err)
					continue
				}
				moved := result.scope != scope
				scope, replyTo = result.scope, result.replyTo
				if scope.peer == "" {
					lastCommunityRoom = scope.room
				}
				if moved {
					hub.Move(client, scope.room, scope.peer)
				}
				response := guiResponse{Type: "action", RequestID: request.RequestID, Room: scope.key(), ReplyTo: replyTo, Panel: result.panel, Form: result.form, Draft: result.draft, Status: result.status, Replace: result.replace, AtLatest: result.at_latest}
				response.Preferences = result.preferences
				response.Center = result.center
				response.Profile = result.profile
				response.Mention = result.mention
				response.ClearView = result.clearView
				response.DirectTarget = result.directTarget
				response.CommunityReceipt = result.communityReceipt
				response.TicReceipt = result.ticReceipt
				response.GuessReceipt = result.guessReceipt
				response.WordReceipt = result.wordReceipt
				response.ReportReceipt = result.reportReceipt
				response.SanctionReceipt = result.sanctionReceipt
				response.ManagementReceipt = result.managementReceipt
				response.ConfigurationReceipt = result.configurationReceipt
				if result.profile != nil {
					profileTarget = result.profile.Name
				}
				if result.center != nil {
					centerSubscribed = true
				}
				if result.closeSession {
					response.Status = "bye"
				}
				for _, message := range result.messages {
					response.Messages = append(response.Messages, guiMessageForViewer(message, user.Name))
				}
				if result.message != nil {
					response.Message = ptrGUIMessage(guiMessageForViewer(*result.message, user.Name))
				}
				if err := guiCompleteActionResult(writer, hub, response, result); err != nil {
					return err
				}
				if result.closeSession {
					return nil
				}
				if moved && !result.replace {
					if err := sendState("", false); err != nil {
						return err
					}
				}
			case "sync":
				if !limiter.Allow() {
					sendError(request.RequestID, errors.New("操作过于频繁，请稍候"))
					continue
				}
				if !joined {
					sendError(request.RequestID, errors.New("authentication required"))
					continue
				}
				if refreshed, ok := hub.RefreshClient(client); !ok {
					sendError(request.RequestID, errors.New("会话已失效，请重新登录"))
					return nil
				} else {
					user = refreshed
					maintenance.revalidate(user)
				}
				if err := sendState(request.RequestID, false); err != nil {
					sendError(request.RequestID, err)
				}
			case "disconnect":
				_ = writer.send(guiResponse{Type: "status", RequestID: request.RequestID, Status: "bye"})
				return nil
			default:
				sendError(request.RequestID, errors.New("unknown request type"))
			}
		case event := <-events:
			refreshed, valid := hub.RefreshClient(client)
			if !valid {
				return errors.New("session is no longer authorized")
			}
			user = refreshed
			maintenance.revalidate(user)
			if user.MustReset {
				continue
			} // No content/presence/notice leaks into reset-only mode.
			if event.Message != nil {
				fresh, err := store.Message(user.Name, event.Message.ID)
				message := guiMessageForViewer(fresh, user.Name)
				if err == nil && (message.Room == scope.key() || message.Recipient != "") {
					if err := writer.send(guiResponse{Type: "message", Message: &message}); err != nil {
						return err
					}
				}
			} else if event.ChangedID != 0 {
				fresh, err := store.Message(user.Name, event.ChangedID)
				message := guiMessageForViewer(fresh, user.Name)
				if err == nil && (message.Room == scope.key() || message.Recipient != "") {
					if err := writer.send(guiResponse{Type: "message_updated", Message: &message}); err != nil {
						return err
					}
				}
			} else if event.MoveRoom != "" {
				scope = guiScope{room: event.MoveRoom}
				lastCommunityRoom = scope.room
				replyTo = 0
				if err := sendState("", false); err != nil {
					scope = guiScope{room: "lobby"}
					lastCommunityRoom = scope.room
					replyTo = 0
					hub.Move(client, scope.room, scope.peer)
					if fallbackErr := sendState("", false); fallbackErr != nil {
						return fallbackErr
					}
				}
			} else if event.Preferences {
				if err := sendPreferences("", false); err != nil {
					return err
				}
			} else if event.Presence || event.Refresh {
				_ = sendState("", true)
			} else if event.Notice != "" && store.Settings(user.Name)["dnd"] != "on" && !store.Blocked(user.Name, noticeActor(event.Notice)) {
				_ = writer.send(guiResponse{Type: "status", Status: event.Notice})
			}
			if event.Refresh || event.Presence {
				if err := sendProfile("", false); err != nil {
					return err
				}
			}
			if guiCenterEventRelevant(event, user.Name) {
				if err := sendCenter("", false); err != nil {
					return err
				}
			}
		case <-health.C:
			maintenance.expire(time.Now())
			if joined {
				store.sweepExpiredAttachments(ctx)
				var ok bool
				user, ok = maintenance.refresh(hub, client)
				if !ok {
					_ = writer.send(guiResponse{Type: "error", Error: "会话已失效，请重新登录"})
					return nil
				}
				store.TouchSession(client.ID)
				if !user.MustReset {
					if err := sendPreferences("", false); err != nil {
						return err
					}
					if err := sendProfile("", false); err != nil {
						return err
					}
					if err := sendCenter("", false); err != nil {
						return err
					}
				}
			}
		}
	}
}

func ptrGUIMessage(message guiMessage) *guiMessage { return &message }
