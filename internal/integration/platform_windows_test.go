//go:build windows

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

// Done is consulted by startupLock's contention select, after a failed real
// lock attempt. Signal that point so cancellation never depends on a sleep.
type startupLockWaitContext struct {
	context.Context
	waiting chan struct{}
}

func (c startupLockWaitContext) Done() <-chan struct{} {
	select {
	case c.waiting <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func TestWindowsStartupLockCancellation(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "startup.lock")
			held, err := safefs.Lock(path)
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 250*time.Millisecond)
				want = context.DeadlineExceeded
			}
			defer cancel()
			waiting := make(chan struct{}, 1)
			done := make(chan error, 1)
			go func() {
				lock, err := startupLock(startupLockWaitContext{ctx, waiting}, path)
				if lock != nil {
					_ = lock.Close()
				}
				done <- err
			}()
			select {
			case <-waiting:
			case err := <-done:
				t.Fatalf("returned before observing lock contention: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("did not attempt held lock")
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("want %v, got %v", want, err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("startupLock ignored cancellation/deadline")
			}
			if other, err := safefs.Lock(path); err == nil {
				_ = other.Close()
				t.Fatal("cancelled waiter released the original owner's lock")
			}
			if err := held.Close(); err != nil {
				t.Fatal(err)
			}
			retryCtx, retryCancel := context.WithTimeout(context.Background(), time.Second)
			defer retryCancel()
			reacquired, err := startupLock(retryCtx, path)
			if err != nil {
				t.Fatalf("reacquire after owner release: %v", err)
			}
			if err := reacquired.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWindowsPipePeerAndFraming(t *testing.T) {
	dir := t.TempDir()
	name, e := pipeName(dir)
	if e != nil {
		t.Fatal(e)
	}
	p, e := listenPipe(name)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	if second, e := listenPipe(name); e == nil {
		second.Close()
		t.Fatal("second pipe instance accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- servePipe(ctx, p, func(rw io.ReadWriter) {
			var req handshakeRequest
			if readFrame(rw, &req) != nil {
				return
			}
			_ = writeFrame(rw, handshakeResponse{Schema: 1, Protocol: req.Protocol})
		})
	}()
	conn, pid, e := dialPipe(ctx, name)
	if e != nil {
		t.Fatal(e)
	}
	if pid != uint32(os.Getpid()) {
		t.Fatal(pid)
	}
	identity, e := queryProcess(pid)
	if e != nil || identity.FileID == "" || identity.Created == "" {
		t.Fatalf("identity: %+v %v", identity, e)
	}
	if e = writeFrame(conn, handshakeRequest{Schema: 1, Protocol: Protocol}); e != nil {
		t.Fatal(e)
	}
	var response handshakeResponse
	if e = readFrame(conn, &response); e != nil {
		t.Fatal(e)
	}
	conn.Close()
	if response.Protocol != Protocol {
		t.Fatal(response)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("pipe cancellation blocked")
	}
}

// Use an untimed client: dialPipe's watchdog must not rescue a stalled server.
func openStalledPipe(t *testing.T, name string) syscall.Handle {
	t.Helper()
	ptr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(ptr, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.CloseHandle(h) })
	return h
}

