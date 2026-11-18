package registry

import (
	"slices"
	"testing"
)

func TestRegistryDefaultLookupAndNames(t *testing.T) {
	t.Parallel()
	reg := Default()
	names := Names(reg)
	want := []string{"claude", "cursor", "gemini", "opencode"}
	if !slices.Equal(names, want) {
		t.Fatalf("unexpected registered targets: got %v, want %v", names, want)
	}
	for _, n := range want {
		adp, err := Lookup(reg, n)
		if err != nil {
			t.Fatalf("Lookup(%q) failed: %v", n, err)
		}
		if adp.Name() != n || adp.BinaryName() == "" {
			t.Fatalf("invalid adapter metadata for %q", n)
		}
		_ = adp.SchemaContract()
		_ = adp.RequiredProbes()
	}
	if _, err := Lookup(reg, "unknown"); err == nil {
		t.Fatalf("expected error for unknown target")
	}
}
