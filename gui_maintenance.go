package main

import (
	"context"
	"errors"
	"strconv"
	"time"
)

const guiMaintenanceRetention = 5 * time.Minute

type guiMaintenanceRequest struct {
	Operation string `json:"operation"`
	Kind      string `json:"kind,omitempty"`
	JobID     string `json:"job_id,omitempty"`
	StartID   string `json:"start_id,omitempty"`
}
type guiMaintenanceReceipt struct {
	Operation  string   `json:"operation"`
	JobID      string   `json:"job_id"`
	StartID    string   `json:"start_id"`
	Kind       string   `json:"kind"`
	OriginRoom string   `json:"origin_room"`
	State      string   `json:"state"`
	Committed  *bool    `json:"committed,omitempty"`
	HasWarning bool     `json:"has_warning"`
	Lines      []string `json:"lines,omitempty"`
	Warning    string   `json:"warning,omitempty"`
}
type guiMaintenanceWork struct {
	id, startID, owner, sessionID, room, kind string
	epoch                                     int64
	cancel                                    context.CancelFunc
	done                                      chan struct{}
}
type guiMaintenanceResult struct {
	job     *guiMaintenanceWork
	outcome maintenanceOutcome
	err     error
}
type guiMaintenanceCompleted struct {
	job     *guiMaintenanceWork
	outcome maintenanceOutcome
	err     error
	state   string
	expires time.Time
}

// Only the owning protocol loop mutates this object. The worker sends one
// bounded result and closes done; no goroutine mutates user/scope or the writer.
type guiMaintenanceState struct {
	sequence  uint64
	active    *guiMaintenanceWork
	completed *guiMaintenanceCompleted
	results   chan guiMaintenanceResult
}

func newGUIMaintenance() *guiMaintenanceState {
	return &guiMaintenanceState{results: make(chan guiMaintenanceResult, 1)}
}
func (m *guiMaintenanceState) stop() {
	if m.active != nil {
		m.active.cancel()
		<-m.active.done
		m.active = nil
	}
	m.completed = nil
}

// Policy refresh cancels outstanding work and forgets private completed data;
// final result delivery still independently reauthorizes from the database.
func (m *guiMaintenanceState) revalidate(user User) {
	allowed := isAdmin(user.Role) && !user.MustReset
	if m.active != nil && (!allowed || m.active.owner != user.Name || m.active.epoch != user.AuthEpoch) {
		m.active.cancel()
	}
	if m.completed != nil && (!allowed || m.completed.job.owner != user.Name || m.completed.job.epoch != user.AuthEpoch) {
		m.completed = nil
	}
}

// The idle health branch uses this same fresh-policy path. Revoked sessions
// clear private descriptors and request cancellation before protocol teardown.
func (m *guiMaintenanceState) refresh(hub *Hub, client *Client) (User, bool) {
	user, ok := hub.RefreshClient(client)
	if !ok {
		m.revalidate(User{})
	} else {
		m.revalidate(user)
	}
	return user, ok
}
func (m *guiMaintenanceState) expire(now time.Time) {
	if m.completed != nil && !now.Before(m.completed.expires) {
		m.completed = nil
	}
}
func guiMaintenanceOnly(request guiRequest, scope guiScope) bool {
	return request.Room == scope.key() && request.Composition == nil && request.Body == "" && request.ReplyTo == 0 && request.Position == nil && request.Version == 0 && request.Username == "" && request.Password == "" && request.NewPassword == "" && request.Confirm == "" && request.Invite == "" && request.Before == 0 && request.BackupID == "" && request.SelfIPForce == nil && request.ReadAfter == 0 && request.ReadUntil == nil && request.WordMode == "" && request.GameID == 0 && request.TicVersion == nil && request.ExpectedRoom == ""
}
func maintenanceSummary(job *guiMaintenanceWork, operation, state string, committed, warning bool) *guiMaintenanceReceipt {
	out := &guiMaintenanceReceipt{Operation: operation, JobID: job.id, StartID: job.startID, Kind: job.kind, OriginRoom: job.room, State: state, HasWarning: warning}
	if state == "complete" || state == "cancelled" || state == "failed" {
		out.Committed = &committed
	}
	return out
}

