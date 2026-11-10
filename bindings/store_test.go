package bindings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/integration"
)

func fixture(t *testing.T, scene string, models ...string) Binding {
	t.Helper()
	definition, ok := integration.RuntimeScene(scene)
	if !ok {
		t.Fatal(scene)
	}
	if len(models) == 0 {
		models = definition.RequiredModels
	}
	b := Binding{
		Scene: scene, InventoryID: "inventory-one", RegistryRevision: 3, Device: "cpu",
		Component: integration.ExecutionComponentRef{ID: definition.Components[0], Version: "v1", ManifestSHA256: strings.Repeat("a", 64), Protocol: "test/v1"},
	}
	root := t.TempDir()
	for i, model := range models {
		b.Files = append(b.Files, FrozenFile{
			ModelID: model, FileID: fmt.Sprintf("file-%d", i), Group: "selected-group",
			ScopePath: root, ScopeIdentity: "scope-identity", RelativePath: fmt.Sprintf("opaque-%d.data", i),
			Identity: fmt.Sprintf("identity-%d", i), Bytes: "17", Modified: time.Unix(1700000000, 123).UTC(),
		})
	}
	return b
}

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func put(t *testing.T, s *Store, revision uint64, b Binding) Snapshot {
	t.Helper()
	next, err := s.Put(context.Background(), revision, b)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func TestReopenMultipleScenesAndReferences(t *testing.T) {
	d := t.TempDir()
	s := openStore(t, d)
	if got := s.Snapshot(); got.SchemaVersion != 1 || got.Revision != 0 || len(got.Bindings) != 0 || got.Error != "" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(d, "bindings.json")); !os.IsNotExist(err) {
		t.Fatal("Open must not persist an empty document", err)
	}
	a := put(t, s, 0, fixture(t, "highlight.transcribe"))
	first := a.Bindings["highlight.transcribe"].Ref
	if first.Validate() != nil || first.Revision != 1 {
		t.Fatal(first)
	}
	b := put(t, s, 1, fixture(t, "image.face"))
	if b.Revision != 2 || b.Bindings[first.Scene].Ref != first {
		t.Fatal("unrelated scene changed", b)
	}
	raw, err := os.ReadFile(filepath.Join(d, "bindings.json"))
	if err != nil || strings.Contains(string(raw), "inference") || strings.Contains(string(raw), "passed") {
		t.Fatal("store must not manufacture inference evidence", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, d)
	if got := reopened.Snapshot(); !reflect.DeepEqual(got, b) {
		t.Fatalf("reopen mismatch: got %+v, want %+v", got, b)
	}
	if _, err := reopened.Delete(context.Background(), 1, first.Scene); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	after, err := reopened.Delete(context.Background(), 2, first.Scene)
	if err != nil || after.Revision != 3 || len(after.Bindings) != 1 {
		t.Fatal(after, err)
	}
	if _, err := reopened.Delete(context.Background(), 3, first.Scene); err == nil || reopened.Snapshot().Revision != 3 {
		t.Fatal("missing delete changed revision", err)
	}
	reopened.Close()
	if got := openStore(t, d).Snapshot(); !reflect.DeepEqual(got, after) {
		t.Fatal(got)
	}
}

func TestCloneIsolation(t *testing.T) {
	s := openStore(t, t.TempDir())
	input := fixture(t, "highlight.score")
	out := put(t, s, 0, input)
	original := s.Snapshot()
	input.Files[0].Identity = "caller-mutated"
	v := out.Bindings[input.Scene]
	v.Files[0].Bytes = "100"
	out.Bindings[input.Scene] = v
	delete(out.Bindings, input.Scene)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				copy := s.Snapshot()
				copy.Bindings[input.Scene].Files[0].Identity = "mutated-copy"
				delete(copy.Bindings, input.Scene)
			}
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(s.Snapshot(), original) {
		t.Fatal("caller owns store memory")
	}
}

