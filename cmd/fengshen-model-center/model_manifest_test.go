package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aura-bootstrap/fengshen_model_center/internal/modelmanifest"
)

func TestWriteModelManifestFromRoot(t *testing.T) {
	stateDir := t.TempDir()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ggml-small.bin"), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	target := modelmanifest.DefaultPath(stateDir)
	mirror := filepath.Join(t.TempDir(), "model_manifest.json")
	if err := writeModelManifest([]string{root}, []string{target, mirror}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{target, mirror} {
		m, _, err := modelmanifest.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Entries) != 1 || m.Entries[0].ModelID != "whisper-asr" || m.Entries[0].Bytes != 3 {
			t.Fatalf("manifest=%+v", m)
		}
	}
}
