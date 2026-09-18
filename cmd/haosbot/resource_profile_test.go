package main

import "testing"

func TestResourceProfileDefaultsPreserveBalancedRuntime(t *testing.T) {
	t.Setenv(resourceProfileEnv, "")
	t.Setenv(obsidianProjectionEnv, "")
	p := resolveResourceProfile()
	if p.Name != "balanced" || !p.ObsidianEnabled || p.ProjectionWorkers != 1 {
		t.Fatalf("unexpected default profile: %+v", p)
	}
	if p.FutureRAMBytes != 10*1024*1024 || p.FutureDiskBytes != 200*1024*1024 {
		t.Fatalf("unexpected future budget: %+v", p)
	}
}

func TestResourceProfileLowDisablesOptionalObsidianProjection(t *testing.T) {
	t.Setenv(resourceProfileEnv, "low")
	t.Setenv(obsidianProjectionEnv, "")
	p := resolveResourceProfile()
	if p.Name != "low" || p.ObsidianEnabled || p.MemoryRetrievalEnabled || p.ProjectionPollMs != 5000 {
		t.Fatalf("unexpected low profile: %+v", p)
	}
	if p.MemoryMaxContentBytes != 32*1024 || p.MemoryMaxPendingBytes != 2*1024*1024 {
		t.Fatalf("unexpected low limits: %+v", p)
	}
}

func TestResourceProfileExplicitProjectionOverride(t *testing.T) {
	t.Setenv(resourceProfileEnv, "low")
	t.Setenv(obsidianProjectionEnv, "on")
	if p := resolveResourceProfile(); !p.ObsidianEnabled {
		t.Fatal("explicit Obsidian override was ignored")
	}
}
