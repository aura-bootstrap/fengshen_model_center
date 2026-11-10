package components

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reportFixture(t *testing.T) (string, Manifest, StartupReport) {
	t.Helper()
	root, m := fixture(t)
	m.StartupReportRef = "startup.json"
	dir := ComponentDirectory(root, m.ComponentID, m.Version)
	save(t, dir, m)
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	zero := 0
	p := StartupReport{Schema: 1, ComponentID: m.ComponentID, Version: m.Version, ManifestSHA256: hex.EncodeToString(h[:]), StartupTest: "passed", MetadataOnly: &MetadataOnlyEvidence{Arguments: []string{"--component-info"}, ExitCode: &zero, CheckedAt: time.Now().UTC(), NoModelLoad: true, NoNetwork: true, NoCompilation: true}, CleanEnvironment: "not_verified", ProtocolEvidence: "not_declared", Inference: "not_tested"}
	writeReport(t, dir, p)
	return root, m, p
}
func writeReport(t *testing.T, dir string, p StartupReport) {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "startup.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestHaarMetadataArgumentIsComponentSpecific(t *testing.T) {
	_, m, p := reportFixture(t)
	for _, id := range IDs {
		for _, args := range [][]string{{"--haar-component-info"}, {"--haar-component-info", "--component-info"}, {"--haar-component-info=true"}, {"--haar-component-info", "--root=outside"}} {
			m.ComponentID, p.ComponentID = id, id
			p.MetadataOnly.Arguments = args
			b, _ := json.Marshal(p)
			_, err := ParseStartupReport(bytes.NewReader(b), m, p.ManifestSHA256)
			if (err == nil) != (id == "haar-consumer" && len(args) == 1 && args[0] == "--haar-component-info") {
				t.Fatalf("Haar metadata argv %v admitted for %s: %v", args, id, err)
			}
		}
	}
}

func TestNativeStreamMetadataArgumentIsComponentSpecific(t *testing.T) {
	_, m, p := reportFixture(t)
	p.MetadataOnly.Arguments = []string{"--metadata"}
	for _, id := range IDs {
		m.ComponentID, p.ComponentID = id, id
		b, _ := json.Marshal(p)
		_, err := ParseStartupReport(bytes.NewReader(b), m, p.ManifestSHA256)
		if (err == nil) != (id == "whisper-stream") {
			t.Fatalf("metadata argv admitted for %s: %v", id, err)
		}
	}
}

// Explicit opt-in checks real build output without executing or loading a model.
func TestNativeStreamDelivery(t *testing.T) {
	root := os.Getenv("FENGSHEN_STREAM_COMPONENTS_TEST_ROOT")
	if root == "" {
		t.Skip("explicit native delivery root not provided")
	}
	m, err := New(root).Verify(context.Background(), "whisper-stream", "4979e04-stream-v1")
	if err != nil || m.Integrity != "matched" || m.StartupTest != "passed" || m.CleanEnvironment != "not_verified" || m.Inference != "not_tested" {
		t.Fatalf("real native delivery: %+v %v", m, err)
	}
}

