package registry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/adrianolimagarcia/nanobot-go/internal/channels"
)

func TestLiveChannelSmokeE2E(t *testing.T) {
	raw := os.Getenv("HAOSBOT_CHANNEL_E2E_JSON")
	if raw == "" {
		t.Skip("HAOSBOT_CHANNEL_E2E_JSON is not configured")
	}

	var configs map[string]map[string]any
	if err := json.Unmarshal([]byte(raw), &configs); err != nil {
		t.Fatalf("decode HAOSBOT_CHANNEL_E2E_JSON: %v", err)
	}
	if len(configs) == 0 {
		t.Fatal("HAOSBOT_CHANNEL_E2E_JSON contains no channels")
	}

	for name, values := range configs {
		name, values := name, values
		t.Run(name, func(t *testing.T) {
			manifest, ok := Lookup(name)
			if !ok {
				t.Fatalf("transport %q is not registered", name)
			}
			if values == nil {
				values = map[string]any{}
			}
			values["enabled"] = true

			result := Validate(name, values, ValidationContext{AllowLocalServiceAccess: true})
			if result.Status == "invalid" || result.Status == "needs_setup" || result.Status == "unsupported" {
				t.Fatalf("validation failed: status=%s message=%s checks=%+v", result.Status, result.Message, result.Checks)
			}
			if manifest.Probe == ProbeLive && result.Status != "connected" {
				t.Fatalf("live probe did not connect: status=%s checks=%+v", result.Status, result.Checks)
			}

			ch, err := manifest.Build(channels.NewMapSection(values), validationPublisher{})
			if err != nil {
				t.Fatalf("build runtime: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			errCh := make(chan error, 1)
			go func() { errCh <- ch.Start(ctx) }()

			select {
			case err := <-errCh:
				cancel()
				if err == nil {
					t.Fatal("runtime exited during smoke window without an error")
				}
				t.Fatalf("runtime failed during smoke window: %v", err)
			case <-time.After(4 * time.Second):
				// Surviving the smoke window proves the runtime can initialize
				// with these credentials/dependencies without immediately dying.
			}

			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			if err := ch.Stop(stopCtx); err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("stop runtime: %v", err)
			}
			cancel()

			select {
			case <-errCh:
			case <-time.After(5 * time.Second):
				t.Error("runtime did not exit after Stop/cancel")
			}
		})
	}
}
