//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Exclusive handles distinguish unreadable consumer fixtures from merely unchanged files.
func TestWindowsNonintrusiveConsumerSentinels(t *testing.T) {
	m := defaultScopeManager(t)
	appData := filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	t.Setenv("APPDATA", appData)
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(configDir) != filepath.Clean(appData) {
		t.Fatalf("consumer config directory escaped fixture: %q", configDir)
	}

	var requests atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()

	type fingerprint struct {
		sha   [sha256.Size]byte
		mtime time.Time
	}
	fingerprintFile := func(path string) fingerprint {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read fingerprint %s: %v", path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return fingerprint{sha256.Sum256(body), info.ModTime()}
	}
	before := map[string]fingerprint{}
	var sentinels []string
	add := func(path, body string) {
		t.Helper()
		put(t, path, body)
		before[path] = fingerprintFile(path)
		sentinels = append(sentinels, path)
	}
	addJSON := func(path string, value any) {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		add(path, string(body))
	}

	// Use real current-user DPAPI ciphertext, not plaintext disguised as a secret.
	// Only this test process calls CryptProtectData; no consumer/tool is launched.
	ciphertext := nonintrusiveDPAPIPayload(t, []byte("offline-consumer-secret-sentinel"))
	const revision = "sr_nonintrusive_windows"
	for _, consumer := range []string{"user_client", "dev_client"} {
		exeDir := filepath.Join(m.root, "src", consumer)
		dataRoot := exeDir
		if consumer == "dev_client" {
			dataRoot = filepath.Join(exeDir, "v3")
		}
		addJSON(filepath.Join(dataRoot, "highlight-config.json"), map[string]any{
			"version": 1,
			"local":   map[string]any{}, "analysis": map[string]any{},
			"scoring":         map[string]any{"provider": "local", "base_url": endpoint.URL + "/v1", "model": "sentinel", "timeout_sec": 1},
			"secret_revision": revision,
			"local_vision":    map[string]any{"enabled": false, "base_url": endpoint.URL + "/v1", "model": "sentinel", "timeout_sec": 1, "secret_revision": ""},
		})
		add(filepath.Join(dataRoot, "highlight-secrets", revision+".bin"), string(ciphertext))
		execution := map[string]any{"development_config_version": 1, "secret_revision": revision}
		if consumer == "user_client" {
			// Legacy user execution stores a server config reference, not a
			// development_config_version (catalog.Execution.validateLegacy).
			execution = map[string]any{"server_config_ref": map[string]any{
				"track": "traditional", "version": 1,
				"sha256": fmt.Sprintf("%064x", 1), "secret_revision": revision,
			}}
		}
		addJSON(filepath.Join(exeDir, "workspace", "tasks", "1", "highlight_catalog.json"), map[string]any{
			"schema_version": 1, "task_id": "1", "task_mode": "highlight",
			"input_fingerprint": fmt.Sprintf("%064x", 2), "execution": execution,
			"selected": []any{}, "analysis_summary": map[string]any{},
		})
	}
	addJSON(filepath.Join(m.root, "src", "admin_client", "ui_settings.json"), map[string]any{"theme_color": "blue"})
	addJSON(filepath.Join(configDir, "FengshenCardAdmin", "remember.json"), map[string]any{"token": "admin-remember-sentinel"})

	weight := filepath.Join(m.root, "models", "955717e8-sentinel.th")
	put(t, weight, "tiny Demucs filename fixture; not loadable weights\x00\x01")
	before[weight] = fingerprintFile(weight)

	var handles []syscall.Handle
	closeHandles := func() {
		for _, handle := range handles {
			if err := syscall.CloseHandle(handle); err != nil {
				t.Errorf("CloseHandle: %v", err)
			}
		}
		handles = nil
	}
	// defer also runs after Fatal/Goexit and before t.TempDir cleanup. Register
	// it before the first CreateFile so partial acquisition failures are safe.
	defer closeHandles()
	for _, path := range sentinels {
		name, err := syscall.UTF16PtrFromString(path)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			t.Fatalf("exclusive CreateFile %s: %v", path, err)
		}
		handles = append(handles, handle)
	}
	assertUnreadable := func() {
		t.Helper()
		for _, path := range sentinels {
			_, err := os.ReadFile(path)
			var pathErr *os.PathError
			// Win32 ERROR_SHARING_VIOLATION = 32. EACCES / ACL errors,
			// missing files, and generic os.IsPermission are not evidence.
			if !errors.As(err, &pathErr) || pathErr.Op != "open" || !errors.Is(pathErr.Err, syscall.Errno(32)) {
				t.Fatalf("expected open ERROR_SHARING_VIOLATION for %s, got %T: %v", path, err, err)
			}
		}
	}
	assertUnreadable()

	state := m.State()
	if state.Registry.Revision != 0 {
		t.Fatalf("unexpected initial registry: %+v", state.Registry)
	}
	if evidence := m.Components(); len(evidence) == 0 {
		t.Fatal("Components returned no component diagnostics")
	}
	inv := scanFixture(t, m)
	if !inv.Complete {
		t.Fatalf("locked consumer files made scan incomplete: %+v", inv.Diagnostics)
	}
	files, _, err := m.Files(inv.ID, "demucs", "", 200)
	if err != nil || len(files) != 1 {
		t.Fatalf("Demucs fixture files: %+v, %v", files, err)
	}
	op, err := m.StartVerify(context.Background(), inv.ID, "demucs", []string{files[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	done := await(t, m, op.ID)
	weightSHA := before[weight].sha
	if done.Status != "completed" || len(done.Hashes) != 1 || done.Hashes[0].Status != "fingerprint_computed" || done.Hashes[0].SHA256 != hex.EncodeToString(weightSHA[:]) {
		t.Fatalf("ID-selected hash failed with consumers locked: %+v", done)
	}
	registered, err := m.Registration(context.Background(), "demucs", weight, m.State().Registry.Revision, false)
	if err != nil {
		t.Fatal(err)
	}
	if registered.Revision != state.Registry.Revision+1 {
		t.Fatalf("registration revision did not advance: %+v", registered)
	}
	removed, err := m.Registration(context.Background(), "demucs", "", registered.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	if removed.Revision != registered.Revision+1 || len(removed.Registrations) != 0 {
		t.Fatalf("unregistration failed: %+v", removed)
	}
	again := scanFixture(t, m)
	if !again.Complete || again.RegistryRevision != removed.Revision {
		t.Fatalf("second scan failed: %+v", again)
	}
	files, _, err = m.Files(again.ID, "demucs", "", 200)
	if err != nil || len(files) != 1 {
		t.Fatalf("default-scope weight disappeared after unregistration: %+v, %v", files, err)
	}
	m.State()
	m.Components()
	assertUnreadable()
	if got := requests.Load(); got != 0 {
		t.Fatalf("center contacted consumer-configured loopback: %d requests", got)
	}

	closeHandles()
	for path, want := range before {
		got := fingerprintFile(path)
		if got.sha != want.sha || !got.mtime.Equal(want.mtime) {
			t.Errorf("sentinel/weight changed: %s: before=%+v after=%+v", path, want, got)
		}
	}
	// Unchanged hashes prove non-mutation only. The exclusive handles plus
	// exact sharing-violation probes above prove the sentinels were unreadable
	// throughout the operations; this does not audit attempted/ignored opens.
	t.Logf("%d consumer sentinels unreadable across State/Components/scan/hash/register/unregister/rescan; SHA/mtime preserved for sentinels and weight; loopback requests=%d", len(sentinels), requests.Load())
}

func nonintrusiveDPAPIPayload(t *testing.T, plaintext []byte) []byte {
	t.Helper()
	type blob struct {
		size uint32
		data *byte
	}
	in := blob{uint32(len(plaintext)), &plaintext[0]}
	var out blob
	protect := syscall.NewLazyDLL("crypt32.dll").NewProc("CryptProtectData")
	free := syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")
	ok, _, err := protect.Call(uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&out)))
	runtime.KeepAlive(plaintext)
	if ok == 0 {
		t.Fatalf("CryptProtectData: %v", err)
	}
	defer free.Call(uintptr(unsafe.Pointer(out.data)))
	return append([]byte{'F', 'S', 'H', 'S', 1, 0}, unsafe.Slice(out.data, int(out.size))...)
}
