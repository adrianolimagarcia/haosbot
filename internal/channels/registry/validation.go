package registry

import (
	"context"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
	"github.com/adrianolimagarcia/nanobot-go/internal/channels/telegram"
	"github.com/adrianolimagarcia/nanobot-go/internal/core"
)

// ProbeLevel describes how far the setup checker can verify a channel.
type ProbeLevel string

const (
	ProbeConfig     ProbeLevel = "config"
	ProbeDependency ProbeLevel = "dependency"
	ProbeLive       ProbeLevel = "live"
)

// Capabilities is the transport feature contract exposed to the WebUI.
// A false value means the runtime must not advertise or assume that feature.
type Capabilities struct {
	Text      bool `json:"text"`
	Media     bool `json:"media"`
	Threads   bool `json:"threads"`
	Reactions bool `json:"reactions"`
	Streaming bool `json:"streaming"`
	Typing    bool `json:"typing"`
	Groups    bool `json:"groups"`
}

// ValidationContext carries safety policy for probes.
type ValidationContext struct {
	AllowLocalServiceAccess bool
}

// ValidationCheck is one setup-check result.
type ValidationCheck struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// ValidationResult is the stable response returned by every channel checker.
type ValidationResult struct {
	Name            string            `json:"name"`
	Status          string            `json:"status"`
	Probe           ProbeLevel        `json:"probe"`
	Checks          []ValidationCheck `json:"checks"`
	Identity        map[string]any    `json:"identity"`
	MissingFields   []string          `json:"missing_fields"`
	CanEnable       bool              `json:"can_enable"`
	RequiresRestart bool              `json:"requires_restart"`
	CheckedAt       string            `json:"checked_at"`
	Message         string            `json:"message"`
}

type validationPublisher struct{}

func (validationPublisher) PublishInbound(context.Context, core.InboundMessage) error { return nil }

// Validate checks a registered transport using its declared probe level.
// Telegram currently has a real remote identity probe. Every other transport
// still receives strict runtime-constructor validation, so the UI never labels
// a config-only result as "connected".
func Validate(name string, values map[string]any, ctx ValidationContext) ValidationResult {
	manifest, ok := Lookup(name)
	if !ok {
		return ValidationResult{
			Name: name, Status: "unsupported", Probe: ProbeConfig,
			Checks: []ValidationCheck{{ID: "registry", Label: "Transport registry", Status: "fail", Message: "Transport is not registered."}},
			Identity: map[string]any{}, MissingFields: []string{}, CanEnable: false,
			CheckedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Message: "This channel is not supported by the WebUI setup checker.",
		}
	}

	if name == telegram.ChannelName {
		payload := telegram.ValidateChannel(values, telegram.ValidationContext{AllowLocalServiceAccess: ctx.AllowLocalServiceAccess})
		checks := make([]ValidationCheck, 0, len(payload.Checks))
		for _, check := range payload.Checks {
			checks = append(checks, ValidationCheck{ID: check.ID, Label: check.Label, Status: check.Status, Message: check.Message})
		}
		return ValidationResult{
			Name: payload.Name, Status: payload.Status, Probe: ProbeLive,
			Checks: checks, Identity: payload.Identity, MissingFields: payload.MissingFields,
			CanEnable: payload.CanEnable, RequiresRestart: payload.RequiresRestart,
			CheckedAt: payload.CheckedAt, Message: payload.Message,
		}
	}

	normalized := make(map[string]any, len(values)+1)
	for key, value := range values {
		normalized[key] = value
	}
	// Constructors commonly enforce required credentials only for enabled
	// channels. Force validation mode on without mutating persisted config.
	normalized["enabled"] = true

	_, err := manifest.Build(channels.NewMapSection(normalized), validationPublisher{})
	if err != nil {
		return ValidationResult{
			Name: name, Status: "invalid", Probe: manifest.Probe,
			Checks: []ValidationCheck{{ID: "runtime_config", Label: "Runtime configuration", Status: "fail", Message: err.Error()}},
			Identity: map[string]any{}, MissingFields: []string{}, CanEnable: false,
			CheckedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Message: "Configuration was checked and looks invalid.",
		}
	}

	probeMessage := "Runtime accepted the configuration. A remote identity probe is not implemented for this transport."
	if manifest.Probe == ProbeDependency {
		probeMessage = "Runtime accepted the configuration. The external companion service must be checked at deployment time."
	}
	return ValidationResult{
		Name: name, Status: "configured", Probe: manifest.Probe,
		Checks: []ValidationCheck{
			{ID: "runtime_config", Label: "Runtime configuration", Status: "pass", Message: "Configuration can construct the channel runtime."},
			{ID: "remote_probe", Label: "Remote connection", Status: "warn", Message: probeMessage},
		},
		Identity: map[string]any{}, MissingFields: []string{}, CanEnable: true, RequiresRestart: true,
		CheckedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Message: "Configuration is valid; full remote verification is not available for this transport.",
	}
}
