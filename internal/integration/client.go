package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

type processIdentity struct {
	PID                    uint32
	Created, Image, FileID string
	SID                    string
	Size, Modified         int64
}

// Keep a safety margin below the host's 60-second token lifetime. The start
// time is captured before sending the handshake, never extended by reuse.
const readSessionLifetime = 45 * time.Second

type readSession struct {
	Response handshakeResponse
	Started  time.Time
}

type cacheEntry struct {
	Identity                       processIdentity
	Expected, Delivery, DeliveryID string
	DeliverySize, DeliveryModified int64
	Files                          map[string]fileStamp
	Session                        readSession
}
type Client struct {
	mu    sync.Mutex
	cfg   Config
	cache cacheEntry
}

func DefaultConfig() Config {
	exe, _ := os.Executable()
	base, _ := os.UserConfigDir()
	return Config{InstallRoot: filepath.Dir(exe), Root: filepath.Dir(exe), RootKind: "installed", DataDir: filepath.Join(base, "FengshenSlicer", "model-center-integration"), CenterSHA256: CenterSHA256, DeliverySHA256: DeliverySHA256}
}
func NewClient(cfg Config) (*Client, error) {
	if e := platformSupported(); e != nil {
		return nil, e
	}
	if cfg.CenterSHA256 == "" {
		cfg.CenterSHA256 = CenterSHA256
	}
	if cfg.DeliverySHA256 == "" {
		cfg.DeliverySHA256 = DeliverySHA256
	}
	if !digestValid(cfg.CenterSHA256) || !digestValid(cfg.DeliverySHA256) {
		return nil, errors.New("center_unbuilt: 缺少受信中心或交付摘要，禁止任意 go run")
	}
	if e := validateDirectories(cfg); e != nil {
		return nil, e
	}
	return &Client{cfg: cfg}, nil
}
func validateDirectories(c Config) error {
	if c.RootKind != "development" && c.RootKind != "installed" {
		return errors.New("center_root_kind_invalid")
	}
	for _, p := range []string{c.Root, c.InstallRoot, c.DataDir} {
		if !filepath.IsAbs(p) {
			return errors.New("center_path_not_absolute")
		}
		if e := safefs.ValidatePath(p); e != nil {
			return e
		}
	}
	// Integration descriptors and tokens must never share registry storage.
	if strings.EqualFold(filepath.Clean(c.Root), filepath.Clean(c.DataDir)) || strings.EqualFold(filepath.Clean(c.InstallRoot), filepath.Clean(c.DataDir)) {
		return errors.New("center_data_not_private")
	}
	if c.RootKind == "installed" && !strings.EqualFold(filepath.Clean(c.Root), filepath.Clean(c.InstallRoot)) {
		return errors.New("center_installed_root_invalid")
	}
	if c.RootKind == "development" {
		for _, p := range []string{"sidecar", "models"} {
			s, e := os.Stat(filepath.Join(c.Root, p))
			if e != nil || !s.IsDir() {
				return errors.New("center_development_root_invalid")
			}
		}
	}
	return nil
}
func directoryIdentities(c Config) (string, string, error) {
	if e := platformCheckPath(c.Root); e != nil {
		return "", "", e
	}
	if e := platformCheckPath(c.DataDir); e != nil {
		return "", "", e
	}
	a, e := os.Open(c.Root)
	if e != nil {
		return "", "", e
	}
	defer a.Close()
	ai, e := safefs.Identity(a)
	if e != nil {
		return "", "", e
	}
	b, e := os.Open(c.DataDir)
	if e != nil {
		return "", "", e
	}
	defer b.Close()
	bi, e := safefs.Identity(b)
	return ai, bi, e
}
func (c *Client) exe() string {
	return filepath.Join(c.cfg.InstallRoot, "model-center", "fengshen-model-center.exe")
}
func (c *Client) delivery() string {
	return filepath.Join(c.cfg.InstallRoot, "model-center", "delivery.json")
}
func (c *Client) cached(p processIdentity) bool {
	return c.cache.Expected != "" && c.cache.Expected == c.cfg.CenterSHA256 && c.cache.Delivery == c.cfg.DeliverySHA256 && c.cache.Identity == p
}
func metadata(path string) (string, int64, int64, error) {
	if e := platformCheckPath(path); e != nil {
		return "", 0, 0, e
	}
	if e := safefs.ValidatePath(path); e != nil {
		return "", 0, 0, e
	}
	f, e := os.Open(path)
	if e != nil {
		return "", 0, 0, e
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil || !s.Mode().IsRegular() {
		return "", 0, 0, errors.New("center_file_invalid")
	}
	id, e := safefs.Identity(f)
	return id, s.Size(), s.ModTime().UnixNano(), e
}
func hashFile(ctx context.Context, path, expected string, limit int64) error {
	if e := platformCheckPath(path); e != nil {
		return e
	}
	if e := safefs.ValidatePath(path); e != nil {
		return e
	}
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	before, e := f.Stat()
	if e != nil || !before.Mode().IsRegular() || before.Size() > limit {
		return errors.New("center_file_invalid")
	}
	id, e := safefs.Identity(f)
	if e != nil {
		return e
	}
	h := sha256.New()
	b := make([]byte, 1024*1024)
	var total int64
	for {
		if e = ctx.Err(); e != nil {
			return e
		}
		n, re := f.Read(b)
		total += int64(n)
		if total > limit {
			return errors.New("center_file_too_large")
		}
		if n > 0 {
			h.Write(b[:n])
		}
		if re == io.EOF {
			break
		}
		if re != nil {
			return re
		}
	}
	after, e := f.Stat()
	if e != nil {
		return e
	}
	aid, e := safefs.Identity(f)
	if e != nil {
		return e
	}
	pid, size, mod, e := metadata(path)
	if e != nil || id != aid || id != pid || before.Size() != after.Size() || before.ModTime() != after.ModTime() || size != before.Size() || mod != before.ModTime().UnixNano() || total != size {
		return errors.New("center_file_changed")
	}
	if hex.EncodeToString(h.Sum(nil)) != expected {
		return errors.New("center_hash_mismatch")
	}
	return nil
}
func (c *Client) verify(ctx context.Context, p processIdentity, explicit bool) error {
	if !strings.EqualFold(filepath.Clean(p.Image), filepath.Clean(c.exe())) {
		c.cache = cacheEntry{}
		return identityError(errors.New("image_path"))
	}
	id, size, mod, e := metadata(c.delivery())
	if e != nil {
		c.cache = cacheEntry{}
		return e
	}
	if !explicit && c.cached(p) && c.cache.DeliveryID == id && c.cache.DeliverySize == size && c.cache.DeliveryModified == mod && stampsCurrent(c.cache.Files) {
		return nil
	}
	c.cache = cacheEntry{}
	if !explicit {
		return ErrUnconfirmed
	}
	files, e := c.verifyDelivery(ctx)
	if e != nil {
		return identityError(e)
	}
	next, e := queryProcess(p.PID)
	if e != nil || next != p {
		return identityError(errors.New("process_changed"))
	}
	c.cache = cacheEntry{Identity: p, Expected: c.cfg.CenterSHA256, Delivery: c.cfg.DeliverySHA256, DeliveryID: id, DeliverySize: size, DeliveryModified: mod, Files: files}
	return nil
}
func writeFrame(w io.Writer, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if len(b) > MaxDescriptor {
		return errors.New("ipc_too_large")
	}
	frame := make([]byte, 4+len(b))
	binary.LittleEndian.PutUint32(frame, uint32(len(b)))
	copy(frame[4:], b)
	n, e := w.Write(frame)
	if e == nil && n != len(frame) {
		e = io.ErrShortWrite
	}
	return e
}
func readFrame(r io.Reader, v any) error {
	var n [4]byte
	if _, e := io.ReadFull(r, n[:]); e != nil {
		return e
	}
	size := binary.LittleEndian.Uint32(n[:])
	if size > MaxDescriptor {
		return errors.New("ipc_too_large")
	}
	b := make([]byte, size)
	if _, e := io.ReadFull(r, b); e != nil {
		return e
	}
	return decode(bytes.NewReader(b), MaxDescriptor, v)
}
func (c *Client) connect(ctx context.Context, explicit bool) (out handshakeResponse, err error) {
	// A failed identity/IPC check must also revoke any previously cached token.
	defer func() {
		if err != nil {
			c.cache = cacheEntry{}
		}
	}()
	f, e := os.Open(filepath.Join(c.cfg.DataDir, DescriptorName))
	if e != nil {
		return out, e
	}
	var d Descriptor
	e = decode(f, MaxDescriptor, &d)
	f.Close()
	if e != nil {
		return out, e
	}
	name, e := pipeName(c.cfg.DataDir)
	if e != nil {
		return out, e
	}
	if d.Schema != 1 || len(d.InstanceID) != 64 || d.PID == 0 || d.PipeName != name {
		return out, identityError(errors.New("descriptor"))
	}
	if _, e = canonical(d.LoopbackAddress); e != nil {
		return out, e
	}
	root, data, e := directoryIdentities(c.cfg)
	if e != nil {
		return out, e
	}
	if d.RootKind != c.cfg.RootKind || d.RootIdentity != root || d.DataIdentity != data {
		return out, identityError(errors.New("root_data_binding"))
	}
	conn, pid, e := dialPipe(ctx, name)
	if e != nil {
		return out, e
	}
	defer conn.Close()
	p, e := queryProcess(pid)
	if e != nil {
		return out, e
	}
	if pid != d.PID || p.Created != d.ProcessStartedAt {
		return out, identityError(errors.New("pipe_pid"))
	}
	if e = c.verify(ctx, p, explicit); e != nil {
		return out, e
	}
	// Reuse only after all live checks above. Closing this pipe without sending
	// a frame lets the host observe EOF without issuing another token. Session
	// storage lives inside the verified cache so cache invalidation revokes both.
	session := c.cache.Session
	age := time.Since(session.Started)
	if !explicit && session.Response.Descriptor == d && c.cached(p) && session.Response.Token != "" && age >= 0 && age < readSessionLifetime {
		return session.Response, nil
	}
	c.cache.Session = readSession{}
	started := time.Now()
	req := handshakeRequest{Schema: 1, Protocol: Protocol, InstanceID: d.InstanceID, RootKind: d.RootKind, RootIdentity: root, DataIdentity: data}
	if e = writeFrame(conn, req); e != nil {
		return out, e
	}
	if e = readFrame(conn, &out); e != nil {
		return out, e
	}
	if out.Schema != 1 || out.Protocol != Protocol || out.Descriptor != d || out.Build == "" || !safeCode(out.Build) || !digestValid(out.Token) {
		return handshakeResponse{}, identityError(errors.New("handshake"))
	}
	p2, e := queryProcess(pid)
	if e != nil || p2 != p {
		c.cache = cacheEntry{}
		return handshakeResponse{}, identityError(errors.New("process_changed"))
	}
	c.cache.Session = readSession{Response: out, Started: started}
	return out, nil
}
func fetch(ctx context.Context, h handshakeResponse, path string, v any, timeout time.Duration) error {
	return fetchLimited(ctx, h, path, v, timeout, MaxResponse)
}
func fetchLimited(ctx context.Context, h handshakeResponse, path string, v any, timeout time.Duration, limit int64) error {
	base, e := canonical(h.Descriptor.LoopbackAddress)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, "GET", base[:len(base)-1]+path, nil)
	if e != nil {
		return e
	}
	req.Header.Set(ReadOnlyHeader, h.Token)
	client := newHTTPClient(timeout)
	defer client.CloseIdleConnections()
	resp, e := client.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("center_http_status_%d", resp.StatusCode)
	}
	return decode(resp.Body, limit, v)
}
func (c *Client) inspect(ctx context.Context, explicit bool) (Status, error) {
	s := Status{ObservedAt: time.Now().UTC(), ProcessState: "not_running", IdentityState: "unconfirmed", HealthState: "unknown"}
	_, _, _, e := metadata(c.exe())
	s.Installed = e == nil
	if e != nil {
		c.cache = cacheEntry{}
		s.Reason = "center_missing"
		return s, e
	}
	h, e := c.connect(ctx, explicit)
	if e != nil {
		s.Reason = "center_identity_unconfirmed"
		return s, e
	}
	var health Health
	if e = fetch(ctx, h, HealthPath, &health, 2*time.Second); e != nil {
		c.cache = cacheEntry{}
		s.Reason = "center_health_unavailable"
		return s, e
	}
	if health.Schema != 1 || health.Product != Product || health.Protocol != Protocol || health.Build != h.Build || health.InstanceID != h.Descriptor.InstanceID {
		c.cache = cacheEntry{}
		return s, identityError(errors.New("health_binding"))
	}
	s.ProcessState = "running"
	s.IdentityState = "confirmed"
	s.HealthState = health.State
	s.InstanceID = health.InstanceID
	s.Build = health.Build
	s.Protocol = Protocol
	s.URL, _ = canonical(h.Descriptor.LoopbackAddress)
	return s, nil
}

