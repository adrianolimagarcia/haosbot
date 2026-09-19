package cron

import "context"

const (
	KindAt    = "at"
	KindEvery = "every"
	KindCron  = "cron"

	PayloadAgentTurn   = "agent_turn"
	PayloadSystemEvent = "system_event"

	StatusRunning = "running"
	StatusOK      = "ok"
	StatusError   = "error"
	StatusSkipped = "skipped"

	MisfireFireOnce = "fire_once"
	MisfireSkip     = "skip"
)

type Schedule struct {
	Kind    string `json:"kind"`
	AtMS    *int64 `json:"atMs,omitempty"`
	EveryMS *int64 `json:"everyMs,omitempty"`
	Expr    string `json:"expr,omitempty"`
	TZ      string `json:"tz,omitempty"`
}

type Payload struct {
	Kind           string         `json:"kind"`
	Message        string         `json:"message"`
	SystemEvent    string         `json:"systemEvent,omitempty"`
	SessionKey     string         `json:"sessionKey,omitempty"`
	OriginChannel  string         `json:"originChannel,omitempty"`
	OriginChatID   string         `json:"originChatId,omitempty"`
	OriginMetadata map[string]any `json:"originMetadata,omitempty"`
}

type RunRecord struct {
	RunAtMS         int64  `json:"runAtMs"`
	ScheduledForMS  int64  `json:"scheduledForMs,omitempty"`
	Status          string `json:"status"`
	DurationMS      int64  `json:"durationMs"`
	Error           string `json:"error,omitempty"`
	RunID           string `json:"runId,omitempty"`
	IdempotencyKey  string `json:"idempotencyKey,omitempty"`
}

type State struct {
	NextRunAtMS        *int64      `json:"nextRunAtMs,omitempty"`
	LastRunAtMS        *int64      `json:"lastRunAtMs,omitempty"`
	LastScheduledForMS *int64      `json:"lastScheduledForMs,omitempty"`
	LastStatus         string      `json:"lastStatus,omitempty"`
	LastError          string      `json:"lastError,omitempty"`
	LastIdempotencyKey string      `json:"lastIdempotencyKey,omitempty"`
	RunHistory         []RunRecord `json:"runHistory,omitempty"`
	Pending            bool        `json:"-"`
}

type Job struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Enabled        bool     `json:"enabled"`
	Schedule       Schedule `json:"schedule"`
	Payload        Payload  `json:"payload"`
	State          State    `json:"state"`
	CreatedAtMS    int64    `json:"createdAtMs"`
	UpdatedAtMS    int64    `json:"updatedAtMs"`
	DeleteAfterRun bool     `json:"deleteAfterRun,omitempty"`
	TimeoutMS      int64    `json:"timeoutMs,omitempty"`
	MisfirePolicy  string   `json:"misfirePolicy,omitempty"`
	MisfireGraceMS int64    `json:"misfireGraceMs,omitempty"`

	// Execution-only fields are never persisted. They bind one dispatch to the
	// schedule instant that produced it, which makes the idempotency key stable
	// across retries/restarts.
	ScheduledForMS int64  `json:"-"`
	IdempotencyKey string `json:"-"`
}

type Store struct {
	Version int   `json:"version"`
	Jobs    []Job `json:"jobs"`
}

type RunResult struct {
	RunID    string
	Response string
}

type Update struct {
	Name           *string
	Enabled        *bool
	Schedule       *Schedule
	Message        *string
	DeleteAfterRun *bool
	TimeoutMS      *int64
	MisfirePolicy  *string
	MisfireGraceMS *int64
}

type Executor func(ctx context.Context, job Job, runID string) (RunResult, error)

func cloneJob(in Job) Job {
	out := in
	if in.Schedule.AtMS != nil {
		v := *in.Schedule.AtMS
		out.Schedule.AtMS = &v
	}
	if in.Schedule.EveryMS != nil {
		v := *in.Schedule.EveryMS
		out.Schedule.EveryMS = &v
	}
	out.Payload.OriginMetadata = cloneMap(in.Payload.OriginMetadata)
	out.State.RunHistory = append([]RunRecord(nil), in.State.RunHistory...)
	if in.State.NextRunAtMS != nil {
		v := *in.State.NextRunAtMS
		out.State.NextRunAtMS = &v
	}
	if in.State.LastRunAtMS != nil {
		v := *in.State.LastRunAtMS
		out.State.LastRunAtMS = &v
	}
	if in.State.LastScheduledForMS != nil {
		v := *in.State.LastScheduledForMS
		out.State.LastScheduledForMS = &v
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
