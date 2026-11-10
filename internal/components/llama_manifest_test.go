package components

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLlamaManifestAdmission(t *testing.T) {
	count := 0
	for _, e := range New(t.TempDir()).List() {
		if e.ComponentID == "llama-runtime" {
			count++
			if e.Packaging != "pending_build" || e.Inference != "not_tested" {
				t.Fatal("missing llama delivery became ready")
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected one llama-runtime item, got %d", count)
	}
	m := Manifest{Schema: 1, ComponentID: "llama-runtime", Version: "b12182-fengshen1-cpu", OS: "windows", Arch: "amd64", EntryType: "executable", Entry: "llama-runtime.exe", Packaging: "native", Protocols: []string{"llama.cpp.server.http/b12182"}, Files: []File{{Path: "llama-runtime.exe", Bytes: "1", SHA256: strings.Repeat("a", 64)}}, StartupReportRef: "startup-report.json"}
	if err := Validate(m); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"launcher.ps1", "launcher.py", "../llama-server.exe"} {
		bad := m
		bad.Entry = entry
		if Validate(bad) == nil {
			t.Fatalf("accepted non-delivery entry %q", entry)
		}
	}
	// Metadata evidence cannot claim real inference or an authenticated server.
	report := StartupReport{Schema: 1, ComponentID: m.ComponentID, Version: m.Version, ManifestSHA256: strings.Repeat("b", 64), StartupTest: "not_tested", CleanEnvironment: "not_verified", ProtocolEvidence: "declared_not_tested", Inference: "passed"}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseStartupReport(bytes.NewReader(data), m, report.ManifestSHA256); err == nil {
		t.Fatal("metadata promoted to inference")
	}
	report.Inference = "not_tested"
	data, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseStartupReport(bytes.NewReader(data), m, report.ManifestSHA256); err != nil {
		t.Fatal(err)
	}
}

// Explicit opt-in: verify actual external delivery, without executing any binary or loading weights.
func TestLlamaDeliveredManifest(t *testing.T) {
	root := os.Getenv("LLAMA_RUNTIME_DELIVERY")
	if root == "" {
		t.Skip("set LLAMA_RUNTIME_DELIVERY to an explicit built component directory")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("delivery path must be absolute")
	}
	raw, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.ComponentID != "llama-runtime" || m.Packaging != "native" || m.Entry != "llama-runtime.exe" {
		t.Fatal("wrong delivery identity")
	}
	found := map[string]bool{}
	for _, f := range m.Files {
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		stat, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if !stat.Mode().IsRegular() {
			t.Fatalf("not a regular file: %s", p)
		}
		file, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		n, err := io.Copy(h, file)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("read %s: %v %v", p, err, closeErr)
		}
		if n != stat.Size() || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
			t.Fatalf("delivery digest mismatch: %s", p)
		}
		if strconv.FormatInt(n, 10) != f.Bytes {
			t.Fatalf("delivery size mismatch: %s", p)
		}
		found[f.Path] = true
	}
	for _, required := range []string{"internal/llama-server.exe", "source-lock.json", "supervisor-contract.json", "build-evidence.json", "licenses/llama-LICENSE", "server-version.txt", "server-help.txt", "pe-dependencies.txt"} {
		if !found[required] {
			t.Fatalf("incomplete delivery: %s", required)
		}
	}
	buildRaw, err := os.ReadFile(filepath.Join(root, "build-evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var build struct {
		BinarySource string `json:"binary_source"`
		Reused       bool   `json:"upstream_binary_reused"`
		Commit       string `json:"source_commit"`
	}
	if err := json.Unmarshal(buildRaw, &build); err != nil {
		t.Fatal(err)
	}
	if build.BinarySource != "source-build" || build.Reused || build.Commit != "fd27b2034905f8dadd8bd6d2cc44595f4929d391" {
		t.Fatal("unproven source-build identity")
	}
	sum := sha256.Sum256(raw)
	reportRaw, err := os.ReadFile(filepath.Join(root, m.StartupReportRef))
	if err != nil {
		t.Fatal(err)
	}
	report, err := ParseStartupReport(bytes.NewReader(reportRaw), m, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if report.StartupTest != "passed" || report.CleanEnvironment != "not_verified" || report.Inference != "not_tested" {
		t.Fatal("unexpected evidence claim")
	}
	// A modified manifest must invalidate the exact-byte startup identity.
	if _, err := ParseStartupReport(bytes.NewReader(reportRaw), m, strings.Repeat("0", 64)); err == nil {
		t.Fatal("stale report accepted")
	}
}
