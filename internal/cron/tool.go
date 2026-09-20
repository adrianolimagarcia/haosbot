package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/tools"
)

type Tool struct {
	tools.Base
	service         *Service
	defaultTimezone string
}

func NewTool(service *Service, defaultTimezone string) *Tool {
	if strings.TrimSpace(defaultTimezone) == "" {
		defaultTimezone = "UTC"
	}
	return &Tool{service: service, defaultTimezone: defaultTimezone}
}

func (t *Tool) Name() string { return "cron" }

func (t *Tool) Description() string {
	return "Schedule reminders and recurring agent tasks. Actions: add, list, remove, pause, resume, run, cancel. " +
		"Jobs are bound to the current chat session and execute in the background."
}

func (t *Tool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"action":{"type":"string","enum":["add","list","remove","pause","resume","run","cancel"]},
			"name":{"type":"string"},
			"message":{"type":"string"},
			"every_seconds":{"type":"integer","minimum":1},
			"cron_expr":{"type":"string"},
			"tz":{"type":"string"},
			"at":{"type":"string"},
			"job_id":{"type":"string"},
			"timeout_seconds":{"type":"integer","minimum":0},
			"misfire_policy":{"type":"string","enum":["fire_once","skip"]},
			"misfire_grace_seconds":{"type":"integer","minimum":0}
		},
		"required":["action"],
		"additionalProperties":false
	}`)
}

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var args struct {
		Action       string `json:"action"`
		Name         string `json:"name"`
		Message      string `json:"message"`
		EverySeconds *int64 `json:"every_seconds"`
		CronExpr     string `json:"cron_expr"`
		TZ           string `json:"tz"`
		At           string `json:"at"`
		JobID        string `json:"job_id"`
		TimeoutSeconds *int64 `json:"timeout_seconds"`
		MisfirePolicy string `json:"misfire_policy"`
		MisfireGraceSeconds *int64 `json:"misfire_grace_seconds"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return tools.Errf("Error: invalid cron arguments: %v", err), nil
	}
	if t.service == nil {
		return tools.Errf("Error: automation scheduler is not configured"), nil
	}
	switch args.Action {
	case "list":
		jobs := t.service.ListJobs(true)
		if len(jobs) == 0 {
			return tools.OK("No scheduled jobs."), nil
		}
		var b strings.Builder
		b.WriteString("Scheduled jobs:")
		for _, job := range jobs {
			fmt.Fprintf(&b, "\n- %s (id: %s, %s", job.Name, job.ID, formatSchedule(job.Schedule))
			if !job.Enabled {
				b.WriteString(", paused")
			}
			b.WriteString(")")
			if job.State.NextRunAtMS != nil {
				fmt.Fprintf(&b, "\n  Next run: %s", time.UnixMilli(*job.State.NextRunAtMS).Format(time.RFC3339))
			}
			if job.State.LastStatus != "" {
				fmt.Fprintf(&b, "\n  Last status: %s", job.State.LastStatus)
			}
		}
		return tools.OK(b.String()), nil
	case "pause", "resume":
		id := strings.TrimSpace(args.JobID)
		if id == "" { return tools.Errf("Error: job_id is required"), nil }
		enabled := args.Action == "resume"
		if _, err := t.service.UpdateJob(id, Update{Enabled:&enabled}); err != nil { return tools.Errf("Error: %v", err), nil }
		return tools.OK(fmt.Sprintf("%s job %s", args.Action, id)), nil
	case "run":
		id := strings.TrimSpace(args.JobID)
		if id == "" { return tools.Errf("Error: job_id is required"), nil }
		if err := t.service.RunNow(id, true); err != nil { return tools.Errf("Error: %v", err), nil }
		return tools.OK("Queued job " + id), nil
	case "cancel":
		id := strings.TrimSpace(args.JobID)
		if id == "" { return tools.Errf("Error: job_id is required"), nil }
		if err := t.service.Cancel(id); err != nil { return tools.Errf("Error: %v", err), nil }
		return tools.OK("Cancelled job " + id), nil
	case "remove":
		if !t.service.Running() {
			return tools.Errf("Error: automation scheduler is not running; use the persistent HAOSBOT gateway"), nil
		}
		if strings.TrimSpace(args.JobID) == "" {
			return tools.Errf("Error: job_id is required for remove"), nil
		}
		if err := t.service.RemoveJob(strings.TrimSpace(args.JobID)); err != nil {
			return tools.Errf("Error: %v", err), nil
		}
		return tools.OK("Removed job " + strings.TrimSpace(args.JobID)), nil
	case "add":
		if !t.service.Running() {
			return tools.Errf("Error: automation scheduler is not running; use the persistent HAOSBOT gateway"), nil
		}
		if InExecution(ctx) {
			return tools.Errf("Error: cannot schedule new jobs from within a cron job execution"), nil
		}
		message := strings.TrimSpace(args.Message)
		if message == "" {
			return tools.Errf("Error: message is required when action='add'"), nil
		}
		route := tools.RequestRouteFromContext(ctx)
		sessionKey := strings.TrimSpace(route.SessionKey)
		channel := strings.TrimSpace(route.Channel)
		chatID := strings.TrimSpace(route.ChatID)
		if channel == "" || chatID == "" {
			channel, chatID = splitSessionKey(sessionKey)
		}
		if sessionKey == "" || channel == "" || chatID == "" {
			return tools.Errf("Error: scheduled jobs must be created from a chat session"), nil
		}
		schedule, deleteAfter, err := t.scheduleFromArgs(args.EverySeconds, args.CronExpr, args.TZ, args.At)
		if err != nil {
			return tools.Errf("Error: %v", err), nil
		}
		name := strings.TrimSpace(args.Name)
		if name == "" {
			name = message
			if len([]rune(name)) > 30 {
				name = string([]rune(name)[:30])
			}
		}
		job, err := t.service.AddJob(Job{
			Name: name,
			Schedule: schedule,
			DeleteAfterRun: deleteAfter,
			TimeoutMS: secondsToMS(args.TimeoutSeconds), MisfirePolicy: strings.TrimSpace(args.MisfirePolicy), MisfireGraceMS: secondsToMS(args.MisfireGraceSeconds),
			Payload: Payload{
				Kind: PayloadAgentTurn, Message: message,
				SessionKey: sessionKey, OriginChannel: channel, OriginChatID: chatID,
				OriginMetadata: persistableMetadata(route.Metadata),
			},
		})
		if err != nil {
			return tools.Errf("Error: %v", err), nil
		}
		return tools.OK(fmt.Sprintf("Created job '%s' (id: %s)", job.Name, job.ID)), nil
	default:
		return tools.Errf("Error: unknown cron action %q", args.Action), nil
	}
}

