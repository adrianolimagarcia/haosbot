package config

import "testing"

func TestMultiAgentDefaultsAndProfileDecode(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Agents.MultiAgent.Enabled || cfg.Agents.MultiAgent.MaxDepth != 3 || cfg.Agents.MultiAgent.MaxParallel != 4 {
		t.Fatalf("multi-agent defaults = %+v", cfg.Agents.MultiAgent)
	}
	if len(cfg.Agents.Profiles) != 4 || !cfg.Agents.Profiles["coder"].Enabled {
		t.Fatalf("default profiles = %+v", cfg.Agents.Profiles)
	}

	cfg = mustLoad(t, `{"agents":{"multiAgent":{"enabled":true,"maxDepth":5,"maxParallel":2},"profiles":{"remote":{"enabled":true,"name":"Remote","role":"reviewer","endpoint":"https://example.com/a2a","tokenEnv":"REMOTE_TOKEN","toolAllow":[],"delegateTo":["reviewer"],"memoryScope":"private","maxParallel":1,"model":"deepseek/test"}}}}`)
	if cfg.Agents.MultiAgent.MaxDepth != 5 || cfg.Agents.MultiAgent.MaxParallel != 2 {
		t.Fatalf("decoded multi-agent limits = %+v", cfg.Agents.MultiAgent)
	}
	p, ok := cfg.Agents.Profiles["remote"]
	if !ok || p.Endpoint != "https://example.com/a2a" || p.TokenEnv != "REMOTE_TOKEN" || p.MemoryScope != "private" || p.MaxParallel != 1 || len(p.DelegateTo) != 1 || p.DelegateTo[0] != "reviewer" || p.Model != "deepseek/test" {
		t.Fatalf("decoded profile = %+v ok=%v", p, ok)
	}
}

func TestExplicitEmptyProfilesDisablesBuiltInRoster(t *testing.T) {
	cfg := mustLoad(t, `{"agents":{"profiles":{}}}`)
	if len(cfg.Agents.Profiles) != 0 {
		t.Fatalf("profiles = %+v, want explicit empty roster", cfg.Agents.Profiles)
	}
}
