//go:build windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
)

// Separate opt-in: do not run alongside any other fixed-port center E2E.
// The fixture has no networking, descriptor, model or inference implementation.
func TestCenterStartupTimeoutWindows(t *testing.T) {
	if os.Getenv("FENGSHEN_CENTER_STARTUP_TIMEOUT_E2E") != "1" {
		t.Skip("explicit opt-in required: FENGSHEN_CENTER_STARTUP_TIMEOUT_E2E=1")
	}
	startupTimeoutFreePort(t)
	base, err := os.MkdirTemp("", "fengshen-startup-timeout-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Clean(filepath.Join(cwd, "../.."))
	if strings.EqualFold(filepath.VolumeName(repo), filepath.VolumeName(base)) {
		rel, err := filepath.Rel(repo, base)
		if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
			t.Fatalf("fixture must be outside repository: %q (relative %q, %v)", base, rel, err)
		}
	}
	root := filepath.Join(base, "installation")
	centerDir := filepath.Join(root, "model-center")
	if err := os.MkdirAll(centerDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Build a standalone source file, not the production center. Ignore launch
	// flags except the private data directory; publish only our unique marker.
	source := filepath.Join(base, "lazy.go")
	e2eWrite(t, source, []byte(`package main
import ("os"; "path/filepath"; "strconv"; "time")
func main() {
    data := ""
    for i := 1; i+1 < len(os.Args); i++ {
        if os.Args[i] == "-integration-data" { data = os.Args[i+1]; break }
    }
    if data == "" { os.Exit(2) }
    if err := os.WriteFile(filepath.Join(data, "lazy-started.fixture"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil { os.Exit(3) }
    for { time.Sleep(time.Hour) }
}
`))
	exe := filepath.Join(centerDir, "fengshen-model-center.exe")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-trimpath", "-o", exe, source)
	build.Dir = base
	// PATH/GOROOT/GOTOOLCHAIN select the same offline toolchain as the test
	// invocation. Never permit automatic toolchain or module downloads.
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOFLAGS=")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build offline lazy fixture: %v\n%s", err, output)
	}
	e2eWrite(t, filepath.Join(root, "resource-layout.json"), []byte(`{"schema":1,"center":"model-center/fengshen-model-center.exe","components":"model-center/components","root_kind":"installed","model_roots":[],"haar_files":[]}`))
	type file struct {
		Path   string `json:"path"`
		Bytes  string `json:"bytes"`
		SHA256 string `json:"sha256"`
	}
	var files []file
	for _, name := range []string{"model-center/fengshen-model-center.exe", "resource-layout.json"} {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file{name, fmt.Sprint(len(b)), e2eDigest(b)})
	}
	manifest, err := json.Marshal(map[string]any{"schema": 1, "files": files, "inference": "not_tested", "clean_environment": "not_verified"})
	if err != nil {
		t.Fatal(err)
	}
	e2eWrite(t, filepath.Join(centerDir, "delivery.json"), manifest)
	t.Logf("offline fixture built before watcher: %s sha256=%s delivery=%s", exe, files[0].SHA256, e2eDigest(manifest))

	for _, parentCancel := range []bool{false, true} {
		name := "default_15s_deadline"
		if parentCancel {
			name = "parent_cancel_after_live_marker"
		}
		t.Run(name, func(t *testing.T) {
			startupTimeoutFreePort(t)
			data, err := os.MkdirTemp(base, "data-")
			if err != nil {
				t.Fatal(err)
			}
			client := e2eClient(t, integration.Config{InstallRoot: root, Root: root, RootKind: "installed", DataDir: data, CenterSHA256: files[0].SHA256, DeliverySHA256: e2eDigest(manifest)})
			// Exact final EXE path, only after compilation. Retained kernel handles
			// bind the child independently of its marker and prevent PID recycling.
			watcher := e2eWatch(t, exe)
			defer watcher.close(t)
			ctx, cancel := context.WithCancel(context.Background())
			type result struct {
				err     error
				elapsed time.Duration
			}
			done := make(chan result, 1)
			started := time.Now()
			go func() {
				_, err := client.EnsureRunning(ctx)
				done <- result{err, time.Since(started)}
			}()
			received := false
			// On any assertion failure cancel and join EnsureRunning before the
			// watcher performs identity-checked fallback cleanup of our child.
			defer func() {
				cancel()
				if !received {
					<-done
				}
			}()
			poll := time.NewTicker(10 * time.Millisecond)
			defer poll.Stop()
			markerLimit := time.NewTimer(8 * time.Second)
			defer markerLimit.Stop()
			var child e2eProcess
			for child.handle == 0 {
				select {
				case r := <-done:
					received = true
					if r.err != nil && strings.HasPrefix(r.err.Error(), "center_port_conflict:") {
						t.Skipf("18120 acquired externally; no request/termination of occupant: %v", r.err)
					}
					t.Fatalf("returned before live fixture evidence: %v (%s)", r.err, r.elapsed)
				case <-markerLimit.C:
					t.Fatal("fixture did not publish a live, OS-bound startup marker within 8s")
				case <-poll.C:
					b, err := os.ReadFile(filepath.Join(data, "lazy-started.fixture"))
					if os.IsNotExist(err) {
						continue
					}
					if err != nil {
						t.Fatal(err)
					}
					pid, err := strconv.ParseUint(string(b), 10, 32)
					if err != nil {
						continue // WriteFile may still be writing the marker.
					}
					watcher.scan()
					watcher.sync(func(p map[uint32]e2eProcess) { child = p[uint32(pid)] })
				}
			}
			if watcher.count() != 1 || !watcher.owns(child.pid, child.created) {
				t.Fatal("marker did not bind exactly one live, owned OS process")
			}
			startupTimeoutNoDescriptor(t, data)
			t.Logf("startup wait reached: marker PID=%d created=%s live at %s", child.pid, child.created, time.Since(started))
			var canceledAt time.Time
			if parentCancel {
				// A timer after positive startup evidence, never a sleep guessing
				// whether Start/queryProcess/the waiting loop have been reached.
				hold := time.NewTimer(250 * time.Millisecond)
				defer hold.Stop()
				select {
				case r := <-done:
					received = true
					t.Fatalf("returned before parent cancel: %v", r.err)
				case <-hold.C:
				}
				if !watcher.owns(child.pid, child.created) {
					t.Fatal("owned child exited before parent cancel")
				}
				canceledAt = time.Now()
				cancel()
			}
			limit := time.NewTimer(time.Until(started.Add(22 * time.Second)))
			defer limit.Stop()
			var r result
			select {
			case r = <-done:
				received = true
			case <-limit.C:
				t.Fatal("EnsureRunning exceeded 22s safety bound")
			}
			if r.err == nil || !strings.HasPrefix(r.err.Error(), "center_start_timeout:") {
				t.Fatalf("want startup wait timeout, not startupLock/early failure: %v", r.err)
			}
			if parentCancel {
				if ctx.Err() != context.Canceled || r.elapsed >= 10*time.Second || time.Since(canceledAt) > 2*time.Second {
					t.Fatalf("parent cancellation not prompt: elapsed=%s response=%s ctx=%v", r.elapsed, time.Since(canceledAt), ctx.Err())
				}
			} else if ctx.Err() != nil || r.elapsed < 14500*time.Millisecond || r.elapsed > 22*time.Second {
				t.Fatalf("not the default 15s deadline: elapsed=%s parent=%v", r.elapsed, ctx.Err())
			}
			// Assert before fallback cleanup: production must have killed its child.
			watcher.assertExited(t)
			startupTimeoutNoDescriptor(t, data)
			t.Logf("%v after %s; OS-bound PID=%d exited before test cleanup; no descriptor", r.err, r.elapsed, child.pid)
		})
	}
}

func startupTimeoutFreePort(t *testing.T) {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:18120")
	if err != nil {
		t.Skipf("18120 occupied; no request or termination attempted: %v", err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
}

func startupTimeoutNoDescriptor(t *testing.T, data string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(data, integration.DescriptorName)); !os.IsNotExist(err) {
		t.Fatalf("descriptor must never be published: %v", err)
	}
}
