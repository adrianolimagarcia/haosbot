package registry

import "testing"

func TestBuiltInManifestsHaveRuntimeAndSetup(t *testing.T) {
	manifests := All()
	if len(manifests) != 18 {
		t.Fatalf("registered transports = %d, want 18", len(manifests))
	}
	seen := make(map[string]bool, len(manifests))
	for _, manifest := range manifests {
		if manifest.ID == "" || manifest.Name == "" || manifest.Build == nil || manifest.Setup == nil {
			t.Errorf("incomplete channel manifest: %#v", manifest)
		}
		if manifest.Probe == "" {
			t.Errorf("channel %q has no probe level", manifest.ID)
		}
		if !manifest.Capabilities.Text {
			t.Errorf("channel %q must declare text capability", manifest.ID)
		}
		if seen[manifest.ID] {
			t.Errorf("duplicate channel manifest %q", manifest.ID)
		}
		seen[manifest.ID] = true
	}
}

func TestLookupReturnsRegisteredTransport(t *testing.T) {
	manifest, ok := Lookup("matrix")
	if !ok || manifest.Name != "Matrix" || manifest.Build == nil {
		t.Fatalf("Lookup(matrix) = %#v, %v", manifest, ok)
	}
	if _, ok := Lookup("not-a-transport"); ok {
		t.Fatal("Lookup accepted an unknown transport")
	}
}


func TestCapabilityContractIsConservative(t *testing.T) {
	websocket, ok := Lookup("websocket")
	if !ok || !websocket.Capabilities.Media || !websocket.Capabilities.Streaming || !websocket.Capabilities.Threads {
		t.Fatalf("websocket capabilities = %#v", websocket.Capabilities)
	}
	telegram, ok := Lookup("telegram")
	if !ok || telegram.Probe != ProbeLive || !telegram.Capabilities.Threads || !telegram.Capabilities.Groups {
		t.Fatalf("telegram manifest = %#v", telegram)
	}
	signal, ok := Lookup("signal")
	if !ok || signal.Probe != ProbeDependency {
		t.Fatalf("signal probe = %q", signal.Probe)
	}
	for _, manifest := range All() {
		if manifest.Capabilities.Reactions || manifest.Capabilities.Typing {
			t.Errorf("%s advertises an unimplemented capability: %#v", manifest.ID, manifest.Capabilities)
		}
	}
}

func TestValidateRejectsUnknownTransport(t *testing.T) {
	result := Validate("not-a-transport", nil, ValidationContext{})
	if result.Status != "unsupported" || result.CanEnable {
		t.Fatalf("Validate(unknown) = %#v", result)
	}
}
