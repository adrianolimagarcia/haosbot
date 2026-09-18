package api

import (
	"path/filepath"
	"testing"

	"github.com/adrianolimagarcia/nanobot-go/internal/config"
)

func TestConfigTargetPathPrefersLoadedSource(t *testing.T) {
	cfg := config.DefaultConfig()
	legacy := filepath.Join(t.TempDir(), ".nanobot", "config.json")
	cfg.BindSourcePath(legacy)

	if got := configTargetPath(cfg); got != legacy {
		t.Fatalf("configTargetPath=%q want loaded source %q", got, legacy)
	}
}

func TestConfigTargetPathFallsBackToCanonicalDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.DefaultConfig()

	want := filepath.Join(home, ".haosbot", "config.json")
	if got := configTargetPath(cfg); got != want {
		t.Fatalf("configTargetPath=%q want %q", got, want)
	}
}

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

func TestRedactedConfigRemovesDynamicHeaderSecrets(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Providers.OpenAI.ExtraHeaders = map[string]string{
		"X-API-Key":           "header-secret",
		"X-Auth-Token":        "token-secret",
		"Proxy-Authorization": "proxy-secret",
		"X-Request-ID":        "safe-value",
	}

	view, err := redactedConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	providers := view["providers"].(map[string]any)
	openai := providers["openai"].(map[string]any)
	headers := openai["extraHeaders"].(map[string]any)

	for _, key := range []string{"X-API-Key", "X-Auth-Token", "Proxy-Authorization"} {
		if got := headers[key]; got != "" {
			t.Errorf("%s leaked: %#v", key, got)
		}
		if got := headers[key+"Configured"]; got != true {
			t.Errorf("%sConfigured=%#v want true", key, got)
		}
	}
	if got := headers["X-Request-ID"]; got != "safe-value" {
		t.Errorf("non-secret header was redacted: %#v", got)
	}

	// Numeric token-budget fields are plural and must remain visible; they are
	// configuration, not credentials.
	agents := view["agents"].(map[string]any)
	defaults := agents["defaults"].(map[string]any)
	if _, ok := defaults["maxTokensConfigured"]; ok {
		t.Fatal("maxTokens was incorrectly classified as a secret")
	}
	if _, ok := defaults["contextWindowTokensConfigured"]; ok {
		t.Fatal("contextWindowTokens was incorrectly classified as a secret")
	}
}
