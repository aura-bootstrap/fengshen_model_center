//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

// This opt-in suite launches only its freshly built executable. Never run in
// parallel: APPDATA is process-wide and the production launch port is fixed.
func TestTrustedCenterE2E(t *testing.T) {
	if os.Getenv("FENGSHEN_CENTER_E2E") != "1" {
		t.Skip("explicit opt-in required: FENGSHEN_CENTER_E2E=1")
	}
	probe, err := net.Listen("tcp", "127.0.0.1:18120")
	if err != nil {
		t.Skipf("18120 occupied; no request or termination attempted: %v", err)
	}
	probe.Close()
	root, data, appdata := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("APPDATA", appdata)
	configDir, err := os.UserConfigDir()
	if err != nil || configDir != appdata {
		t.Fatalf("UserConfigDir did not honor isolated APPDATA: %q, %v", configDir, err)
	}
	registryDir := filepath.Join(appdata, "FengshenSlicer", "model-center")
	if err := os.MkdirAll(registryDir, 0700); err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(registryDir, "registrations.json")
	e2eWrite(t, registryPath, []byte(`{"schema_version":1,"revision":7,"registrations":{}}`))
	centerDir := filepath.Join(root, "model-center")
	if err := os.MkdirAll(centerDir, 0700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(centerDir, "fengshen-model-center.exe")
	build := exec.Command("go", "build", "-o", exe, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build current center: %v\n%s", err, output)
	}
	e2eWrite(t, filepath.Join(root, "resource-layout.json"), []byte(`{"schema":1,"center":"model-center/fengshen-model-center.exe","components":"model-center/components","root_kind":"installed","model_roots":[],"haar_files":[]}`))
	type deliveryFile struct {
		Path   string `json:"path"`
		Bytes  string `json:"bytes"`
		SHA256 string `json:"sha256"`
	}
	files := []deliveryFile{}
	for _, rel := range []string{"model-center/fengshen-model-center.exe", "resource-layout.json"} {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, deliveryFile{rel, fmt.Sprint(len(b)), e2eDigest(b)})
	}
	manifest, err := json.Marshal(map[string]any{"schema": 1, "files": files, "inference": "not_tested", "clean_environment": "not_verified"})
	if err != nil {
		t.Fatal(err)
	}
	e2eWrite(t, filepath.Join(centerDir, "delivery.json"), manifest)
	cfg := integration.Config{InstallRoot: root, Root: root, RootKind: "installed", DataDir: data, CenterSHA256: files[0].SHA256, DeliverySHA256: e2eDigest(manifest)}
	client := e2eClient(t, cfg)
	t.Logf("built current EXE once: sha256=%s delivery=%s; APPDATA isolated; no src/sidecar in installation", cfg.CenterSHA256, cfg.DeliverySHA256)
	ctx := context.Background()

	t.Run("occupied_port_never_requested", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:18120")
		if err != nil {
			t.Skipf("18120 acquired externally; leaving occupant untouched: %v", err)
		}
		var requests atomic.Int32
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(500) })}
		done := make(chan struct{})
		go func() { defer close(done); _ = server.Serve(listener) }()
		defer func() { _ = server.Close(); <-done }()
		_, err = client.EnsureRunning(ctx)
		if err == nil || !strings.Contains(err.Error(), "center_port_conflict") {
			t.Fatalf("want port conflict: %v", err)
		}
		if requests.Load() != 0 {
			t.Fatalf("requested occupant %d times", requests.Load())
		}
		if _, err := os.Stat(filepath.Join(data, integration.DescriptorName)); !os.IsNotExist(err) {
			t.Fatalf("unexpected descriptor: %v", err)
		}
		t.Log("real listener occupied 18120: conflict, zero HTTP requests, no launch")
	})

	// Observe direct children by OS parent PID and the unique temporary image.
	// Retain kernel handles so cleanup cannot target a recycled PID.
	watcher := e2eWatch(t, exe)
	defer watcher.close(t)
	t.Run("failed_child_cleanup", func(t *testing.T) {
		lock, err := safefs.Lock(filepath.Join(registryDir, "registry.lock"))
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		before := watcher.count()
		_, err = client.EnsureRunning(ctx)
		if err == nil || !strings.Contains(err.Error(), "center_start_failed") {
			t.Fatalf("expected registry-locked startup failure: %v", err)
		}
		watcher.scan()
		if watcher.count() == before {
			t.Fatal("failed child was not observed; cannot claim process cleanup evidence")
		}
		watcher.assertExited(t)
		t.Log("real child failed against held registry lock; every observed owned child exited")
	})
	if t.Failed() {
		return
	}
	// A second external port owner must not be touched, even if it raced build.
	probe, err = net.Listen("tcp", "127.0.0.1:18120")
	if err != nil {
		t.Skipf("18120 now occupied externally; no request/kill: %v", err)
	}
	probe.Close()
	before := watcher.count()
	const starters = 4
	clients := []*integration.Client{client}
	for i := 1; i < starters; i++ {
		clients = append(clients, e2eClient(t, cfg))
	}
	type launchResult struct {
		status integration.Status
		err    error
	}
	ready := make(chan struct{}, starters)
	start := make(chan struct{})
	results := make(chan launchResult, starters)
	for _, c := range clients {
		go func(c *integration.Client) {
			ready <- struct{}{}
			<-start
			s, err := c.EnsureRunning(ctx)
			results <- launchResult{s, err}
		}(c)
	}
	for i := 0; i < starters; i++ {
		<-ready
	}
	close(start)
	var status integration.Status
	for i := 0; i < starters; i++ {
		result := <-results
		if result.err != nil {
			t.Errorf("concurrent first launch result %d: %v", i, result.err)
			continue
		}
		if i == 0 {
			status = result.status
		} else {
			// Each inspection has its own observation timestamp; all instance
			// identity, readiness and endpoint fields must otherwise agree.
			comparable := result.status
			comparable.ObservedAt = status.ObservedAt
			if !reflect.DeepEqual(comparable, status) {
				t.Errorf("concurrent clients returned different statuses: %+v / %+v", status, result.status)
			}
		}
	}
	if t.Failed() {
		return
	}
	watcher.scan()
	descriptorPath := filepath.Join(data, integration.DescriptorName)
	original, err := os.ReadFile(descriptorPath)
	if err != nil {
		t.Fatal(err)
	}
	var desc integration.Descriptor
	if err := json.Unmarshal(original, &desc); err != nil {
		t.Fatal(err)
	}
	if !watcher.owns(desc.PID, desc.ProcessStartedAt) || watcher.count() != before+1 {
		t.Fatal("descriptor does not bind exactly one newly created, OS-identified child")
	}
	if status.IdentityState != "confirmed" || status.HealthState != "ready" || !status.UIAssetsReady || status.URL != "http://127.0.0.1:18120/" {
		t.Fatalf("bad first status: %+v", status)
	}
	if status.InstanceID == "" || status.InstanceID != desc.InstanceID {
		t.Fatal("concurrent statuses do not bind the descriptor instance")
	}
	t.Logf("%d independent clients concurrently confirmed exactly one new center: pid=%d created=%s instance=%s URL=%s", starters, desc.PID, desc.ProcessStartedAt, status.InstanceID, status.URL)
	t.Run("same_client_reuse", func(t *testing.T) {
		next, err := client.EnsureRunning(ctx)
		if err != nil || next.InstanceID != status.InstanceID {
			t.Fatalf("reuse: %+v %v", next, err)
		}
		inspected, err := client.Inspect(ctx)
		if err != nil || inspected.InstanceID != status.InstanceID {
			t.Fatalf("cached inspect: %+v %v", inspected, err)
		}
		watcher.scan()
		if watcher.count() != before+1 {
			t.Fatal("reuse spawned a child")
		}
	})
	t.Run("automatic_inspect_no_initial_hash_and_explicit_other_client", func(t *testing.T) {
		other := e2eClient(t, cfg)
		unconfirmed, err := other.Inspect(ctx)
		if !errors.Is(err, integration.ErrUnconfirmed) || unconfirmed.URL != "" {
			t.Fatalf("automatic inspect trusted first contact: %+v %v", unconfirmed, err)
		}
		if _, err := other.Dependencies(ctx); !errors.Is(err, integration.ErrUnconfirmed) {
			t.Fatalf("deps implicitly confirmed: %v", err)
		}
		wrong := cfg
		wrong.CenterSHA256 = strings.Repeat("0", 64)
		bad := e2eClient(t, wrong)
		if _, err := bad.Inspect(ctx); !errors.Is(err, integration.ErrUnconfirmed) {
			t.Fatalf("Inspect reached initial hash instead of unconfirmed gate: %v", err)
		}
		if _, err := bad.EnsureRunning(ctx); err == nil || !strings.Contains(err.Error(), "delivery_center_mismatch") {
			t.Fatalf("explicit verification did not detect deliberately wrong trusted digest: %v", err)
		}
		confirmed, err := other.EnsureRunning(ctx)
		if err != nil || confirmed.InstanceID != status.InstanceID {
			t.Fatalf("second client explicit confirmation: %+v %v", confirmed, err)
		}
		watcher.scan()
		if watcher.count() != before+1 {
			t.Fatal("another client spawned or replaced child")
		}
		t.Log("fresh Inspect/Dependencies unconfirmed; wrong digest ignored by automatic Inspect but rejected by explicit open; second client explicitly reused instance")
	})
	t.Run("dependencies_read_only_no_scan_or_inference", func(t *testing.T) {
		diskBefore := e2eTree(t, root, registryDir)
		a, err := client.Dependencies(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, err := client.Dependencies(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if a.InstanceID != status.InstanceID || a.RegistryRevision != 7 || b.RegistryRevision != 7 || a.InventoryID != "" || b.InventoryID != "" || a.Complete || !a.Stale || len(a.Components) == 0 || len(a.Models) == 0 {
			t.Fatalf("unexpected snapshot: %+v / %+v", a, b)
		}
		for _, evidence := range append(a.Components, a.Models...) {
			if evidence.Inference != "not_tested" {
				t.Fatalf("inference claimed: %+v", evidence)
			}
		}
		if !reflect.DeepEqual(diskBefore, e2eTree(t, root, registryDir)) {
			t.Fatal("Dependencies changed installation or registry files")
		}
		t.Logf("two authenticated snapshots: revision=%d inventory empty/stale; files unchanged; all evidence inference=not_tested", a.RegistryRevision)
	})
	t.Run("untrusted_descriptor_rejected_without_killing_reused_instance", func(t *testing.T) {
		defer e2eWrite(t, descriptorPath, original)
		altered := desc
		altered.ProcessStartedAt = "0000000000000000"
		body, _ := json.Marshal(altered)
		e2eWrite(t, descriptorPath, body)
		if _, err := client.EnsureRunning(ctx); err == nil || !strings.Contains(err.Error(), "pipe_pid") {
			t.Fatalf("untrusted descriptor accepted: %v", err)
		}
		after, err := os.ReadFile(descriptorPath)
		if err != nil || string(after) != string(body) {
			t.Fatal("untrusted descriptor removed or overwritten")
		}
		if !watcher.owns(desc.PID, desc.ProcessStartedAt) {
			t.Fatal("reused instance was killed")
		}
		watcher.scan()
		if watcher.count() != before+1 {
			t.Fatal("untrusted descriptor bypass spawned child")
		}
	})
	final, err := client.Inspect(ctx)
	if err != nil || final.InstanceID != status.InstanceID {
		t.Fatalf("instance not intact after descriptor rejection: %+v %v", final, err)
	}
}

func e2eDigest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func e2eWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func e2eClient(t *testing.T, cfg integration.Config) *integration.Client {
	t.Helper()
	c, err := integration.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func e2eTree(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			// Registry's lifetime lock is opened exclusively on Windows; it is
			// synchronization state, not mutable registration/model content.
			if d.Name() == "registry.lock" {
				st, err := d.Info()
				if err != nil {
					return err
				}
				result[path] = fmt.Sprintf("lock/%d/%d", st.Size(), st.ModTime().UnixNano())
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			st, err := d.Info()
			if err != nil {
				return err
			}
			result[path] = fmt.Sprintf("%s/%d/%d", e2eDigest(b), st.Size(), st.ModTime().UnixNano())
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}

// The watcher only retains direct children of this test with the exact unique
// built image. Handles, creation times and image paths are rechecked before kill.
type e2eProcess struct {
	pid            uint32
	handle         syscall.Handle
	created, image string
}
type e2eWatcher struct {
	exe      string
	commands chan func(map[uint32]e2eProcess)
	done     chan struct{}
}

func e2eWatch(t *testing.T, exe string) *e2eWatcher {
	t.Helper()
	w := &e2eWatcher{exe: exe, commands: make(chan func(map[uint32]e2eProcess)), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		processes := map[uint32]e2eProcess{}
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case fn, ok := <-w.commands:
				if !ok {
					return
				}
				fn(processes)
			case <-tick.C:
				w.collect(processes)
			}
		}
	}()
	return w
}
func e2eIdentity(h syscall.Handle) (string, string, error) {
	var created, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		return "", "", err
	}
	buf := make([]uint16, 32768)
	n := uint32(len(buf))
	r, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW").Call(uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return "", "", err
	}
	return fmt.Sprintf("%08x%08x", created.HighDateTime, created.LowDateTime), syscall.UTF16ToString(buf[:n]), nil
}
func (w *e2eWatcher) collect(found map[uint32]e2eProcess) {
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer syscall.CloseHandle(snapshot)
	entry := syscall.ProcessEntry32{Size: uint32(unsafe.Sizeof(syscall.ProcessEntry32{}))}
	for err = syscall.Process32First(snapshot, &entry); err == nil; err = syscall.Process32Next(snapshot, &entry) {
		if entry.ParentProcessID != uint32(os.Getpid()) {
			continue
		}
		if _, ok := found[entry.ProcessID]; ok {
			continue
		}
		h, err := syscall.OpenProcess(0x1000|0x100000|1, false, entry.ProcessID)
		if err != nil {
			continue
		}
		created, image, err := e2eIdentity(h)
		if err != nil || !strings.EqualFold(filepath.Clean(image), filepath.Clean(w.exe)) {
			syscall.CloseHandle(h)
			continue
		}
		found[entry.ProcessID] = e2eProcess{entry.ProcessID, h, created, image}
	}
}
func (w *e2eWatcher) sync(fn func(map[uint32]e2eProcess)) {
	done := make(chan struct{})
	w.commands <- func(p map[uint32]e2eProcess) { fn(p); close(done) }
	<-done
}
func (w *e2eWatcher) scan() { w.sync(w.collect) }
func (w *e2eWatcher) count() int {
	n := 0
	w.sync(func(p map[uint32]e2eProcess) { n = len(p) })
	return n
}
func (w *e2eWatcher) owns(pid uint32, created string) bool {
	owned := false
	w.sync(func(p map[uint32]e2eProcess) {
		v, ok := p[pid]
		if ok {
			state, _ := syscall.WaitForSingleObject(v.handle, 0)
			c, image, err := e2eIdentity(v.handle)
			owned = state == syscall.WAIT_TIMEOUT && err == nil && c == created && c == v.created && image == v.image
		}
	})
	return owned
}
func (w *e2eWatcher) assertExited(t *testing.T) {
	t.Helper()
	var issues []string
	w.sync(func(p map[uint32]e2eProcess) {
		for _, v := range p {
			state, err := syscall.WaitForSingleObject(v.handle, 3000)
			if err != nil || state != syscall.WAIT_OBJECT_0 {
				issues = append(issues, fmt.Sprintf("pid=%d state=%d err=%v", v.pid, state, err))
			}
		}
	})
	if len(issues) > 0 {
		t.Fatal(issues)
	}
}
func (w *e2eWatcher) close(t *testing.T) {
	t.Helper()
	var issues []string
	var killed []uint32
	w.sync(func(p map[uint32]e2eProcess) {
		w.collect(p)
		for _, v := range p {
			state, err := syscall.WaitForSingleObject(v.handle, 0)
			if err == nil && state == syscall.WAIT_TIMEOUT {
				created, image, err := e2eIdentity(v.handle)
				if err != nil || created != v.created || image != v.image || !strings.EqualFold(filepath.Clean(image), filepath.Clean(w.exe)) {
					issues = append(issues, fmt.Sprintf("refused cleanup identity mismatch pid=%d", v.pid))
				} else if err := syscall.TerminateProcess(v.handle, 0); err != nil {
					issues = append(issues, err.Error())
				} else {
					killed = append(killed, v.pid)
					state, err = syscall.WaitForSingleObject(v.handle, 5000)
					if err != nil || state != syscall.WAIT_OBJECT_0 {
						issues = append(issues, fmt.Sprintf("cleanup wait pid=%d: %v/%d", v.pid, err, state))
					}
				}
			} else if err != nil {
				issues = append(issues, err.Error())
			}
			syscall.CloseHandle(v.handle)
		}
	})
	close(w.commands)
	<-w.done
	if len(issues) > 0 {
		t.Error(issues)
	}
	t.Logf("cleanup: terminated only test-created identity-checked PIDs %v; retained process handles prevent PID reuse", killed)
}