func TestCompositionAndFrozenContentValidation(t *testing.T) {
	for _, scene := range integration.RuntimeScenes() {
		t.Run("valid/"+scene.ID, func(t *testing.T) {
			put(t, openStore(t, t.TempDir()), 0, fixture(t, scene.ID))
		})
	}
	for _, models := range [][]string{{"demucs-ft"}, {"whisper-asr", "whisper-vad"}} {
		scene := "timbre.separate"
		if models[0] == "whisper-asr" {
			scene = "highlight.transcribe"
		}
		put(t, openStore(t, t.TempDir()), 0, fixture(t, scene, models...))
	}
	cases := map[string]func(*Binding){
		"unknown scene":          func(b *Binding) { b.Scene = "unknown" },
		"missing inventory":      func(b *Binding) { b.InventoryID = "" },
		"wrong component":        func(b *Binding) { b.Component.ID = "seedvc-runner" },
		"bad manifest":           func(b *Binding) { b.Component.ManifestSHA256 = "bad" },
		"empty files":            func(b *Binding) { b.Files = nil },
		"unknown model":          func(b *Binding) { b.Files[0].ModelID = "unknown" },
		"wrong scene model":      func(b *Binding) { b.Files[0].ModelID = "local-vision" },
		"missing file ID":        func(b *Binding) { b.Files[0].FileID = "" },
		"missing group":          func(b *Binding) { b.Files[0].Group = "" },
		"missing identity":       func(b *Binding) { b.Files[0].Identity = "" },
		"missing scope identity": func(b *Binding) { b.Files[0].ScopeIdentity = "" },
		"missing modified":       func(b *Binding) { b.Files[0].Modified = time.Time{} },
		"negative bytes":         func(b *Binding) { b.Files[0].Bytes = "-1" },
		"zero bytes":             func(b *Binding) { b.Files[0].Bytes = "0" },
		"noncanonical bytes":     func(b *Binding) { b.Files[0].Bytes = "017" },
		"overflow bytes":         func(b *Binding) { b.Files[0].Bytes = "9223372036854775808" },
		"bad bytes type":         func(b *Binding) { b.Files[0].Bytes = "1.5" },
		"relative scope":         func(b *Binding) { b.Files[0].ScopePath = "relative" },
		"relative escape":        func(b *Binding) { b.Files[0].RelativePath = "../escape" },
		"windows escape":         func(b *Binding) { b.Files[0].RelativePath = `..\escape` },
		"ADS":                    func(b *Binding) { b.Files[0].RelativePath = "file:stream" },
		"reserved device":        func(b *Binding) { b.Files[0].RelativePath = "NUL.bin" },
		"empty relative":         func(b *Binding) { b.Files[0].RelativePath = "" },
		"device injection":       func(b *Binding) { b.Device = "cpu --command" },
		"partial registration":   func(b *Binding) { b.Files[0].RegistrationIdentity = "registered" },
		"duplicate file ID":      func(b *Binding) { f := b.Files[0]; f.RelativePath = "another"; b.Files = append(b.Files, f) },
		"mixed groups": func(b *Binding) {
			f := b.Files[0]
			f.FileID = "second"
			f.Identity = "second"
			f.RelativePath = "second"
			f.Group = "other"
			b.Files = append(b.Files, f)
		},
		"duplicate location": func(b *Binding) { f := b.Files[0]; f.FileID = "second"; b.Files = append(b.Files, f) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := openStore(t, t.TempDir())
			b := fixture(t, "highlight.score")
			mutate(&b)
			if _, err := s.Put(context.Background(), 0, b); err == nil || s.Snapshot().Revision != 0 {
				t.Fatal("invalid binding accepted", err)
			}
		})
	}
	for _, tc := range []struct {
		scene  string
		models []string
	}{
		{"timbre.separate", []string{"demucs", "demucs-ft"}},
		{"highlight.transcribe", []string{"whisper-vad"}},
		{"timbre.convert", []string{"seed-vc", "seed-whisper", "campplus"}},
		{"image.face", []string{"haar-frontal"}},
	} {
		s := openStore(t, t.TempDir())
		if _, err := s.Put(context.Background(), 0, fixture(t, tc.scene, tc.models...)); err == nil {
			t.Fatal("incomplete or ambiguous model set accepted", tc)
		}
	}
	// Multiple opaque entries in one model/group are accepted without guessing their filenames.
	b := fixture(t, "highlight.vision")
	f := b.Files[0]
	f.FileID, f.RelativePath, f.Identity = "projector", "arbitrary.payload", "projector-identity"
	b.Files = append(b.Files, f)
	put(t, openStore(t, t.TempDir()), 0, b)
}

