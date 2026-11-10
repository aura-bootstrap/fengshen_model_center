package components

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func fixture(t *testing.T) (string, Manifest) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "whisper-batch", "1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("not an executable")
	if err := os.WriteFile(filepath.Join(dir, "runner.exe"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	m := Manifest{Schema: 1, ComponentID: "whisper-batch", Version: "1", OS: runtime.GOOS, Arch: runtime.GOARCH, EntryType: "executable", Entry: "runner.exe", Packaging: "native", Files: []File{{Path: "runner.exe", Bytes: "17", SHA256: hex.EncodeToString(sum[:])}}}
	save(t, dir, m)
	return root, m
}
func save(t *testing.T, dir string, m Manifest) {
	t.Helper()
	b, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
}
func find(t *testing.T, list []Evidence, id string) Evidence {
	t.Helper()
	for _, e := range list {
		if e.ComponentID == id {
			return e
		}
	}
	t.Fatal("missing component", id)
	return Evidence{}
}
func TestMissingAndOrthogonalEvidence(t *testing.T) {
	list := New(t.TempDir()).List()
	if len(list) != 7 {
		t.Fatalf("got %d", len(list))
	}
	for _, e := range list {
		if e.Packaging != "pending_build" || e.Inference != "not_tested" {
			t.Fatalf("%+v", e)
		}
	}
	root, manifest := fixture(t)
	mgr := New(root)
	e := find(t, mgr.List(), "whisper-batch")
	if e.FilesState != "complete" || e.Integrity != "not_checked" || e.StartupTest != "not_tested" || e.Inference != "not_tested" {
		t.Fatalf("%+v", e)
	}
	e, err := mgr.Verify(context.Background(), "whisper-batch", "1")
	if !reflect.DeepEqual(e.Manifest, &manifest) || e.StartupTest != "not_tested" || e.Inference != "not_tested" {
		t.Fatalf("verify lost declaration or promoted runtime evidence: %+v", e)
	}
	if err != nil || e.Integrity != "matched" {
		t.Fatalf("%+v %v", e, err)
	}
	if err = os.WriteFile(filepath.Join(root, "whisper-batch", "1", "runner.exe"), []byte("different payload"), 0600); err != nil {
		t.Fatal(err)
	}
	if e = find(t, mgr.List(), "whisper-batch"); e.Integrity != "stale" {
		t.Fatalf("old hash survived: %+v", e)
	}
	e, err = mgr.Verify(context.Background(), "whisper-batch", "1")
	if err == nil || e.Integrity != "mismatch" {
		t.Fatalf("%+v %v", e, err)
	}
}
func TestManifestDetailsJSON(t *testing.T) {
	root, manifest := fixture(t)
	manifest.Protocols = []string{"<script>declaration only</script>"}
	manifest.ModelRefs = []string{"<img src=x onerror=alert(1)>", "whisper-asr"}
	manifest.DriverRequirements = []string{"GPU driver <b>required</b>"}
	manifest.BuildInputs = []BuildInput{{Name: "tool", Version: "locked", SHA256: strings.Repeat("a", 64), License: "<svg onload=alert(1)>"}}
	// A declared dependency need not be delivered; do not invent a ready runtime.
	manifest.RuntimeDependencies = []Dependency{{ComponentID: "private-runtime", Version: "1"}}
	save(t, filepath.Join(root, manifest.ComponentID, manifest.Version), manifest)
	mgr := New(root)
	e := find(t, mgr.List(), manifest.ComponentID)
	assertDetails := func(e Evidence) {
		t.Helper()
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Manifest *Manifest `json:"manifest"`
		}
		if err := json.Unmarshal(b, &response); err != nil || response.Manifest == nil || !reflect.DeepEqual(*response.Manifest, manifest) {
			t.Fatalf("API evidence lost parsed declarations: %s (%v)", b, err)
		}
		if e.Integrity != "not_checked" || e.StartupTest != "not_tested" || e.Inference != "not_tested" || e.ProtocolEvidence != "declared_not_tested" {
			t.Fatalf("declarations promoted evidence: %+v", e)
		}
	}
	assertDetails(e)
	if e.FilesState != "dependency_missing" {
		t.Fatalf("dependency declaration became delivery: %+v", e)
	}
	// Verification rejection still returns the same read-only declaration.
	e, err := mgr.Verify(context.Background(), manifest.ComponentID, manifest.Version)
	if err == nil {
		t.Fatal("missing runtime dependency admitted")
	}
	assertDetails(e)
	// Even a valid declaration with a missing entry remains available, without
	// requiring the referenced binary, model, driver or build input contents.
	if err := os.Remove(filepath.Join(root, manifest.ComponentID, manifest.Version, manifest.Entry)); err != nil {
		t.Fatal(err)
	}
	assertDetails(find(t, mgr.List(), manifest.ComponentID))
}