func TestWindowsPipeStalledPeerRecovery(t *testing.T) {
	for _, mode := range []string{"read", "flush", "write"} {
		t.Run(mode, func(t *testing.T) {
			name, err := pipeName(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			listener, err := listenPipe(name)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, returned := make(chan struct{}), make(chan error, 1)
			done := make(chan error, 1)
			go func() {
				first := true
				done <- servePipe(ctx, listener, func(rw io.ReadWriter) {
					if first {
						first = false
						close(entered)
						if mode == "read" {
							var req handshakeRequest
							returned <- readFrame(rw, &req)
							return
						}
						token := "unread"
						if mode == "write" {
							_, err := rw.Write([]byte(strings.Repeat("x", 128*1024)))
							returned <- err
							return
						}
						returned <- writeFrame(rw, handshakeResponse{Schema: 1, Protocol: Protocol, Token: token})
						return
					}
					var req handshakeRequest
					if readFrame(rw, &req) == nil && req.Schema == 1 && req.Protocol == Protocol {
						_ = writeFrame(rw, handshakeResponse{Schema: 1, Protocol: Protocol, Token: "second-handshake"})
					}
				})
			}()
			started := time.Now()
			openStalledPipe(t, name)
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("first peer not accepted")
			}
			if mode == "flush" {
				select {
				case err := <-returned:
					if err != nil {
						t.Fatal("small response did not reach pipe buffer:", err)
					}
				case <-time.After(time.Second):
					t.Fatal("small response write blocked instead of flush")
				}
			}
			// Keep the first client handle open and do not read any response.
			retryCtx, retryCancel := context.WithTimeout(ctx, 5*time.Second)
			defer retryCancel()
			conn, pid, err := dialPipe(retryCtx, name)
			if err != nil {
				t.Fatal("listener lost after stalled peer:", err)
			}
			defer conn.Close()
			if elapsed := time.Since(started); elapsed < 1800*time.Millisecond {
				t.Fatalf("stalled peer released before watchdog: %v", elapsed)
			}
			if mode != "flush" {
				if err := <-returned; err == nil {
					t.Fatal("stalled I/O completed without cancellation error")
				}
			}
			if pid != uint32(os.Getpid()) {
				t.Fatal("server PID changed:", pid)
			}
			if err := writeFrame(conn, handshakeRequest{Schema: 1, Protocol: Protocol}); err != nil {
				t.Fatal("second request:", err)
			}
			var response handshakeResponse
			if err := readFrame(conn, &response); err != nil || response.Token != "second-handshake" {
				t.Fatalf("second handshake: %+v, %v", response, err)
			}
			if other, err := listenPipe(name); err == nil {
				other.Close()
				t.Fatal("recovery released first-instance protection")
			}
			conn.Close()
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("server did not stop")
			}
		})
	}
}

func TestWindowsHostStalledPeerRecovery(t *testing.T) {
	for _, mode := range []string{"read", "unread-response"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			h, err := NewHost(HostConfig{Config: Config{InstallRoot: root, Root: root, RootKind: "installed", DataDir: filepath.Join(t.TempDir(), "private")}, Address: "127.0.0.1:18120", Build: "pipe-recovery-test"})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.Register(); err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- h.ServeIPC(ctx) }()
			d := h.desc
			req := handshakeRequest{Schema: 1, Protocol: Protocol, InstanceID: d.InstanceID, RootKind: d.RootKind, RootIdentity: d.RootIdentity, DataIdentity: d.DataIdentity}
			stalled := openStalledPipe(t, d.PipeName)
			if mode == "unread-response" {
				// A complete valid request, but this client never consumes its token.
				raw, err := json.Marshal(req)
				if err != nil {
					t.Fatal(err)
				}
				frame := make([]byte, 4+len(raw))
				binary.LittleEndian.PutUint32(frame, uint32(len(raw)))
				copy(frame[4:], raw)
				var n uint32
				if err := syscall.WriteFile(stalled, frame, &n, nil); err != nil || int(n) != len(frame) {
					t.Fatalf("first request: %d, %v", n, err)
				}
			}
			retryCtx, retryCancel := context.WithTimeout(ctx, 5*time.Second)
			defer retryCancel()
			conn, pid, err := dialPipe(retryCtx, d.PipeName)
			if err != nil {
				t.Fatal("second host connection:", err)
			}
			defer conn.Close()
			if pid != d.PID {
				t.Fatal("server PID changed")
			}
			if err := writeFrame(conn, req); err != nil {
				t.Fatal(err)
			}
			var response handshakeResponse
			if err := readFrame(conn, &response); err != nil || response.Descriptor != d || response.Protocol != Protocol || response.Token == "" {
				t.Fatalf("second host handshake: %+v, %v", response, err)
			}
			httpReq := httptest.NewRequest("GET", "http://"+d.LoopbackAddress+HealthPath, nil)
			httpReq.Header.Set(ReadOnlyHeader, response.Token)
			if !h.AuthorizeReadOnly(httpReq) {
				t.Fatal("second handshake token rejected")
			}
			conn.Close()
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("host IPC did not stop")
			}
		})
	}
}