func TestDeterministicHashAndUnforgeableRevision(t *testing.T) {
	b := fixture(t, "timbre.convert")
	s := openStore(t, t.TempDir())
	first := put(t, s, 0, b).Bindings[b.Scene]
	payload := first
	payload.Ref = integration.ExecutionBindingRef{}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if first.Ref.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("hash must cover canonical content with zero Ref, never itself")
	}
	for i, j := 0, len(b.Files)-1; i < j; i, j = i+1, j-1 {
		b.Files[i], b.Files[j] = b.Files[j], b.Files[i]
	}
	for i := range b.Files {
		b.Files[i].Modified = b.Files[i].Modified.In(time.FixedZone("offset", 3600))
	}
	second := put(t, s, 1, b).Bindings[b.Scene]
	if first.Ref.SHA256 != second.Ref.SHA256 || second.Ref.Revision != 2 {
		t.Fatal("hash depends on selection order/timezone or revision", first.Ref, second.Ref)
	}
	for _, ref := range []integration.ExecutionBindingRef{
		first.Ref,
		{Scene: b.Scene, Revision: 99, SHA256: first.Ref.SHA256},
		{Scene: "image.face", Revision: 2, SHA256: second.Ref.SHA256},
		{Scene: b.Scene, Revision: 2, SHA256: strings.Repeat("f", 64)},
		{Revision: 2},
	} {
		b.Ref = ref
		if _, err := s.Put(context.Background(), 2, b); err == nil || s.Snapshot().Revision != 2 {
			t.Fatal("forged or stale ref accepted", ref, err)
		}
	}
	// A current ref is an optimistic guard, not permission to retain its revision/hash after editing.
	b = second
	b.Files[0].Identity = "new-file-identity"
	third := put(t, s, 2, b).Bindings[b.Scene]
	if third.Ref.Revision != 3 || third.Ref.SHA256 == second.Ref.SHA256 {
		t.Fatal(third.Ref)
	}
	mutations := map[string]func(*Binding){
		"inventory": func(b *Binding) { b.InventoryID = "inventory-two" },
		"registry":  func(b *Binding) { b.RegistryRevision++ },
		"group":     func(b *Binding) { b.Files[0].Group = "new-group" },
		"location":  func(b *Binding) { b.Files[0].RelativePath = "new-location" },
		"bytes":     func(b *Binding) { b.Files[0].Bytes = "18" },
		"modified":  func(b *Binding) { b.Files[0].Modified = b.Files[0].Modified.Add(time.Second) },
		"component": func(b *Binding) { b.Component.Version = "v2" },
		"manifest":  func(b *Binding) { b.Component.ManifestSHA256 = strings.Repeat("b", 64) },
		"protocol":  func(b *Binding) { b.Component.Protocol = "test/v2" },
		"device":    func(b *Binding) { b.Device = "auto" },
		"registration": func(b *Binding) {
			f := &b.Files[0]
			f.RegistrationInputPath, f.RegistrationPath = f.ScopePath, f.ScopePath
			f.RegistrationKind, f.RegistrationIdentity = "directory", "registered-root"
			f.RegistrationUpdatedAt = time.Unix(1700000000, 0).UTC()
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			other := openStore(t, t.TempDir())
			base := fixture(t, "timbre.convert")
			a := put(t, other, 0, base).Bindings[base.Scene]
			mutate(&base)
			changed := put(t, other, 1, base).Bindings[base.Scene]
			if a.Ref.SHA256 == changed.Ref.SHA256 {
				t.Fatal("content change not hashed")
			}
		})
	}
}

func TestNoModelIOOrHistoricalConfigurationWrites(t *testing.T) {
	modelDir := t.TempDir()
	model := filepath.Join(modelDir, "weights.bin")
	original := []byte("never change model bytes")
	if err := os.WriteFile(model, original, 0400); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(model)
	if err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	history := filepath.Join(d, "paths.json")
	if err := os.WriteFile(history, []byte("historical-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, d)
	b := fixture(t, "highlight.score")
	b.Files[0].ScopePath, b.Files[0].RelativePath = modelDir, "weights.bin"
	put(t, s, 0, b)
	// Metadata persistence is independent of model availability; the Manager owns revalidation.
	b.Files[0].RelativePath = "nonexistent.bin"
	put(t, s, 1, b)
	if _, err := s.Delete(context.Background(), 2, b.Scene); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(model)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(model)
	if err != nil || !reflect.DeepEqual(raw, original) || !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		t.Fatal("model changed", err)
	}
	raw, err = os.ReadFile(history)
	if err != nil || string(raw) != "historical-secret" {
		t.Fatal("historical data modified", err)
	}
}
