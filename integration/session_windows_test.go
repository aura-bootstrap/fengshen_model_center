//go:build windows

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Run the existing Go test binary under the expected image path. This fixture
// builds no executable and starts neither the product nor its GUI/services.
func TestWindowsSessionHelper(t *testing.T) {
	root := os.Getenv("FENGSHEN_SESSION_FIXTURE_ROOT")
	if root == "" {
		return
	}
	cfg := Config{InstallRoot: root, Root: root, RootKind: "installed", DataDir: os.Getenv("FENGSHEN_SESSION_FIXTURE_DATA")}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHost(HostConfig{Config: cfg, Address: listener.Addr().String(), Build: "session-test", Dashboard: func() (Dashboard, error) {
		return Dashboard{State: json.RawMessage(`{"ready":true}`), Inventory: json.RawMessage(`null`), LastCompleteInventory: json.RawMessage(`null`), Components: json.RawMessage(`[]`)}, nil
	}, Report: func() (Report, error) { return reportFixture(), nil }, ModelFiles: func(_, _, _ string) (FilesPage, error) { return FilesPage{Files: json.RawMessage(`[]`)}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if engine := os.Getenv("FENGSHEN_SESSION_ENGINE"); engine != "" {
		h.cfg.EngineAllowlist = []TrustedEngine{{Path: engine, SHA256: os.Getenv("FENGSHEN_SESSION_ENGINE_SHA256"), Edition: "admin", Management: true}}
	}
	if err = h.Register(); err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.ServeIPC(ctx) }()
	var requests atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/fixture-counts" {
			h.mu.Lock()
			tokens := len(h.tokens)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode([]int{tokens, int(requests.Load())})
			return
		}
		mode, _ := os.ReadFile(filepath.Join(root, "response-mode"))
		if string(mode) == "401" {
			http.Error(w, "denied", 401)
			return
		}
		if r.URL.Path == ReportPath && string(mode) != "" {
			v := reportFixture()
			v.Build, v.InstanceID = h.cfg.Build, h.desc.InstanceID
			switch string(mode) {
			case "report-build":
				v.Build = "other-build"
			case "report-instance":
				v.InstanceID = "other-instance"
			case "report-schema":
				v.Schema = 2
			case "report-summary":
				v.Summary.Present++
			case "report-oversize":
				v.State = json.RawMessage(`{"large":"` + strings.Repeat("x", MaxDashboardResponse) + `"}`)
			}
			_ = json.NewEncoder(w).Encode(v)
			return
		}
		if string(mode) == "build" {
			_ = json.NewEncoder(w).Encode(Dashboard{Schema: 1, Build: "other-build", InstanceID: h.desc.InstanceID})
			return
		}
		if r.URL.Path == RuntimeSessionPath || r.URL.Path == SessionHeartbeatPath {
			h.ServeSessionHTTP(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})}
	go server.Serve(listener)
	defer server.Close()
	if err = json.NewEncoder(os.Stdout).Encode(true); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	cancel()
	<-done
}

