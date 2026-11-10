package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWeightHubRootOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FENGSHEN_MODEL_WEIGHT_HUB", dir)
	if got := weightHubRoot(); got != filepath.Clean(dir) {
		t.Fatalf("hub override=%q want %q", got, dir)
	}
	t.Setenv("FENGSHEN_MODEL_WEIGHT_HUB", filepath.Join(dir, "missing"))
	if got := weightHubRoot(); got != "" {
		t.Fatalf("missing override accepted: %q", got)
	}
	t.Setenv("FENGSHEN_MODEL_WEIGHT_HUB", "relative")
	if got := weightHubRoot(); got != "" {
		t.Fatalf("relative override accepted: %q", got)
	}
	_ = os.Remove(dir)
}
