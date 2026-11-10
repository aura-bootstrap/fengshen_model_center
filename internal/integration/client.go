package integration

import (
	"context"
	"errors"
	"fmt"
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
	Size, Modified         int64
}
type Client struct {
	mu  sync.Mutex
	cfg Config
}

func DefaultConfig() Config {
	exe, _ := os.Executable()
	base, _ := os.UserConfigDir()
	return Config{InstallRoot: filepath.Dir(exe), Root: filepath.Dir(exe), RootKind: "installed", DataDir: filepath.Join(base, "FengshenSlicer", "model-center-integration")}
}
func NewClient(cfg Config) (*Client, error) {
	if e := platformSupported(); e != nil {
		return nil, e
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
func (c *Client) connect(ctx context.Context) (out Descriptor, err error) {
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
	if d.Schema != 1 || len(d.InstanceID) != 64 || d.PID == 0 || d.Build == "" || !safeCode(d.Build) {
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
	p, e := queryProcess(d.PID)
	if e != nil {
		return out, e
	}
	if p.Created != d.ProcessStartedAt {
		return out, identityError(errors.New("descriptor_pid"))
	}
	return d, ctx.Err()
}
func (c *Client) address(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, e := c.connect(ctx)
	if e != nil {
		return "", e
	}
	if _, e := canonical(d.LoopbackAddress); e != nil {
		return "", e
	}
	return d.LoopbackAddress, nil
}
func fetch(ctx context.Context, d Descriptor, path string, v any, timeout time.Duration) error {
	return fetchLimited(ctx, d, path, v, timeout, MaxResponse)
}
func fetchLimited(ctx context.Context, d Descriptor, path string, v any, timeout time.Duration, limit int64) error {
	base, e := canonical(d.LoopbackAddress)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, "GET", base[:len(base)-1]+path, nil)
	if e != nil {
		return e
	}
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
func (c *Client) inspect(ctx context.Context) (Status, error) {
	s := Status{ObservedAt: time.Now().UTC(), ProcessState: "not_running", IdentityState: "unconfirmed", HealthState: "unknown"}
	_, _, _, e := metadata(c.exe())
	s.Installed = e == nil
	if e != nil {
		s.Reason = "center_missing"
		return s, e
	}
	d, e := c.connect(ctx)
	if e != nil {
		s.Reason = "center_unavailable"
		return s, e
	}
	var health Health
	if e = fetch(ctx, d, HealthPath, &health, 2*time.Second); e != nil {
		s.Reason = "center_health_unavailable"
		return s, e
	}
	if health.Schema != 1 || health.Product != Product || health.Protocol != Protocol || health.Build != d.Build || health.InstanceID != d.InstanceID {
		return s, identityError(errors.New("health_binding"))
	}
	s.ProcessState = "running"
	s.IdentityState = "confirmed"
	s.HealthState = health.State
	s.InstanceID = health.InstanceID
	s.Build = health.Build
	s.Protocol = Protocol
	s.URL, _ = canonical(d.LoopbackAddress)
	return s, nil
}

// Inspect never starts a process.
func (c *Client) Inspect(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inspect(ctx)
}

// Dependencies uses one fresh snapshot. Share this return value between
// request-scoped probes; it is deliberately not cached across requests.
func (c *Client) Dependencies(ctx context.Context) (DependencySnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	var s DependencySnapshot
	d, e := c.connect(ctx)
	if e != nil {
		return s, e
	}
	if e = fetch(ctx, d, DependenciesPath, &s, 3*time.Second); e != nil {
		return s, e
	}
	if s.InstanceID != d.InstanceID || s.Build != d.Build {
		return DependencySnapshot{}, identityError(errors.New("dependency_binding"))
	}
	e = validateSnapshot(&s, time.Now())
	return s, e
}

// EnsureRunning is only for explicit user open. It never opens a browser and
// never returns a URL containing secrets. Successful children are detached.
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
	s, e := c.inspect(ctx)
	if e == nil {
		return s, nil
	}
	// A descriptor of uncertain identity must not be deleted, bypassed or killed.
	if _, se := os.Stat(filepath.Join(c.cfg.DataDir, DescriptorName)); se == nil {
		return s, e
	} else if !os.IsNotExist(se) {
		return s, se
	}
	listener, e := net.Listen("tcp", "127.0.0.1:18120")
	if e != nil {
		return s, errors.New("center_port_conflict: 中心端口被占用，未请求占用者")
	}
	listener.Close()
	cmd := exec.Command(c.exe(), "-root", c.cfg.Root, "-integration-data", c.cfg.DataDir, "-root-kind", c.cfg.RootKind, "-listen", "127.0.0.1:18120", "-install-root", c.cfg.InstallRoot)
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
			s, e = c.inspect(ctx)
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
