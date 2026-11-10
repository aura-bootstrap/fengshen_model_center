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
	"strconv"
	"strings"
	"testing"
)

func TestCrossManifestCycleAndMissing(t *testing.T) {
	root, m := fixture(t)
	dir := filepath.Join(root, "demucs-runner", "1")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "runner.exe"), []byte("not an executable"), 0600)
	n := m
	n.ComponentID = "demucs-runner"
	n.RuntimeDependencies = []Dependency{{"whisper-batch", "1"}}
	save(t, dir, n)
	m.RuntimeDependencies = []Dependency{{"demucs-runner", "1"}}
	save(t, filepath.Join(root, "whisper-batch", "1"), m)
	mgr := New(root)
	e := find(t, mgr.List(), "whisper-batch")
	if e.FilesState != "dependency_cycle" {
		t.Fatalf("%+v", e)
	}
	if _, err := mgr.Verify(context.Background(), "whisper-batch", "1"); err == nil {
		t.Fatal("cycle verified")
	}
	os.RemoveAll(dir)
	e = find(t, mgr.List(), "whisper-batch")
	if e.FilesState != "dependency_missing" {
		t.Fatalf("%+v", e)
	}
}
func TestManifestExactLimitAndCaseDuplicates(t *testing.T) {
	_, m := fixture(t)
	b, _ := json.Marshal(m)
	padded := append(b, []byte(strings.Repeat(" ", MaxManifestBytes-len(b)))...)
	if _, e := ParseManifest(strings.NewReader(string(padded))); e != nil {
		t.Fatal(e)
	}
	m.Files = append(m.Files, File{Path: "RUNNER.EXE", Bytes: "17", SHA256: m.Files[0].SHA256})
	b, _ = json.Marshal(m)
	if _, e := ParseManifest(strings.NewReader(string(b))); e == nil {
		t.Fatal("case collision")
	}
}
func TestValidateManifestFileCountBoundaries(t *testing.T) {
	_, valid := fixture(t)
	if err := Validate(valid); err != nil {
		t.Fatalf("invalid baseline manifest: %v", err)
	}

	// Validate directly: a serialized list this large exceeds the separate
	// ParseManifest byte limit. Keep the entry and every file otherwise valid.
	files := make([]File, 100001)
	files[0] = valid.Files[0]
	for i := 1; i < len(files); i++ {
		files[i] = File{
			Path:   "data/file-" + strconv.Itoa(i) + ".bin",
			Bytes:  "17",
			SHA256: valid.Files[0].SHA256,
		}
	}
	for _, tc := range []struct {
		name    string
		count   int
		wantErr bool
	}{
		{name: "exactly_100000", count: 100000},
		{name: "over_limit_100001", count: 100001, wantErr: true},
		{name: "zero", count: 0, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := valid
			m.Files = files[:tc.count]
			err := Validate(m)
			if tc.wantErr {
				// In particular, zero files must fail the count check, not the
				// later check that the entry is present in the list.
				if err == nil || err.Error() != "invalid file count" {
					t.Fatalf("Validate with %d files: got %v, want invalid file count", tc.count, err)
				}
			} else if err != nil {
				t.Fatalf("Validate rejected %d valid files: %v", tc.count, err)
			}
		})
	}
}

func TestParseManifestByteLimitBoundaries(t *testing.T) {
	const limit = 256 * 1024
	_, valid := fixture(t)
	b, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > limit {
		t.Fatalf("baseline manifest is already over the byte limit: %d", len(b))
	}
	// Only trailing JSON whitespace differs between the two inputs.
	padded := append(b, bytes.Repeat([]byte(" "), limit-len(b))...)
	for _, tc := range []struct {
		name    string
		data    []byte
		size    int
		wantErr bool
	}{
		{name: "exactly_256KiB", data: padded, size: limit},
		{name: "over_limit_by_one", data: append(padded, ' '), size: limit + 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.data) != tc.size {
				t.Fatalf("input has %d bytes, want %d", len(tc.data), tc.size)
			}
			// Bypass only the byte cap to prove neither JSON decoding nor
			// semantic validation can be the reason for rejection.
			var decoded Manifest
			if err := StrictJSON(tc.data, &decoded); err != nil {
				t.Fatalf("boundary input is not strict JSON: %v", err)
			}
			if err := Validate(decoded); err != nil {
				t.Fatalf("boundary input is semantically invalid: %v", err)
			}
			if !reflect.DeepEqual(decoded, valid) {
				t.Fatal("whitespace padding changed the manifest")
			}
			got, err := ParseManifest(bytes.NewReader(tc.data))
			if tc.wantErr {
				if err == nil || err.Error() != "manifest exceeds 256 KiB" {
					t.Fatalf("ParseManifest with %d bytes: got %v, want manifest exceeds 256 KiB", tc.size, err)
				}
			} else {
				if err != nil {
					t.Fatalf("ParseManifest rejected exactly 256 KiB: %v", err)
				}
				if !reflect.DeepEqual(got, valid) {
					t.Fatal("ParseManifest changed the manifest at the exact limit")
				}
			}
		})
	}
}

func TestLargeSequentialVerifyAndEmpty(t *testing.T) {
	root, m := fixture(t)
	payload := []byte(strings.Repeat("0123456789abcdef", 200000))
	sum := sha256.Sum256(payload)
	m.Files[0].Bytes = "3200000"
	m.Files[0].SHA256 = hex.EncodeToString(sum[:])
	dir := filepath.Join(root, "whisper-batch", "1")
	os.WriteFile(filepath.Join(dir, "runner.exe"), payload, 0600)
	save(t, dir, m)
	mgr := New(root)
	e, err := mgr.Verify(context.Background(), "whisper-batch", "1")
	if err != nil || e.Integrity != "matched" {
		t.Fatalf("%+v %v", e, err)
	}
	os.WriteFile(filepath.Join(dir, "runner.exe"), nil, 0600)
	e = find(t, mgr.List(), "whisper-batch")
	if e.FilesState != "incomplete" || e.Integrity != "stale" {
		t.Fatalf("%+v", e)
	}
}
func TestChangesAndCancellationDuringRead(t *testing.T) {
	for _, cancelRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "mutation", true: "cancel"}[cancelRead], func(t *testing.T) {
			root, _ := fixture(t)
			mgr := New(root)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			once := false
			mgr.afterRead = func() {
				if once {
					return
				}
				once = true
				if cancelRead {
					cancel()
				} else {
					if err := os.WriteFile(filepath.Join(root, "whisper-batch", "1", "runner.exe"), []byte("changed-size"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			e, err := mgr.Verify(ctx, "whisper-batch", "1")
			if err == nil || e.Integrity == "matched" {
				t.Fatalf("invalid successful digest: %+v %v", e, err)
			}
		})
	}
}
func TestFrozenInfoDoesNotImportInference(t *testing.T) {
	// Static contract regression, not a claim that a frozen binary was built.
	b, err := os.ReadFile(filepath.Join("..", "sidecar", "model_center_frozen_runner.py"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, bad := range []string{"subprocess", "sys.executable,", "runpy", "shell=True"} {
		if strings.Contains(s, bad) {
			t.Fatalf("unsafe frozen dispatch %s", bad)
		}
	}
	if !strings.Contains(s, `if argv == ["--component-info"]:`) {
		t.Fatal("missing metadata-only dispatch")
	}
	t.Logf("%s/%s frozen artifact and clean environment NOT verified", runtime.GOOS, runtime.GOARCH)
}
