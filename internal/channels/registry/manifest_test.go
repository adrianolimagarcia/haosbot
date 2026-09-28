package registry

import "testing"

func TestBuiltInManifestsHaveRuntimeAndSetup(t *testing.T) {
	manifests := All()
	if len(manifests) != 13 {
		t.Fatalf("registered transports = %d, want 13", len(manifests))
	}
	seen := make(map[string]bool, len(manifests))
	for _, manifest := range manifests {
		if manifest.ID == "" || manifest.Name == "" || manifest.Build == nil || manifest.Setup == nil {
			t.Errorf("incomplete channel manifest: %#v", manifest)
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