func requirePipeHandleClosed(t *testing.T, p *winPipe) {
	t.Helper()
	p.mu.Lock()
	closed, done := p.closed, p.closeDone
	p.mu.Unlock()
	if !closed || done == nil {
		t.Fatal("pipe shutdown not started")
	}
	select {
	case <-done:
		if p.closeErr != nil {
			t.Fatal("pipe handle close failed:", p.closeErr)
		}
	default:
		t.Fatal("pipe shutdown not completed")
	}
	if _, err := p.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatal("closed pipe accepted read:", err)
	}
	if _, err := p.Write([]byte{0}); !errors.Is(err, os.ErrClosed) {
		t.Fatal("closed pipe accepted write:", err)
	}
}

func TestWindowsPipeShutdown(t *testing.T) {
	for _, mode := range []string{"connect", "read", "write", "flush", "before-read", "before-flush"} {
		for _, action := range []string{"cancel", "close"} {
			t.Run(mode+"/"+action, func(t *testing.T) {
				name, err := pipeName(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				listener, err := listenPipe(name)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				p := listener.(*winPipe)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				entered, release := make(chan struct{}), make(chan struct{})
				defer func() {
					select {
					case <-release:
					default:
						close(release)
					}
				}()
				done := make(chan error, 1)
				go func() {
					done <- servePipe(ctx, p, func(rw io.ReadWriter) {
						if mode == "before-read" || mode == "before-flush" {
							close(entered)
							<-release
							if mode == "before-flush" {
								return
							}
						}
						if mode == "flush" {
							_ = writeFrame(rw, handshakeResponse{Schema: 1, Protocol: Protocol})
							close(entered)
							return
						}
						if mode != "before-read" {
							close(entered)
						}
						if mode == "write" {
							_, _ = rw.Write(make([]byte, 128*1024))
						} else {
							var b [1]byte
							_, _ = rw.Read(b[:])
						}
					})
				}()
				if mode != "connect" {
					openStalledPipe(t, name)
					select {
					case <-entered:
					case <-time.After(time.Second):
						t.Fatal("handler not entered")
					}
				}
				if mode != "before-read" && mode != "before-flush" {
					// Observe a submitted operation before shutdown. For flush the
					// active reservation also covers its pre-syscall window.
					deadline := time.NewTimer(time.Second)
					defer deadline.Stop()
					tick := time.NewTicker(time.Millisecond)
					defer tick.Stop()
					for {
						p.mu.Lock()
						h := p.h
						p.mu.Unlock()
						// A pending operation leaves the overlapped pipe handle unsignaled.
						status, err := syscall.WaitForSingleObject(h, 0)
						if err == nil && status == syscall.WAIT_TIMEOUT {
							break
						}
						select {
						case <-tick.C:
						case <-deadline.C:
							t.Fatal("pipe I/O was not pending")
						}
					}
				}
				if action == "cancel" {
					cancel()
				} else {
					closed := make(chan struct{})
					go func() {
						var group sync.WaitGroup
						for i := 0; i < 4; i++ {
							group.Add(1)
							go func() { defer group.Done(); p.Close() }()
						}
						group.Wait()
						close(closed)
					}()
					select {
					case <-closed:
					case <-time.After(time.Second):
						t.Fatal("concurrent Close blocked")
					}
				}
				close(release)
				select {
				case err := <-done:
					if action == "cancel" && !errors.Is(err, context.Canceled) {
						t.Fatalf("cancel result: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("shutdown relied on the two-second watchdog")
				}
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
				requirePipeHandleClosed(t, p)
				if _, err := p.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
					t.Fatal("closed pipe accepted I/O:", err)
				}
			})
		}
	}
}

func TestWindowsPipeClientCloseAndReuse(t *testing.T) {
	name, err := pipeName(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := listenPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- servePipe(ctx, listener, func(rw io.ReadWriter) {
			entered <- struct{}{}
			var req handshakeRequest
			if readFrame(rw, &req) == nil {
				_ = writeFrame(rw, handshakeResponse{Schema: 1, Protocol: Protocol})
			}
		})
	}()
	conn, _, err := dialPipe(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	client := conn.(*timedPipe)
	defer client.Close()
	<-entered
	readDone := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := client.Read(b[:])
		readDone <- err
	}()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("closed client read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("client Close did not release read")
	}
	// Numeric handles can immediately be reused by the server's next I/O event;
	// use process counts to detect leaks across many complete connections.
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		t.Fatal(err)
	}
	countHandles := func() uint32 {
		var n uint32
		r, _, err := kernel.NewProc("GetProcessHandleCount").Call(uintptr(process), uintptr(unsafe.Pointer(&n)))
		if r == 0 {
			t.Fatal(err)
		}
		return n
	}
	// Exercise event cleanup and stopped watchdogs across repeated reuse.
	round := func(offset, n int) {
		for i := 0; i < n; i++ {
			conn, _, err := dialPipe(ctx, name)
			if err != nil {
				t.Fatal(err)
			}
			<-entered
			if err := writeFrame(conn, handshakeRequest{Schema: 1, Protocol: Protocol}); err != nil {
				conn.Close()
				t.Fatal(err)
			}
			var response handshakeResponse
			err = readFrame(conn, &response)
			conn.Close()
			if err != nil || response.Protocol != Protocol {
				t.Fatalf("handshake %d: %+v, %v", offset+i, response, err)
			}
		}
	}
	// A process-wide handle count also grows from runtime threads and lazily
	// loaded modules on first use. Warm those up before measuring; a
	// per-connection pipe/event leak still retains dozens of handles below.
	round(0, 64)
	before := countHandles()
	round(64, 64)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reused listener did not stop")
	}
	requirePipeHandleClosed(t, listener.(*winPipe))
	if after := countHandles(); after > before+8 {
		t.Fatalf("pipe/event handles leaked across reuse: before=%d after=%d", before, after)
	}
}

