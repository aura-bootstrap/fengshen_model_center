package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
)

func put(t *testing.T, p string, b []byte) {
	t.Helper()
	if e := os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
}
func putJSON(t *testing.T, p string, v any) {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	put(t, p, b)
}
func deliveryFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "model-center", "components", "model-center", "1")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	binary := []byte("fixture, never executed")
	put(t, filepath.Join(dir, "fengshen-model-center.exe"), binary)
	put(t, filepath.Join(root, filepath.FromSlash(centerPath)), binary)
	m := components.Manifest{Schema: 1, ComponentID: "model-center", Version: "1", OS: "windows", Arch: "amd64", EntryType: "executable", Entry: "fengshen-model-center.exe", Packaging: "go", StartupReportRef: "startup-report.json"}
	if e := generate(context.Background(), dir, m); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if e != nil {
		t.Fatal(e)
	}
	h := sha256.Sum256(b)
	zero := 0
	putJSON(t, filepath.Join(dir, "startup-report.json"), components.StartupReport{Schema: 1, ComponentID: "model-center", Version: "1", ManifestSHA256: hex.EncodeToString(h[:]), StartupTest: "passed", MetadataOnly: &components.MetadataOnlyEvidence{Arguments: []string{"--component-info"}, ExitCode: &zero, CheckedAt: time.Now().UTC(), NoModelLoad: true, NoNetwork: true, NoCompilation: true}, CleanEnvironment: "not_verified", ProtocolEvidence: "not_declared", Inference: "not_tested"})
	return root, dir
}
func TestWhisperStreamDeliveryDeclaresImplementedVAD(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "external", "whisper", "stream", "build.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	for _, required := range []string{"-or -not $info.vad_supported", "vad_supported=$true", "model_refs=@('whisper-asr','whisper-vad')"} {
		if !strings.Contains(source, required) {
			t.Fatalf("missing native VAD delivery gate: %s", required)
		}
	}
}

func TestWhisperBatchDeliveryContract(t *testing.T) {
	root := filepath.Join("..", "..")
	body, err := os.ReadFile(filepath.Join(root, "external", "whisper", "batch", "build.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"whisper\\stream\\main.cpp", "batch_test.cpp", "@('--component-info')", "fengshen.whispercpp.batch/v1", "4979e04-batch-v2", "-or -not $info.vad_supported", "vad_supported=$true", "model_refs=@('whisper-asr','whisper-vad')", "clean_environment='not_verified'", "inference='not_tested'", "Unclosed non-system DLL dependency"} {
		if !strings.Contains(string(body), required) {
			t.Fatalf("missing real batch delivery gate: %s", required)
		}
	}
	body, err = os.ReadFile("snapshot.go")
	if err != nil || !strings.Contains(string(body), "external/whisper/batch/build.ps1") {
		t.Fatal("batch builder omitted from source snapshot", err)
	}
}

func TestNativeResourcesIncludeAddedInputs(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "external", "whisper", "stream")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main.cpp", "CMakeLists.txt", "added-license.txt"} {
		put(t, filepath.Join(base, name), []byte("locked resource"))
	}
	seen := map[string]bool{}
	if err := nativeResourceInputs(root, func(p string) error { seen[filepath.Base(p)] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || !seen["added-license.txt"] || !seen["main.cpp"] {
		t.Fatalf("native input omission: %v", seen)
	}
}

func TestGenerateAndDeliveryNoDigestCycle(t *testing.T) {
	root, dir := deliveryFixture(t)
	e, err := components.New(filepath.Join(root, "model-center", "components")).Verify(context.Background(), "model-center", "1")
	if err != nil || e.Integrity != "matched" || e.StartupTest != "passed" {
		t.Fatalf("%+v %v", e, err)
	}
	if err = delivery(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(root, "model-center", "delivery.json"))
	if err = delivery(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "model-center", "delivery.json"))
	if string(a) != string(b) {
		t.Fatal("unstable delivery")
	}
	var d Delivery
	if json.Unmarshal(a, &d) != nil || len(d.Files) != 3 {
		t.Fatalf("%s", a)
	}
	var m components.Manifest
	manifest, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	json.Unmarshal(manifest, &m)
	for _, f := range m.Files {
		if f.Path == "manifest.json" || f.Path == m.StartupReportRef {
			t.Fatal("digest cycle")
		}
	}
	if generate(context.Background(), dir, m) == nil {
		t.Fatal("existing manifest silently replaced")
	}
}
func TestDeliveryRejectsIncompleteOrInvalidArtifacts(t *testing.T) {
	for _, name := range []string{"only-manifest", "no-exe", "no-manifest", "bad-manifest", "no-report", "wrong-report-sha", "report-upgrades-inference", "component-mismatch", "center-mismatch", "missing-report-ref"} {
		t.Run(name, func(t *testing.T) {
			root, dir := deliveryFixture(t)
			manifest := filepath.Join(dir, "manifest.json")
			report := filepath.Join(dir, "startup-report.json")
			switch name {
			case "only-manifest":
				os.Remove(filepath.Join(root, filepath.FromSlash(centerPath)))
				os.Remove(report)
			case "no-exe":
				os.Remove(filepath.Join(root, filepath.FromSlash(centerPath)))
			case "no-manifest":
				os.Remove(manifest)
			case "bad-manifest":
				put(t, manifest, []byte(`{"schema":1}`))
			case "no-report":
				os.Remove(report)
			case "wrong-report-sha":
				var p components.StartupReport
				b, _ := os.ReadFile(report)
				json.Unmarshal(b, &p)
				p.ManifestSHA256 = strings.Repeat("0", 64)
				putJSON(t, report, p)
			case "report-upgrades-inference":
				b, _ := os.ReadFile(report)
				put(t, report, []byte(strings.Replace(string(b), `"inference":"not_tested"`, `"inference":"passed"`, 1)))
			case "component-mismatch":
				put(t, filepath.Join(dir, "fengshen-model-center.exe"), []byte("modified"))
			case "center-mismatch":
				put(t, filepath.Join(root, filepath.FromSlash(centerPath)), []byte("modified"))
			case "missing-report-ref":
				var m components.Manifest
				b, _ := os.ReadFile(manifest)
				json.Unmarshal(b, &m)
				m.StartupReportRef = ""
				putJSON(t, manifest, m)
			}
			if err := delivery(context.Background(), root); err == nil {
				t.Fatal("invalid delivery admitted")
			}
			if _, e := os.Stat(filepath.Join(root, "model-center", "delivery.json")); !os.IsNotExist(e) {
				t.Fatal("failed delivery wrote evidence")
			}
		})
	}
}
func TestDeliveryRejectsLinkedMetadata(t *testing.T) {
	root, dir := deliveryFixture(t)
	report := filepath.Join(dir, "startup-report.json")
	b, e := os.ReadFile(report)
	if e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(t.TempDir(), "report.json")
	put(t, target, b)
	if e = os.Remove(report); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(target, report); e != nil {
		t.Skipf("symlink unavailable: %v", e)
	}
	if delivery(context.Background(), root) == nil {
		t.Fatal("linked report admitted")
	}
}

func TestLockRejectsSourceRootReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "source")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	input := []byte("same content")
	h := sha256.Sum256(input)
	put(t, filepath.Join(root, "input"), input)
	p := filepath.Join(parent, "lock.json")
	putJSON(t, p, Lock{Schema: 1, Inputs: []LockedInput{{Path: "input", SHA256: hex.EncodeToString(h[:]), Version: "1", License: "local"}}})
	if e := sealLock(context.Background(), root, p); e != nil {
		t.Fatal(e)
	}
	if e := os.Rename(root, filepath.Join(parent, "moved")); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	put(t, filepath.Join(root, "input"), input)
	if _, e := verifyLock(context.Background(), root, p); e == nil {
		t.Fatal("replaced source root admitted")
	}
}

func TestGenerateRejectsSourceEntries(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "run.py"), []byte("source"))
	m := components.Manifest{Schema: 1, ComponentID: "demucs-runner", Version: "1", OS: "windows", Arch: "amd64", EntryType: "executable", Entry: "run.py", Packaging: "pyinstaller-onedir"}
	if generate(context.Background(), root, m) == nil {
		t.Fatal("source entry admitted")
	}
}
func TestLockRequiresRealPinsAndRejectsReplacement(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(t.TempDir(), "inputs.lock.json")
	put(t, p, []byte(`{"schema":1,"inputs":[{"path":"input","sha256":"latest","version":"latest","license":""}]}`))
	if _, e := verifyLock(context.Background(), root, p); e == nil {
		t.Fatal("unpinned input accepted")
	}
	content := []byte("same content")
	h := sha256.Sum256(content)
	input := filepath.Join(root, "input")
	put(t, input, content)
	putJSON(t, p, Lock{Schema: 1, Inputs: []LockedInput{{Path: "input", SHA256: hex.EncodeToString(h[:]), Version: "1", License: "local"}}})
	if e := sealLock(context.Background(), root, p); e != nil {
		t.Fatal(e)
	}
	if _, e := verifyLock(context.Background(), root, p); e != nil {
		t.Fatal(e)
	}
	// Keep the old file alive to prevent immediate file-ID reuse.
	if e := os.Rename(input, filepath.Join(root, "old")); e != nil {
		t.Fatal(e)
	}
	put(t, input, content)
	if _, e := verifyLock(context.Background(), root, p); e == nil {
		t.Fatal("same-content replacement accepted")
	}
}