func sessionFixture(t *testing.T) *Client {
	t.Helper()
	c, d, _ := deliveryFixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	writeDeliveryFile(t, c.exe(), raw)
	c.cfg.CenterSHA256 = deliveryDigest(raw)
	d.Files[0].Bytes, d.Files[0].SHA256 = strconv.Itoa(len(raw)), c.cfg.CenterSHA256
	saveDelivery(t, c, d)
	c.cfg.Root, c.cfg.RootKind, c.cfg.DataDir = c.cfg.InstallRoot, "installed", filepath.Join(t.TempDir(), "private")
	cmd := exec.Command(c.exe(), "-test.run=^TestWindowsSessionHelper$", "-test.timeout=90s")
	cmd.Env = append(os.Environ(), "FENGSHEN_SESSION_FIXTURE_ROOT="+c.cfg.Root, "FENGSHEN_SESSION_FIXTURE_DATA="+c.cfg.DataDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("fixture exit: %v", err)
		}
	})
	var ready bool
	if err = json.NewDecoder(stdout).Decode(&ready); err != nil || !ready {
		t.Fatalf("fixture ready: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.mu.Lock()
	_, err = c.inspect(ctx, true)
	c.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sessionCounts(t *testing.T, c *Client) []int {
	t.Helper()
	addr := c.cache.Session.Response.Descriptor.LoopbackAddress
	client := newHTTPClient(time.Second)
	defer client.CloseIdleConnections()
	response, err := client.Get("http://" + addr + "/fixture-counts")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var counts []int
	if err = json.NewDecoder(response.Body).Decode(&counts); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestWindowsSessionReuseAndExpiry(t *testing.T) {
	c := sessionFixture(t)
	first := c.cache.Session
	for i := 0; i < 40; i++ {
		if _, err := c.Dashboard(context.Background()); err != nil {
			t.Fatalf("dashboard %d: %v", i, err)
		}
		if _, err := c.ModelFiles(context.Background(), strings.Repeat("a", 32), "model", ""); err != nil {
			t.Fatalf("files %d: %v", i, err)
		}
		if report, err := c.Report(context.Background()); err != nil || report.InstanceID != first.Response.Descriptor.InstanceID || report.Build != first.Response.Build {
			t.Fatalf("report %d: %v", i, err)
		}
		if c.cache.Session != first {
			t.Fatal("session changed during reuse")
		}
	}
	if n := sessionCounts(t, c)[0]; n != 1 {
		t.Fatalf("120 read calls issued %d tokens", n)
	}
	c.cache.Session.Started = time.Now().Add(-readSessionLifetime)
	if _, err := c.Dashboard(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.cache.Session.Response.Token == first.Response.Token {
		t.Fatal("expired session reused")
	}
	if n := sessionCounts(t, c)[0]; n != 2 {
		t.Fatalf("expiry tokens: %d", n)
	}
}

func TestWindowsSessionReadOnlyAndHTTPFailures(t *testing.T) {
	for _, route := range []string{"inspect", "dependencies", "files", "report"} {
		t.Run(route, func(t *testing.T) {
			c := sessionFixture(t)
			h := c.cache.Session.Response
			before := sessionCounts(t, c)
			// Possessing the reused capability cannot authorize a write, and no
			// client retry layer is installed on the HTTP transport.
			client := newHTTPClient(time.Second)
			defer client.CloseIdleConnections()
			req, err := http.NewRequest("POST", "http://"+h.Descriptor.LoopbackAddress+DashboardPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(ReadOnlyHeader, h.Token)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("write authorized: %d", resp.StatusCode)
			}
			writeDeliveryFile(t, filepath.Join(c.cfg.Root, "response-mode"), []byte("401"))
			switch route {
			case "inspect":
				_, err = c.Inspect(context.Background())
			case "dependencies":
				_, err = c.Dependencies(context.Background())
			case "files":
				_, err = c.ModelFiles(context.Background(), strings.Repeat("a", 32), "model", "")
			case "report":
				_, err = c.Report(context.Background())
			}
			if err == nil || !strings.Contains(err.Error(), "401") {
				t.Fatalf("HTTP failure: %v", err)
			}
			if c.cache.Expected != "" || c.cache.Session.Response.Token != "" {
				t.Fatal("HTTP failure retained session")
			}
			if _, err = c.Dashboard(context.Background()); !errors.Is(err, ErrUnconfirmed) {
				t.Fatalf("retry: %v", err)
			}
			c.cache.Session.Response = h
			after := sessionCounts(t, c)
			if after[0] != 1 || after[1] != before[1]+3 {
				t.Fatalf("write/401 retried: before=%v after=%v", before, after)
			}
			c.cache = cacheEntry{}
		})
	}
}

func TestWindowsReportFailuresInvalidateSession(t *testing.T) {
	for _, mode := range []string{"report-build", "report-instance", "report-schema", "report-summary", "report-oversize"} {
		t.Run(mode, func(t *testing.T) {
			c := sessionFixture(t)
			h := c.cache.Session.Response
			before := sessionCounts(t, c)
			writeDeliveryFile(t, filepath.Join(c.cfg.Root, "response-mode"), []byte(mode))
			v, err := c.Report(context.Background())
			if err == nil || v.InstanceID != "" || c.cache.Expected != "" || c.cache.Session.Response.Token != "" {
				t.Fatal("invalid report retained data or identity", err)
			}
			if (mode == "report-build" || mode == "report-instance") && !strings.Contains(err.Error(), "center_identity_mismatch") {
				t.Fatal(err)
			}
			writeDeliveryFile(t, filepath.Join(c.cfg.Root, "response-mode"), nil)
			if _, err = c.Report(context.Background()); !errors.Is(err, ErrUnconfirmed) {
				t.Fatal("passive report revived identity", err)
			}
			c.cache.Session.Response = h
			after := sessionCounts(t, c)
			if after[0] != 1 || after[1] != before[1]+2 {
				t.Fatalf("unexpected report retries: before=%v after=%v", before, after)
			}
			c.cache = cacheEntry{}
		})
	}
}

func TestWindowsSessionFailClosed(t *testing.T) {
	for _, mode := range []string{"descriptor", "root", "data", "pid", "creation", "image", "process-file", "process-size", "process-modified", "expected-build", "delivery-digest", "delivery", "layout", "component", "401", "build"} {
		t.Run(mode, func(t *testing.T) {
			c := sessionFixture(t)
			h := c.cache.Session.Response
			counts := sessionCounts(t, c)
			switch mode {
			case "descriptor", "root", "data", "pid", "creation":
				d := h.Descriptor
				switch mode {
				case "descriptor":
					d.InstanceID = strings.Repeat("b", 64)
				case "root":
					d.RootIdentity = "changed"
				case "data":
					d.DataIdentity = "changed"
				case "pid":
					d.PID++
				case "creation":
					d.ProcessStartedAt = "changed"
				}
				b, _ := json.Marshal(d)
				writeDeliveryFile(t, filepath.Join(c.cfg.DataDir, DescriptorName), b)
			case "image":
				c.cache.Identity.Image = "other.exe"
			case "process-file":
				c.cache.Identity.FileID = "changed"
			case "process-size":
				c.cache.Identity.Size++
			case "process-modified":
				c.cache.Identity.Modified++
			case "expected-build":
				c.cfg.CenterSHA256 = strings.Repeat("c", 64)
			case "delivery-digest":
				c.cfg.DeliverySHA256 = strings.Repeat("c", 64)
			case "delivery":
				writeDeliveryFile(t, c.delivery(), []byte("changed"))
			case "layout":
				writeDeliveryFile(t, filepath.Join(c.cfg.Root, "resource-layout.json"), []byte("changed"))
			case "component":
				writeDeliveryFile(t, filepath.Join(c.cfg.Root, "components/test/manifest.json"), []byte("changed"))
			case "401", "build":
				writeDeliveryFile(t, filepath.Join(c.cfg.Root, "response-mode"), []byte(mode))
			}
			_, err := c.Dashboard(context.Background())
			if err == nil {
				t.Fatal("accepted changed binding/failure")
			}
			if mode == "401" && !strings.Contains(err.Error(), "401") {
				t.Fatal(err)
			}
			if c.cache.Expected != "" || c.cache.Session.Response.Token != "" {
				t.Fatal("failed read retained identity/session")
			}
			// Even restoration must not revive the old token or retry automatically.
			writeDeliveryFile(t, filepath.Join(c.cfg.Root, "response-mode"), nil)
			b, _ := json.Marshal(h.Descriptor)
			writeDeliveryFile(t, filepath.Join(c.cfg.DataDir, DescriptorName), b)
			_, err = c.Dashboard(context.Background())
			if !errors.Is(err, ErrUnconfirmed) {
				t.Fatalf("passive retry: %v", err)
			}
			// Inspect fixture counters without using connect or issuing another token.
			c.cache.Session.Response = h
			after := sessionCounts(t, c)
			wantHTTP := counts[1] + 1 // counter request itself
			if mode == "401" || mode == "build" {
				wantHTTP++
			}
			if after[0] != 1 || after[1] != wantHTTP {
				t.Fatalf("unexpected token/request retries: before=%v after=%v", counts, after)
			}
			c.cache = cacheEntry{}
		})
	}
}
