package cron

import "context"

const (
	KindAt    = "at"
	KindEvery = "every"
	KindCron  = "cron"

	PayloadAgentTurn   = "agent_turn"
	PayloadSystemEvent = "system_event"

	StatusOK      = "ok"
	StatusError   = "error"
	StatusSkipped = "skipped"
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
	SessionKey     string         `json:"sessionKey,omitempty"`
	OriginChannel  string         `json:"originChannel,omitempty"`
	OriginChatID   string         `json:"originChatId,omitempty"`
	OriginMetadata map[string]any `json:"originMetadata,omitempty"`
}

type RunRecord struct {
	RunAtMS    int64  `json:"runAtMs"`
	Status     string `json:"status"`
	DurationMS int64  `json:"durationMs"`
	Error      string `json:"error,omitempty"`
	RunID      string `json:"runId,omitempty"`
}

type State struct {
	NextRunAtMS *int64      `json:"nextRunAtMs,omitempty"`
	LastRunAtMS *int64      `json:"lastRunAtMs,omitempty"`
	LastStatus  string      `json:"lastStatus,omitempty"`
	LastError   string      `json:"lastError,omitempty"`
	RunHistory  []RunRecord `json:"runHistory,omitempty"`
	Pending     bool        `json:"-"`
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
