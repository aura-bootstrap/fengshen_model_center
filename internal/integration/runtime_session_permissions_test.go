package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRuntimeSessionPermissionMatrix(t *testing.T) {
	h, p, _, cancelled := runtimeFixture(t)
	runtime, e := h.admitSession(context.Background(), *p, taskRequest())
	if e != nil {
		t.Fatal(e)
	}
	h.cfg.EngineAllowlist[0].Edition = "admin"
	h.cfg.EngineAllowlist[0].Management = true
	management, e := h.admitSession(context.Background(), *p, SessionRequest{Permission: PermissionManagement})
	if e != nil {
		t.Fatal(e)
	}
	readonly, e := h.issue()
	if e != nil {
		t.Fatal(e)
	}
	credentials := []struct{ header, token string }{{ReadOnlyHeader, readonly}, {RuntimeHeader, runtime.Token}, {ManagementHeader, management.Token}, {"Authorization", "Bearer browser-token"}, {RuntimeHeader, readonly}, {RuntimeHeader, management.Token}, {ManagementHeader, runtime.Token}}
	for i, c := range credentials {
		for _, permission := range []SessionPermission{PermissionReadOnly, PermissionRuntime, PermissionManagement} {
			method, path := "POST", RuntimeJobsPath
			if permission == PermissionReadOnly {
				method, path = "GET", HealthPath
			}
			if permission == PermissionManagement {
				method, path = "GET", BindingsPath
			}
			r := httptest.NewRequest(method, "http://127.0.0.1:18120"+path, nil)
			r.Header.Set(c.header, c.token)
			allowed := false
			if permission == PermissionReadOnly {
				allowed = h.AuthorizeReadOnly(r)
			} else {
				_, e = h.AuthorizeSession(r, permission)
				allowed = e == nil
			}
			want := i == 0 && permission == PermissionReadOnly || i == 1 && permission == PermissionRuntime || i == 2 && permission == PermissionManagement
			if allowed != want {
				t.Fatalf("credential %d permission %s allowed=%v", i, permission, allowed)
			}
		}
	}
	for _, mode := range []string{"host", "origin", "site", "query", "escaped", "duplicate", "mixed", "cookie"} {
		t.Run(mode, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://127.0.0.1:18120"+RuntimeJobsPath, nil)
			r.Header.Set(RuntimeHeader, runtime.Token)
			switch mode {
			case "host":
				r.Host = "localhost:18120"
			case "origin":
				r.Header.Set("Origin", "https://evil.example")
			case "site":
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			case "query":
				r.URL.RawQuery = "token=x"
			case "escaped":
				r.URL.RawPath = "/api/runtime/%6aobs"
			case "duplicate":
				r.Header.Add(RuntimeHeader, runtime.Token)
			case "mixed":
				r.Header.Set(ReadOnlyHeader, readonly)
			case "cookie":
				r.Header.Set("Cookie", "browser=secret")
			}
			if _, e := h.AuthorizeSession(r, PermissionRuntime); e == nil {
				t.Fatal("boundary bypass")
			}
		})
	}
	// Authenticated deletion cancels once and never affects the independent host.
	r := httptest.NewRequest("DELETE", "http://127.0.0.1:18120"+RuntimeSessionPath, nil)
	r.Header.Set(RuntimeHeader, runtime.Token)
	if e = h.DeleteSession(r); e != nil {
		t.Fatal(e)
	}
	if h.closed || *cancelled != 1 {
		t.Fatal("delete lifecycle")
	}
	if e = h.DeleteSession(r); e == nil {
		t.Fatal("revoked credential reused")
	}
	h.Close()
	if *cancelled != 2 {
		t.Fatal("management close cancellation")
	}
}
func TestRuntimeCredentialHeaderIsolation(t *testing.T) {
	h, p, _, _ := runtimeFixture(t)
	runtime, err := h.admitSession(context.Background(), *p, taskRequest())
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.EngineAllowlist[0].Edition = "admin"
	h.cfg.EngineAllowlist[0].Management = true
	management, err := h.admitSession(context.Background(), *p, SessionRequest{Permission: PermissionManagement})
	if err != nil {
		t.Fatal(err)
	}
	readonly, err := h.issue()
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range []struct {
		permission                  SessionPermission
		header, token, method, path string
	}{
		{PermissionReadOnly, ReadOnlyHeader, readonly, "GET", HealthPath},
		{PermissionRuntime, RuntimeHeader, runtime.Token, "POST", RuntimeJobsPath},
		{PermissionManagement, ManagementHeader, management.Token, "GET", BindingsPath},
	} {
		for _, other := range []string{ReadOnlyHeader, RuntimeHeader, ManagementHeader, "X-Model-Center-Token", "Authorization", "Cookie"} {
			if other == auth.header {
				continue
			}
			for _, value := range []string{"browser-or-other-token", ""} {
				t.Run(string(auth.permission)+"/"+other+"/"+value, func(t *testing.T) {
					r := httptest.NewRequest(auth.method, "http://127.0.0.1:18120"+auth.path, nil)
					r.Header.Set(auth.header, auth.token)
					r.Header.Set(other, value)
					if auth.permission == PermissionReadOnly {
						if h.AuthorizeReadOnly(r) {
							t.Fatal("mixed credentials accepted")
						}
					} else if _, err := h.AuthorizeSession(r, auth.permission); err == nil {
						t.Fatal("mixed credentials accepted")
					}
				})
			}
		}
		for _, header := range []string{"Origin", "Sec-Fetch-Site"} {
			for _, second := range []string{"", "https://evil.example", "same-origin"} {
				t.Run(string(auth.permission)+"/duplicate-"+header+"/"+second, func(t *testing.T) {
					r := httptest.NewRequest(auth.method, "http://127.0.0.1:18120"+auth.path, nil)
					r.Header.Set(auth.header, auth.token)
					first := "same-origin"
					if header == "Origin" {
						first = "http://127.0.0.1:18120"
					}
					r.Header[header] = []string{first, second}
					if auth.permission == PermissionReadOnly {
						if h.AuthorizeReadOnly(r) {
							t.Fatal("multiple browser boundary headers accepted")
						}
					} else if _, err := h.AuthorizeSession(r, auth.permission); err == nil {
						t.Fatal("multiple browser boundary headers accepted")
					}
				})
			}
		}
	}
}