func TestWindowsInspectCacheDoesNotReadBinary(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "model-center")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := Config{InstallRoot: root, CenterSHA256: strings.Repeat("a", 64), DeliverySHA256: strings.Repeat("b", 64)}
	c := &Client{cfg: cfg}
	// Deliberately no EXE exists. An automatic first verification must stop at
	// unconfirmed rather than opening/hash-reading it.
	if e := os.WriteFile(c.delivery(), []byte("manifest"), 0600); e != nil {
		t.Fatal(e)
	}
	p := processIdentity{PID: 123, Image: c.exe(), Created: "creation", FileID: "file", Size: 99, Modified: 1}
	if e := c.verify(context.Background(), p, false); e != ErrUnconfirmed {
		t.Fatalf("first inspect: %v", e)
	}
	id, size, mod, e := metadata(c.delivery())
	if e != nil {
		t.Fatal(e)
	}
	deliveryStamp, e := stamp(c.delivery())
	if e != nil {
		t.Fatal(e)
	}
	c.cache = cacheEntry{Identity: p, Expected: cfg.CenterSHA256, Delivery: cfg.DeliverySHA256, DeliveryID: id, DeliverySize: size, DeliveryModified: mod, Files: map[string]fileStamp{c.delivery(): deliveryStamp}}
	if e = c.verify(context.Background(), p, false); e != nil {
		t.Fatalf("cached inspect read binary: %v", e)
	}
	p.Created = "reused-pid"
	if e = c.verify(context.Background(), p, false); e != ErrUnconfirmed {
		t.Fatal("PID reuse did not invalidate", e)
	}
}