func TestManifestDetailsAbsentUnlessValid(t *testing.T) {
	root, manifest := fixture(t)
	dir := filepath.Join(root, manifest.ComponentID, manifest.Version)
	b, _ := json.Marshal(manifest)
	wrongIdentity := manifest
	wrongIdentity.ComponentID = "model-center"
	wrong, _ := json.Marshal(wrongIdentity)
	for name, data := range map[string][]byte{
		"empty": {}, "null": []byte("null"), "empty-object": []byte("{}"),
		"unknown-field":     []byte(strings.Replace(string(b), `"schema":1`, `"schema":1,"unexpected":true`, 1)),
		"partially-decoded": []byte(strings.Replace(string(b), `"entry_type":"executable"`, `"entry_type":"script"`, 1)),
		"layout-identity":   wrong,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			e := find(t, New(root).List(), manifest.ComponentID)
			encoded, _ := json.Marshal(e)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if e.Manifest != nil || fields["manifest"] != nil || e.Packaging != "invalid_manifest" || e.ManifestSHA256 != "" {
				t.Fatalf("invalid declaration leaked: %s", encoded)
			}
		})
	}
	for _, e := range New(t.TempDir()).List() {
		encoded, _ := json.Marshal(e)
		if e.Manifest != nil || strings.Contains(string(encoded), `"manifest":`) {
			t.Fatalf("undelivered declaration invented: %s", encoded)
		}
	}
}

func TestStrictManifest(t *testing.T) {
	_, valid := fixture(t)
	b, _ := json.Marshal(valid)
	for name, data := range map[string][]byte{"large": bytes.Repeat([]byte(" "), MaxManifestBytes+1), "trailing": append(append([]byte{}, b...), []byte(" {}")...), "unknown": []byte(strings.Replace(string(b), `"schema":1`, `"schema":1,"surprise":true`, 1)), "duplicate-key": []byte(strings.Replace(string(b), `"schema":1`, `"schema":1,"schema":1`, 1))} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest(bytes.NewReader(data)); err == nil {
				t.Fatal("accepted malformed manifest")
			}
		})
	}
	for _, path := range []string{"../x", "/x", `C:\x`, `a\b`, "a/../b", "a//b", "a:stream", "CON", "a/NUL.txt", "a. ", "manifest.json"} {
		t.Run(path, func(t *testing.T) {
			m := valid
			m.Entry = path
			m.Files = []File{{Path: path, Bytes: "1", SHA256: strings.Repeat("0", 64)}}
			b, _ := json.Marshal(m)
			if _, err := ParseManifest(bytes.NewReader(b)); err == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
	m := valid
	m.Files = append(m.Files, m.Files[0])
	b, _ = json.Marshal(m)
	if _, err := ParseManifest(bytes.NewReader(b)); err == nil {
		t.Fatal("duplicate accepted")
	}
	m = valid
	m.RuntimeDependencies = []Dependency{{ComponentID: "whisper-batch", Version: "1"}}
	b, _ = json.Marshal(m)
	if _, err := ParseManifest(bytes.NewReader(b)); err == nil {
		t.Fatal("self cycle accepted")
	}
}
func TestCancellationAndPlatform(t *testing.T) {
	root, m := fixture(t)
	mgr := New(root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e, err := mgr.Verify(ctx, "whisper-batch", "1")
	if err == nil || e.Integrity == "matched" {
		t.Fatalf("%+v %v", e, err)
	}
	m.OS = "other"
	save(t, filepath.Join(root, "whisper-batch", "1"), m)
	e = find(t, mgr.List(), "whisper-batch")
	if e.Platform != "mismatch" || e.FilesState != "complete" {
		t.Fatalf("%+v", e)
	}
}
func TestWhisperStreamNeedsExactDeclaration(t *testing.T) {
	root, m := fixture(t)
	dir := filepath.Join(root, "whisper-stream", "1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	p, err := os.ReadFile(filepath.Join(root, "whisper-batch", "1", "runner.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runner.exe"), p, 0600); err != nil {
		t.Fatal(err)
	}
	m.ComponentID = "whisper-stream"
	for _, tc := range []struct {
		name      string
		protocols []string
		want      string
	}{
		{"absent", nil, "missing_required_declaration"},
		{"protocol_only", []string{"fengshen.whispercpp.stream/v1"}, "missing_required_declaration"},
		{"flag_only", []string{"--fengshen-stream-v1"}, "missing_required_declaration"},
		{"batch_protocol", []string{"fengshen.whispercpp.batch/v1", "--fengshen-stream-v1"}, "missing_required_declaration"},
		{"complete", []string{"fengshen.whispercpp.stream/v1", "--fengshen-stream-v1"}, "declared_not_tested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m.Protocols = tc.protocols
			save(t, dir, m)
			mgr := New(root)
			e := find(t, mgr.List(), "whisper-stream")
			if e.ProtocolEvidence != tc.want || e.StartupTest != "not_tested" || e.Inference != "not_tested" {
				t.Fatalf("%+v", e)
			}
			e, err := mgr.Verify(context.Background(), "whisper-stream", "1")
			if err != nil || e.ProtocolEvidence != tc.want || e.Integrity != "matched" || e.StartupTest != "not_tested" || e.Inference != "not_tested" {
				t.Fatalf("%+v %v", e, err)
			}
		})
	}
}
func TestSymlinkRejected(t *testing.T) {
	root, _ := fixture(t)
	dir := filepath.Join(root, "whisper-batch", "1")
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("not an executable"), 0600)
	os.Remove(filepath.Join(dir, "runner.exe"))
	if err := os.Symlink(outside, filepath.Join(dir, "runner.exe")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	e := find(t, New(root).List(), "whisper-batch")
	if e.FilesState == "complete" {
		t.Fatal("link admitted")
	}
}
