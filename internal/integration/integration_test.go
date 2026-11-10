package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDescriptorPhaseDiagnostics(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	for _, phase := range []string{"write", "sync", "close", "publish"} {
		descriptorPhase(phase)()
	}
	want := regexp.MustCompile(`center_descriptor phase=(write|sync|close|publish) event=(start|done) elapsed_ms=[0-9]+$`)
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 8 {
		t.Fatalf("phase record count=%d", len(lines))
	}
	for i, phase := range []string{"write", "sync", "close", "publish"} {
		if !want.MatchString(lines[2*i]) || !strings.HasSuffix(lines[2*i], "phase="+phase+" event=start elapsed_ms=0") || !want.MatchString(lines[2*i+1]) || !strings.Contains(lines[2*i+1], "phase="+phase+" event=done elapsed_ms=") {
			t.Fatal("missing fixed start/done diagnostics")
		}
	}
	output.Reset()
	for _, phase := range []string{"secret-token", "write\nsecret", strings.Repeat("secret", 1000), `W:\private\instance.json`, "http://private.invalid"} {
		descriptorPhase(phase)()
	}
	if output.Len() != 0 {
		t.Fatal("unapproved phase leaked")
	}
}

func TestStrictProtocol(t *testing.T) {
	for _, s := range []string{`{"schema":1,"extra":true}`, `{"schema":1} {}`, strings.Repeat(" ", 8193)} {
		var d Descriptor
		if decode(bytes.NewBufferString(s), 8192, &d) == nil {
			t.Fatalf("accepted invalid protocol: %.40s", s)
		}
	}
}
func TestLiteralLoopback(t *testing.T) {
	for _, a := range []string{"localhost:18120", "0.0.0.0:18120", "127.0.0.1:0", "127.0.0.1:80/x"} {
		if _, e := canonical(a); e == nil {
			t.Fatal(a)
		}
	}
	for _, a := range []string{"127.0.0.1:18120", "[::1]:18120"} {
		if _, e := canonical(a); e != nil {
			t.Fatal(e)
		}
	}
}
func TestTokenLimitsAndIsolation(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := &Host{now: func() time.Time { return now }, tokens: map[string]time.Time{}, desc: Descriptor{LoopbackAddress: "127.0.0.1:18120"}}
	var token string
	for i := 0; i < 32; i++ {
		var e error
		token, e = h.issue()
		if e != nil {
			t.Fatal(e)
		}
	}
	if _, e := h.issue(); e == nil {
		t.Fatal("unbounded sessions")
	}
	req := httptest.NewRequest("GET", "http://127.0.0.1:18120/api/integration/health", nil)
	req.Header.Set(ReadOnlyHeader, token)
	if !h.AuthorizeReadOnly(req) {
		t.Fatal("valid token rejected")
	}
	for _, path := range []string{"/api/state", "/api/integration/health?x=1", "/api/integration/health/"} {
		r := req.Clone(context.Background())
		r.URL.Path = strings.Split(path, "?")[0]
		if strings.Contains(path, "?") {
			r.URL.RawQuery = "x=1"
		}
		if h.AuthorizeReadOnly(r) {
			t.Fatal(path)
		}
	}
	req.Header.Set("Origin", "http://evil.invalid")
	if h.AuthorizeReadOnly(req) {
		t.Fatal("origin")
	}
	req.Header.Del("Origin")
	now = now.Add(60 * time.Second)
	if h.AuthorizeReadOnly(req) {
		t.Fatal("expired")
	}
	if _, e := h.issue(); e != nil {
		t.Fatal(e)
	}
}
func TestConcurrentTokenBound(t *testing.T) {
	h := &Host{now: time.Now, tokens: map[string]time.Time{}}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.issue() }()
	}
	wg.Wait()
	if len(h.tokens) != 32 {
		t.Fatal(len(h.tokens))
	}
}
func TestIdentityCacheBinding(t *testing.T) {
	a := processIdentity{PID: 1, Created: "a", Image: "x", FileID: "v:f", Size: 12, Modified: 3}
	c := &Client{cache: cacheEntry{Identity: a, Expected: strings.Repeat("a", 64), Delivery: strings.Repeat("b", 64)}}
	c.cfg.CenterSHA256 = c.cache.Expected
	c.cfg.DeliverySHA256 = c.cache.Delivery
	if !c.cached(a) {
		t.Fatal("cache miss")
	}
	b := a
	b.Created = "b"
	if c.cached(b) {
		t.Fatal("PID reuse")
	}
	b = a
	b.FileID = "other"
	if c.cached(b) {
		t.Fatal("file replacement")
	}
	c.cfg.CenterSHA256 = strings.Repeat("c", 64)
	if c.cached(a) {
		t.Fatal("expected digest changed")
	}
}
func TestNoCredentialsInDescriptor(t *testing.T) {
	b, _ := json.Marshal(Descriptor{})
	if strings.Contains(string(b), "token") {
		t.Fatal(string(b))
	}
}
func TestUntrustedInspectDoesNotHash(t *testing.T) {
	c := &Client{cfg: Config{CenterSHA256: strings.Repeat("a", 64), DeliverySHA256: strings.Repeat("b", 64)}}
	if c.cached(processIdentity{}) {
		t.Fatal("first inspect must not trust empty cache")
	}
}