func TestStartupReportAdmission(t *testing.T) {
	root, _, _ := reportFixture(t)
	mgr := New(root)
	e := find(t, mgr.List(), "whisper-batch")
	if e.StartupTest != "passed" || e.CleanEnvironment != "not_verified" || e.Inference != "not_tested" || e.Integrity != "not_checked" {
		t.Fatalf("%+v", e)
	}
	e, err := mgr.Verify(context.Background(), "whisper-batch", "1")
	if err != nil || e.StartupTest != "passed" || e.Integrity != "matched" {
		t.Fatalf("%+v %v", e, err)
	}
}
func TestStartupReportStrictContract(t *testing.T) {
	_, m, p := reportFixture(t)
	tests := map[string]func(*StartupReport){
		"schema":                 func(p *StartupReport) { p.Schema = 2 },
		"id":                     func(p *StartupReport) { p.ComponentID = "seedvc-runner" },
		"version":                func(p *StartupReport) { p.Version = "2" },
		"digest":                 func(p *StartupReport) { p.ManifestSHA256 = strings.Repeat("0", 64) },
		"inference":              func(p *StartupReport) { p.Inference = "passed" },
		"protocol":               func(p *StartupReport) { p.ProtocolEvidence = "passed" },
		"missing-evidence":       func(p *StartupReport) { p.MetadataOnly = nil },
		"command":                func(p *StartupReport) { p.MetadataOnly.Arguments = []string{"--infer"} },
		"exit-required":          func(p *StartupReport) { p.MetadataOnly.ExitCode = nil },
		"no-time":                func(p *StartupReport) { p.MetadataOnly.CheckedAt = time.Time{} },
		"network":                func(p *StartupReport) { p.MetadataOnly.NoNetwork = false },
		"model-load":             func(p *StartupReport) { p.MetadataOnly.NoModelLoad = false },
		"compilation":            func(p *StartupReport) { p.MetadataOnly.NoCompilation = false },
		"clean-without-evidence": func(p *StartupReport) { p.CleanEnvironment = "validated" },
		"clean-forged-state":     func(p *StartupReport) { p.CleanEnvironment = "verified" },
	}
	original, _ := json.Marshal(p)
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			var q StartupReport
			json.Unmarshal(original, &q)
			mutate(&q)
			b, _ := json.Marshal(q)
			if _, err := ParseStartupReport(bytes.NewReader(b), m, p.ManifestSHA256); err == nil {
				t.Fatal("invalid report admitted")
			}
		})
	}
	for name, b := range map[string][]byte{"unknown": []byte(strings.Replace(string(original), `"schema":1`, `"schema":1,"unknown":true`, 1)), "duplicate": []byte(strings.Replace(string(original), `"schema":1`, `"schema":1,"schema":1`, 1)), "trailing": append(append([]byte{}, original...), []byte(" {}")...), "null": []byte("null"), "oversize": bytes.Repeat([]byte(" "), MaxStartupReportBytes+1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseStartupReport(bytes.NewReader(b), m, p.ManifestSHA256); err == nil {
				t.Fatal("malformed report admitted")
			}
		})
	}
	padded := append(original, bytes.Repeat([]byte(" "), MaxStartupReportBytes-len(original))...)
	if _, err := ParseStartupReport(bytes.NewReader(padded), m, p.ManifestSHA256); err != nil {
		t.Fatal(err)
	}
}
func TestCleanEnvironmentIsRecordedNotAuthenticated(t *testing.T) {
	root, m, p := reportFixture(t)
	p.CleanEnvironment = "validated"
	p.CleanEnvironmentEvidence = &CleanEnvironmentEvidence{Kind: "vm", EnvironmentRef: "isolated-test-image", EvidenceRef: "retained-run-1", NoHostPython: true, NoHostCompiler: true, NoDevelopmentMounts: true}
	writeReport(t, ComponentDirectory(root, m.ComponentID, m.Version), p)
	e := find(t, New(root).List(), m.ComponentID)
	if e.StartupTest != "passed" || e.CleanEnvironment != "not_verified" || e.StartupReport == nil || e.StartupReport.CleanEnvironment != "validated" {
		t.Fatalf("%+v", e)
	}
	p.CleanEnvironmentEvidence.Kind = "path-only"
	b, _ := json.Marshal(p)
	if _, err := ParseStartupReport(bytes.NewReader(b), m, p.ManifestSHA256); err == nil {
		t.Fatal("PATH-only accepted")
	}
}
func TestReportChangesInvalidateOldEvidence(t *testing.T) {
	for _, change := range []string{"size", "mtime", "identity", "missing", "binary", "manifest"} {
		t.Run(change, func(t *testing.T) {
			root, m, _ := reportFixture(t)
			mgr := New(root)
			if _, err := mgr.Verify(context.Background(), m.ComponentID, m.Version); err != nil {
				t.Fatal(err)
			}
			dir := ComponentDirectory(root, m.ComponentID, m.Version)
			path := filepath.Join(dir, "startup.json")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "size":
				err = os.WriteFile(path, append(b, ' '), 0600)
			case "mtime":
				err = os.Chtimes(path, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
			case "identity":
				tmp := filepath.Join(dir, "replacement")
				if err = os.WriteFile(tmp, b, 0600); err == nil {
					err = os.Remove(path)
				}
				if err == nil {
					err = os.Rename(tmp, path)
				}
			case "missing":
				err = os.Remove(path)
			case "binary":
				err = os.WriteFile(filepath.Join(dir, m.Entry), []byte("changed"), 0600)
			case "manifest":
				m.DriverRequirements = []string{"changed"}
				save(t, dir, m)
			}
			if err != nil {
				t.Fatal(err)
			}
			e := find(t, mgr.List(), m.ComponentID)
			if e.StartupTest != "stale" || e.Integrity != "stale" || e.StartupReport != nil || e.CleanEnvironment != "not_verified" {
				t.Fatalf("%+v", e)
			}
			e = find(t, mgr.List(), m.ComponentID)
			if e.StartupTest != "stale" {
				t.Fatalf("stale evidence revived: %+v", e)
			}
		})
	}
}
func TestStartupReportLinkRejected(t *testing.T) {
	root, m, _ := reportFixture(t)
	dir := ComponentDirectory(root, m.ComponentID, m.Version)
	report := filepath.Join(dir, m.StartupReportRef)
	target := filepath.Join(t.TempDir(), "report.json")
	b, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(target, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(report); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, report); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	e := find(t, New(root).List(), m.ComponentID)
	if e.StartupTest != "invalid_report" || e.StartupReport != nil {
		t.Fatalf("linked report admitted: %+v", e)
	}
}

func TestReportMissingInvalidAndExcludedFromFiles(t *testing.T) {
	root, m, _ := reportFixture(t)
	dir := ComponentDirectory(root, m.ComponentID, m.Version)
	if err := os.Remove(filepath.Join(dir, m.StartupReportRef)); err != nil {
		t.Fatal(err)
	}
	if e := find(t, New(root).List(), m.ComponentID); e.StartupTest != "not_tested" {
		t.Fatalf("%+v", e)
	}
	if err := os.WriteFile(filepath.Join(dir, m.StartupReportRef), []byte(`{"schema":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if e := find(t, New(root).List(), m.ComponentID); e.StartupTest != "invalid_report" {
		t.Fatalf("%+v", e)
	}
	m.Files = append(m.Files, File{Path: "STARTUP.JSON", Bytes: "1", SHA256: strings.Repeat("0", 64)})
	if Validate(m) == nil {
		t.Fatal("report included in manifest.Files")
	}
}