// Inspect never starts a process or performs the first binary/manifest hash.
func (c *Client) Inspect(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inspect(ctx, false)
}

// Dependencies uses one fresh authenticated snapshot. Share this return value
// between request-scoped probes; it is deliberately not cached across requests.
func (c *Client) Dependencies(ctx context.Context) (DependencySnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	var s DependencySnapshot
	h, e := c.connect(ctx, false)
	if e != nil {
		return s, e
	}
	if e = fetch(ctx, h, DependenciesPath, &s, 3*time.Second); e != nil {
		c.cache = cacheEntry{}
		return s, e
	}
	if s.InstanceID != h.Descriptor.InstanceID || s.Build != h.Build {
		c.cache = cacheEntry{}
		return DependencySnapshot{}, identityError(errors.New("dependency_binding"))
	}
	e = validateSnapshot(&s, time.Now())
	return s, e
}

// EnsureRunning is only for explicit user open. It never opens a browser and
// never returns a URL containing credentials. Successful children are detached.
func (c *Client) EnsureRunning(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := safefs.SecureDir(c.cfg.DataDir); e != nil {
		return Status{}, e
	}
	lock, e := startupLock(ctx, filepath.Join(c.cfg.DataDir, "startup.lock"))
	if e != nil {
		return Status{}, e
	}
	defer lock.Close()
	s, e := c.inspect(ctx, true)
	if e == nil {
		return s, nil
	}
	// A descriptor of uncertain identity must not be deleted, bypassed or killed.
	if _, se := os.Stat(filepath.Join(c.cfg.DataDir, DescriptorName)); se == nil {
		return s, e
	} else if !os.IsNotExist(se) {
		return s, se
	}
	if _, e = c.verifyDelivery(ctx); e != nil {
		return s, e
	}
	listener, e := net.Listen("tcp", "127.0.0.1:18120")
	if e != nil {
		return s, errors.New("center_port_conflict: 中心端口被占用，未请求占用者")
	}
	listener.Close()
	cmd := exec.Command(c.exe(), "-root", c.cfg.Root, "-integration-data", c.cfg.DataDir, "-root-kind", c.cfg.RootKind, "-listen", "127.0.0.1:18120", "-install-root", c.cfg.InstallRoot, "-center-sha256", c.cfg.CenterSHA256, "-delivery-sha256", c.cfg.DeliverySHA256)
	cmd.Dir = filepath.Dir(c.exe())
	detach(cmd)
	if e = cmd.Start(); e != nil {
		return s, e
	}
	owned, e := queryProcess(uint32(cmd.Process.Pid))
	if e != nil {
		_ = cmd.Process.Release()
		return s, identityError(e)
	}
	success := false
	defer func() {
		if !success {
			current, qe := queryProcess(owned.PID)
			if qe == nil && current == owned {
				_ = cmd.Process.Kill()
			}
		}
		_ = cmd.Process.Release()
	}()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return s, errors.New("center_start_timeout: 中心启动等待取消或超过15秒")
		case <-tick.C:
			s, e = c.inspect(ctx, true)
			if e == nil {
				success = true
				return s, nil
			}
			if _, qe := queryProcess(owned.PID); qe != nil {
				return s, errors.New("center_start_failed: 中心子进程已退出")
			}
		}
	}
}