func maintenanceFrame(requestID string, receipt *guiMaintenanceReceipt) guiResponse {
	return guiResponse{Type: "maintenance", RequestID: requestID, Room: receipt.OriginRoom, Maintenance: receipt}
}
func (m *guiMaintenanceState) request(ctx context.Context, store *Store, user User, scope guiScope, sessionID string, request guiRequest) (guiResponse, error) {
	var empty guiResponse
	in := request.Maintenance
	if in == nil || request.SecretAdmin != nil || ctx == nil || sessionID == "" || !guiMaintenanceOnly(request, scope) {
		return empty, errors.New("无效的后台维护请求")
	}
	m.expire(time.Now())
	switch in.Operation {
	case "start":
		if in.JobID != "" || in.StartID != "" || (in.Kind != "backup" && in.Kind != "check") {
			return empty, errors.New("无效作业类型或标识")
		}
		if m.active != nil {
			return empty, errors.New("当前连接已有后台维护作业")
		}
		// Coarse UI admission is not authoritative. Fresh session/role/epoch
		// checks happen in the worker after any DB queue, before SQL/IO work.
		if !isAdmin(user.Role) || user.MustReset {
			return empty, errors.New("需要已登录管理员权限")
		}
		if m.sequence == ^uint64(0) {
			return empty, errors.New("作业标识已用尽，请重新连接")
		}
		m.sequence++
		jobCtx, cancel := context.WithCancel(ctx)
		job := &guiMaintenanceWork{id: strconv.FormatUint(m.sequence, 10), startID: request.RequestID, owner: user.Name, sessionID: sessionID, room: scope.key(), kind: in.Kind, epoch: user.AuthEpoch, cancel: cancel, done: make(chan struct{})}
		m.completed = nil
		m.active = job
		go func() {
			defer close(job.done)
			out, err := store.RunMaintenanceForSession(jobCtx, job.owner, job.sessionID, job.kind)
			select {
			case m.results <- guiMaintenanceResult{job: job, outcome: out, err: err}:
			case <-ctx.Done():
			}
		}()
		return maintenanceFrame(request.RequestID, maintenanceSummary(job, "start", "running", false, false)), nil
	case "cancel":
		if in.Kind != "" || in.StartID == "" {
			return empty, errors.New("必须明确当前作业")
		}
		if m.active != nil && m.active.startID == in.StartID && (in.JobID == "" || m.active.id == in.JobID) && m.active.owner == user.Name && m.active.sessionID == sessionID {
			m.active.cancel()
			return maintenanceFrame(request.RequestID, maintenanceSummary(m.active, "cancel", "cancel_requested", false, false)), nil
		}
		if c := m.completed; c != nil && c.job.startID == in.StartID && c.job.id == in.JobID && c.job.owner == user.Name && c.job.sessionID == sessionID {
			return maintenanceFrame(request.RequestID, maintenanceSummary(c.job, "cancel", c.state, c.outcome.Committed, c.outcome.Warning != "")), nil
		}
		return empty, errors.New("该作业不再属于当前连接，未取消其它作业")
	case "view":
		if in.Kind != "" || in.JobID == "" || in.StartID == "" {
			return empty, errors.New("缺少原作业标识")
		}
		c := m.completed
		if c == nil || c.job.id != in.JobID || c.job.startID != in.StartID || c.job.owner != user.Name || c.job.sessionID != sessionID {
			return empty, errors.New("作业结果已过期或不属于当前连接")
		}
		current, err := store.maintenanceActor(ctx, user.Name, sessionID)
		if err != nil || current.AuthEpoch != c.job.epoch {
			m.completed = nil
			return empty, errors.New("当前权限无法查看维护结果")
		}
		out := maintenanceSummary(c.job, "view", c.state, c.outcome.Committed, c.outcome.Warning != "")
		out.Lines = append([]string{}, c.outcome.Lines...)
		if c.err != nil {
			out.Lines = append(out.Lines, cleanText(c.err.Error(), 300))
		}
		out.Warning = cleanText(c.outcome.Warning, 300)
		return maintenanceFrame(request.RequestID, out), nil
	default:
		return empty, errors.New("无效维护操作")
	}
}
func (m *guiMaintenanceState) finish(ctx context.Context, store *Store, result guiMaintenanceResult) (guiResponse, bool) {
	job := result.job
	if job == nil || m.active != job {
		return guiResponse{}, false
	}
	m.active.cancel()
	m.active = nil
	state := "complete"
	if result.err != nil {
		if errors.Is(result.err, context.Canceled) {
			state = "cancelled"
		} else {
			state = "failed"
		}
	}
	current, err := store.maintenanceActor(ctx, job.owner, job.sessionID)
	if err != nil || current.AuthEpoch != job.epoch {
		m.completed = nil
		return maintenanceFrame(job.startID, maintenanceSummary(job, "complete", "unavailable", false, false)), true
	}
	m.completed = &guiMaintenanceCompleted{job: job, outcome: result.outcome, err: result.err, state: state, expires: time.Now().Add(guiMaintenanceRetention)}
	// Completion is deliberately non-sensitive. Fetching paths/diagnostics
	// requires an explicit, currently visible client view and fresh role again.
	return maintenanceFrame(job.startID, maintenanceSummary(job, "complete", state, result.outcome.Committed, result.outcome.Warning != "")), true
}