func TestRuntimeSessionMaximumAndMetadata(t *testing.T) {
	for _, field := range []string{"pid", "created", "image", "file", "size", "modified", "sid", "exit"} {
		t.Run(field, func(t *testing.T) {
			h, p, _, cancelled := runtimeFixture(t)
			s, e := h.admitSession(context.Background(), *p, taskRequest())
			if e != nil {
				t.Fatal(e)
			}
			switch field {
			case "pid":
				p.PID++
			case "created":
				p.Created += "x"
			case "image":
				p.Image += "x"
			case "file":
				p.FileID += "x"
			case "size":
				p.Size++
			case "modified":
				p.Modified++
			case "sid":
				p.SID += "x"
			case "exit":
				h.process = func(uint32) (processIdentity, error) { return processIdentity{}, fmt.Errorf("exit") }
			}
			if sessionHTTP(h, s.Token, PermissionRuntime).Code != 403 || *cancelled != 1 {
				t.Fatal("metadata reuse")
			}
		})
	}
	h, p, now, cancelled := runtimeFixture(t)
	s, e := h.admitSession(context.Background(), *p, taskRequest())
	if e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:18120"+SessionHeartbeatPath, nil)
	r.Header.Set(RuntimeHeader, s.Token)
	for i := 0; i < 29; i++ {
		*now = now.Add(4 * time.Minute)
		if e = h.HeartbeatSession(r); e != nil {
			t.Fatal(e)
		}
	}
	*now = now.Add(4 * time.Minute)
	if h.HeartbeatSession(r) == nil || *cancelled != 1 {
		t.Fatal("two-hour maximum extended")
	}
}
func TestRuntimeCredentialsNeverFormatSecret(t *testing.T) {
	s := &SessionCredentials{token: strings.Repeat("a", 64)}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if strings.Contains(fmt.Sprintf(format, s), s.token) {
			t.Fatal("secret formatted")
		}
	}
	b, _ := json.Marshal(s)
	if string(b) != "{}" {
		t.Fatal("credentials serialized")
	}
}
