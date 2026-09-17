package api

import (
	"path/filepath"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

func TestRedactedConfigRemovesSecrets(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.API.APIKey = "gateway-secret"
	providerSecret := "provider-secret"
	cfg.Providers.OpenAI.APIKey = &providerSecret

	view, err := redactedConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}

	api := view["api"].(map[string]any)
	if got := api["apiKey"]; got != "" {
		t.Fatalf("api.apiKey leaked: %#v", got)
	}
	if got := api["apiKeyConfigured"]; got != true {
		t.Fatalf("api.apiKeyConfigured=%#v want true", got)
	}

	providers := view["providers"].(map[string]any)
	openai := providers["openai"].(map[string]any)
	if got := openai["apiKey"]; got != "" {
		t.Fatalf("providers.openai.apiKey leaked: %#v", got)
	}
	if got := openai["apiKeyConfigured"]; got != true {
		t.Fatalf("providers.openai.apiKeyConfigured=%#v want true", got)
	}
}

func TestSaveConfigPatchPreservesUnrelatedFieldsAndBlankSecrets(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.API.APIKey = "gateway-secret"
	providerSecret := "provider-secret"
	cfg.Providers.OpenAI.APIKey = &providerSecret
	cfg.Tools.Exec.Timeout = 321
	cfg.Agents.Defaults.Model = "old-model"

	target := filepath.Join(t.TempDir(), "config.json")
	patch := map[string]any{
		"agents": map[string]any{
			"defaults": map[string]any{
				"model": "new-model",
			},
		},
		"providers": map[string]any{
			"openai": map[string]any{
				"apiKey": "",
			},
		},
	}

	if err := saveConfigPatch(cfg, target, patch); err != nil {
		t.Fatalf("saveConfigPatch: %v", err)
	}
	got, err := config.Load(target)
	if err != nil {
		t.Fatalf("reload patched config: %v", err)
	}
	if got.Agents.Defaults.Model != "new-model" {
		t.Fatalf("model=%q want new-model", got.Agents.Defaults.Model)
	}
	if got.API.APIKey != "gateway-secret" {
		t.Fatalf("gateway API key was lost")
	}
	if got.Providers.OpenAI.APIKey == nil || *got.Providers.OpenAI.APIKey != "provider-secret" {
		t.Fatalf("provider API key was lost")
	}
	if got.Tools.Exec.Timeout != 321 {
		t.Fatalf("unrelated tools.exec.timeout=%d want 321", got.Tools.Exec.Timeout)
	}
}
