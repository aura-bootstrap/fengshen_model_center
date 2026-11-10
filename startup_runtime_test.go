package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartupEngineTrustBoundary(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "model-center"), 0700); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	for _, digests := range [][2]string{{"", ""}, {digest, digest}} {
		engines, err := loadStartupEngineTrust(context.Background(), root, digests[0], digests[1])
		if err != nil || len(engines) != 0 {
			t.Fatalf("legacy read-only startup: %v %v", engines, err)
		}
	}
	path := filepath.Join(root, "model-center", "engine-trust.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, digests := range [][2]string{{"", ""}, {digest, ""}, {digest, digest}} {
		if _, err := loadStartupEngineTrust(context.Background(), root, digests[0], digests[1]); err == nil {
			t.Fatal("unconfirmed or corrupt trust accepted")
		}
	}
	if err := os.WriteFile(filepath.Join(root, "engine.exe"), []byte("fixture: metadata only"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"schema": 1, "center_sha256": digest, "delivery_sha256": digest, "engines": []map[string]any{{"path": "engine.exe", "sha256": digest, "edition": "user", "management": false}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	engines, err := loadStartupEngineTrust(context.Background(), root, digest, digest)
	if err != nil || len(engines) != 1 || engines[0].Management || engines[0].Path != filepath.Join(root, "engine.exe") {
		t.Fatalf("configured trust: %+v %v", engines, err)
	}
}
