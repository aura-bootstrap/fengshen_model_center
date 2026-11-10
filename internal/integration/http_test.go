package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPRedirectAndProxyDenied(t *testing.T) {
	hits := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; t.Error("redirect target contacted") }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer server.Close()
	t.Setenv("HTTP_PROXY", destination.URL)
	t.Setenv("HTTPS_PROXY", destination.URL)
	c := newHTTPClient(time.Second)
	defer c.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", server.URL, nil)
	if _, e := c.Do(req); e == nil {
		t.Fatal("redirect accepted")
	}
	if hits != 0 {
		t.Fatal(hits)
	}
	if c.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("proxy enabled")
	}
}
func TestSnapshotEvidenceLimits(t *testing.T) {
	now := time.Now()
	base := func() DependencySnapshot {
		return DependencySnapshot{Schema: 1, FinishedAt: now, Models: []Evidence{{ID: "seed-vc", Inference: "not_tested"}}}
	}
	good := base()
	if e := validateSnapshot(&good, now); e != nil {
		t.Fatal(e)
	}
	for _, modify := range []func(*DependencySnapshot){
		func(s *DependencySnapshot) { s.Models[0].Inference = "passed" },
		func(s *DependencySnapshot) { s.Diagnostics = []string{`C:\secret\models`} },
		func(s *DependencySnapshot) { s.Components = make([]Evidence, 65) },
		func(s *DependencySnapshot) { s.Diagnostics = make([]string, 257) },
	} {
		s := base()
		modify(&s)
		if validateSnapshot(&s, now) == nil {
			t.Fatal("invalid snapshot accepted")
		}
	}
	stale := base()
	stale.FinishedAt = now.Add(-6 * time.Minute)
	if e := validateSnapshot(&stale, now); e != nil || !stale.Stale {
		t.Fatal("stale missing")
	}
}
