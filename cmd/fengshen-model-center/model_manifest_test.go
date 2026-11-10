package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aura-bootstrap/fengshen_model_center/internal/modelmanifest"
)

func TestWriteModelManifestFromRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ggml-small.bin"), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeModelManifest([]string{root}); err != nil {
		t.Fatal(err)
	}
	m, _, err := modelmanifest.Load(modelmanifest.DefaultPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 || m.Entries[0].ModelID != "whisper-asr" || m.Entries[0].Bytes != 3 {
		t.Fatalf("manifest=%+v", m)
	}
}