func TestWindowsHostLifecycle(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(t.TempDir(), "private")
	h, e := NewHost(HostConfig{Config: Config{InstallRoot: root, Root: root, RootKind: "installed", DataDir: data}, Address: "127.0.0.1:18120", Build: "test-build"})
	if e != nil {
		t.Fatal(e)
	}
	if e = h.Register(); e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	raw, e := os.ReadFile(filepath.Join(data, DescriptorName))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(raw), "token") {
		t.Fatal("token leaked")
	}
	var desc Descriptor
	if e = json.Unmarshal(raw, &desc); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.ServeIPC(ctx) }()
	conn, pid, e := dialPipe(ctx, desc.PipeName)
	if e != nil {
		t.Fatal(e)
	}
	if pid != desc.PID {
		t.Fatal("PID binding")
	}
	req := handshakeRequest{Schema: 1, Protocol: Protocol, InstanceID: desc.InstanceID, RootKind: desc.RootKind, RootIdentity: desc.RootIdentity, DataIdentity: desc.DataIdentity}
	if e = writeFrame(conn, req); e != nil {
		t.Fatal(e)
	}
	var response handshakeResponse
	if e = readFrame(conn, &response); e != nil {
		t.Fatal(e)
	}
	conn.Close()
	httpReq := httptest.NewRequest("GET", "http://127.0.0.1:18120"+HealthPath, nil)
	httpReq.Header.Set(ReadOnlyHeader, response.Token)
	if !h.AuthorizeReadOnly(httpReq) {
		t.Fatal("IPC token rejected")
	}
	if e = h.Close(); e != nil {
		t.Fatal(e)
	}
	if h.AuthorizeReadOnly(httpReq) {
		t.Fatal("closed host accepted token")
	}
	if _, e = os.Stat(filepath.Join(data, DescriptorName)); !os.IsNotExist(e) {
		t.Fatal("descriptor not removed", e)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("ServeIPC did not close")
	}
}

func TestWindowsHostClosePreservesForeignDescriptor(t *testing.T) {
	for _, mode := range []string{"foreign", "malformed", "missing"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			data := filepath.Join(t.TempDir(), "private")
			h, err := NewHost(HostConfig{Config: Config{InstallRoot: root, Root: root, RootKind: "installed", DataDir: data}, Address: "127.0.0.1:18120", Build: "test-build"})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.Register(); err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			path := filepath.Join(data, DescriptorName)
			var body []byte
			switch mode {
			case "foreign":
				desc := h.desc
				desc.InstanceID, err = randomID()
				if err != nil {
					t.Fatal(err)
				}
				if desc.InstanceID == h.desc.InstanceID {
					t.Fatal("fixture must identify a different instance")
				}
				body, err = json.Marshal(desc)
				if err != nil {
					t.Fatal(err)
				}
			case "malformed":
				body = []byte(`{"schema":1,"instance_id":`)
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if body != nil {
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if err := h.Close(); err != nil {
					t.Fatal(err)
				}
			}
			got, err := os.ReadFile(path)
			if mode == "missing" {
				if !os.IsNotExist(err) {
					t.Fatal("Close recreated a missing descriptor", err)
				}
			} else if err != nil || string(got) != string(body) {
				t.Fatalf("Close changed a descriptor it does not own: %q, %v", got, err)
			}
			lock, err := safefs.Lock(filepath.Join(data, "instance.lock"))
			if err != nil {
				t.Fatal("Close retained its instance lock:", err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWindowsHashExpectedAndReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "center.exe")
	if e := os.WriteFile(path, []byte("trusted"), 0600); e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256([]byte("trusted"))
	expected := hex.EncodeToString(sum[:])
	if e := hashFile(context.Background(), path, expected, 1024); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, []byte("changed"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := hashFile(context.Background(), path, expected, 1024); e == nil {
		t.Fatal("digest mismatch accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if hashFile(ctx, path, expected, 1024) == nil {
		t.Fatal("cancelled hash accepted")
	}
}