func (t *Tool) scheduleFromArgs(every *int64, expr, tz, at string) (Schedule, bool, error) {
	switch {
	case every != nil:
		if *every <= 0 {
			return Schedule{}, false, fmt.Errorf("every_seconds must be > 0")
		}
		ms := *every * 1000
		s := Schedule{Kind: KindEvery, EveryMS: &ms}
		return s, false, ValidateSchedule(s)
	case strings.TrimSpace(expr) != "":
		effectiveTZ := strings.TrimSpace(tz)
		if effectiveTZ == "" {
			effectiveTZ = t.defaultTimezone
		}
		s := Schedule{Kind: KindCron, Expr: strings.TrimSpace(expr), TZ: effectiveTZ}
		return s, false, ValidateSchedule(s)
	case strings.TrimSpace(at) != "":
		loc, err := time.LoadLocation(t.defaultTimezone)
		if err != nil {
			return Schedule{}, false, err
		}
		value := strings.TrimSpace(at)
		dt, err := time.Parse(time.RFC3339, value)
		if err != nil {
			dt, err = time.ParseInLocation("2006-01-02T15:04:05", value, loc)
		}
		if err != nil {
			return Schedule{}, false, fmt.Errorf("invalid ISO datetime %q", value)
		}
		ms := dt.UnixMilli()
		s := Schedule{Kind: KindAt, AtMS: &ms}
		return s, true, ValidateSchedule(s)
	default:
		return Schedule{}, false, fmt.Errorf("either every_seconds, cron_expr, or at is required")
	}
}

func persistableMetadata(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		raw, err := json.Marshal(value)
		if err != nil {
			continue
		}
		var detached any
		if err := json.Unmarshal(raw, &detached); err != nil {
			continue
		}
		out[key] = detached
	}
	return out
}

func splitSessionKey(key string) (string, string) {
	channel, chatID, ok := strings.Cut(key, ":")
	if !ok {
		return "", ""
	}
	return strings.TrimSpace(channel), strings.TrimSpace(chatID)
}

func formatSchedule(s Schedule) string {
	switch s.Kind {
	case KindEvery:
		if s.EveryMS != nil {
			return fmt.Sprintf("every %s", time.Duration(*s.EveryMS)*time.Millisecond)
		}
	case KindCron:
		if s.TZ != "" {
			return fmt.Sprintf("cron %q (%s)", s.Expr, s.TZ)
		}
		return fmt.Sprintf("cron %q", s.Expr)
	case KindAt:
		if s.AtMS != nil {
			return "at " + time.UnixMilli(*s.AtMS).Format(time.RFC3339)
		}
	}
	return s.Kind
}

func secondsToMS(v *int64) int64 { if v == nil { return 0 }; return *v * 1000 }
