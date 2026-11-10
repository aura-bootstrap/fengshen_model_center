package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func httpManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	for _, p := range []string{"sidecar", "models"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0700); err != nil {
			t.Fatal(err)
		}
	}
	m, err := NewManager(root, filepath.Join(t.TempDir(), "paths.json"), testManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	allowFixtureRoots(m, filepath.Join(root, "models"))
	t.Cleanup(func() { _ = m.Close() })
	return m
}
func TestHTTPContractsAndHeaders(t *testing.T) {
	m := httpManager(t)
	h := newHandler(m, "127.0.0.1:18120", nil)
	tests := []struct {
		name, method, path, body, host, origin, site string
		want                                         int
	}{
		{"state", "GET", "/api/state", "", "127.0.0.1:18120", "", "", 200},
		{"report", "GET", "/api/report", "", "127.0.0.1:18120", "", "", 200},
		{"report origin", "GET", "/api/report", "", "127.0.0.1:18120", "null", "", 403},
		{"bad host", "GET", "/", "", "localhost:18120", "", "", 403},
		{"null origin", "GET", "/", "", "127.0.0.1:18120", "null", "", 403},
		{"same site", "GET", "/", "", "127.0.0.1:18120", "", "same-site", 403},
		{"cross site", "GET", "/api/state", "", "127.0.0.1:18120", "", "cross-site", 403},
		{"unknown resource", "GET", "/catalog.go", "", "127.0.0.1:18120", "", "", 404},
		{"method", "PUT", "/api/state", "{}", "127.0.0.1:18120", "", "", 405},
		{"unknown field", "POST", "/api/operations/scan", "{\"registry_revision\":0,\"path\":\"x\"}", "127.0.0.1:18120", "", "", 400},
		{"null body", "POST", "/api/operations/scan", "null", "127.0.0.1:18120", "", "", 400},
		{"two objects", "POST", "/api/operations/scan", "{}{}", "127.0.0.1:18120", "", "", 400},
		{"missing revision", "POST", "/api/operations/scan", "{}", "127.0.0.1:18120", "", "", 422},
		{"oversized", "POST", "/api/operations/scan", strings.Repeat(" ", 8193), "127.0.0.1:18120", "", "", 413},
		{"file ID not path", "GET", "/api/inventories/unknown/models/local-text/files", "", "127.0.0.1:18120", "", "", 404},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, strings.NewReader(tc.body))
			r.Host = tc.host
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "object-src 'none'") {
				t.Fatal("security headers")
			}
			if tc.want == 405 && w.Header().Get("Allow") == "" {
				t.Error("Allow header missing")
			}
		})
	}
	if m.State().CurrentInventory != "" {
		t.Fatal("GET triggered scan")
	}
}

// Requests go through the production handler and JSON decoding.
func httpRequest(t *testing.T, h http.Handler, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1:18120"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, w.Code, status, w.Body.String())
	}
	for key, want := range map[string]string{
		"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
		"Cache-Control": "no-store", "X-Frame-Options": "DENY",
		"Content-Security-Policy": "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; object-src 'none'; frame-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'",
	} {
		if got := w.Header().Get(key); got != want {
			t.Errorf("%s=%q want=%q", key, got, want)
		}
	}
	for _, key := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials"} {
		if w.Header().Get(key) != "" {
			t.Errorf("unexpected %s", key)
		}
	}
	return w
}

func httpJSON(t *testing.T, w *httptest.ResponseRecorder, out any) {
	t.Helper()
	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("JSON content type=%q body=%s", got, w.Body.String())
	}
	d := json.NewDecoder(w.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		t.Fatal(err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		t.Fatalf("response has trailing JSON: %v", err)
	}
}

func httpError(t *testing.T, h http.Handler, method, path, body string, status int, code string) {
	t.Helper()
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	httpJSON(t, httpRequest(t, h, method, path, body, status), &envelope)
	if envelope.Error.Code != code || envelope.Error.Message == "" {
		t.Fatalf("error=%+v want code=%s", envelope.Error, code)
	}
}

func httpBody(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func httpOpaqueID(t *testing.T, id string) {
	t.Helper()
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != 16 || id != strings.ToLower(id) {
		t.Fatalf("not an opaque ID: %q", id)
	}
}

func httpStart(t *testing.T, h http.Handler, kind, body string) Operation {
	t.Helper()
	var op Operation
	httpJSON(t, httpRequest(t, h, "POST", "/api/operations/"+kind, body, 202), &op)
	httpOpaqueID(t, op.ID)
	if op.Kind != kind || op.Status != "running" || op.FinishedAt != nil || op.StartedAt.IsZero() {
		t.Fatalf("accepted operation=%+v", op)
	}
	return op
}

func httpPoll(t *testing.T, h http.Handler, id, status string) Operation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var op Operation
		httpJSON(t, httpRequest(t, h, "GET", "/api/operations/"+id, "", 200), &op)
		if op.ID != id {
			t.Fatalf("operation ID=%q want=%q", op.ID, id)
		}
		if op.FinishedAt != nil {
			if op.Status != status || op.FinishedAt.Before(op.StartedAt) {
				t.Fatalf("terminal operation=%+v want=%s", op, status)
			}
			return op
		}
		if op.Status != "running" && op.Status != "cancelling" {
			t.Fatalf("nonterminal operation=%+v", op)
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s timed out", id)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHTTPStrictBodies(t *testing.T) {
	m := httpManager(t)
	h := newHandler(m, "127.0.0.1:18120", nil)
	for _, endpoint := range []struct{ method, path, valid, unknown, wrongType string }{
		{"POST", "/api/operations/scan", `{"registry_revision":0}`, `{"registry_revision":0,"extra":1}`, `{"registry_revision":"0"}`},
		{"POST", "/api/operations/verify", `{"inventory_id":"missing","model_id":"whisper-asr","file_ids":[]}`, `{"extra":1}`, `{"file_ids":"x"}`},
		{"POST", "/api/operations/verify-component", `{"component_id":"missing","version":"1"}`, `{"extra":1}`, `{"component_id":7}`},
		{"POST", "/api/operations/missing/cancel", `{}`, `{"extra":1}`, `{"extra":[]}`},
	} {
		t.Run(endpoint.method+endpoint.path, func(t *testing.T) {
			for n, body := range []string{"", "null", "[]", "true", `"text"`, "{", "{}{}", "{} trailing", `{"x":1,"x":2}`, endpoint.unknown, endpoint.wrongType} {
				t.Run(fmt.Sprint(n), func(t *testing.T) { httpError(t, h, endpoint.method, endpoint.path, body, 400, "request_invalid") })
			}
			body := endpoint.valid + strings.Repeat(" ", 8193-len(endpoint.valid))
			httpError(t, h, endpoint.method, endpoint.path, body, 413, "request_too_large")
		})
	}
	for _, body := range []string{`{"registry_revision":-1}`, `{"registry_revision":1.5}`, `{"registry_revision":18446744073709551616}`, `{"registry_revision":0,"registry_revision":0}`} {
		httpError(t, h, "POST", "/api/operations/scan", body, 400, "request_invalid")
	}
	for _, tc := range []struct{ method, path, body, code string }{
		{"POST", "/api/operations/scan", `{}`, "request_invalid"},
		{"POST", "/api/operations/scan", `{"registry_revision":null}`, "request_invalid"},
	} {
		httpError(t, h, tc.method, tc.path, tc.body, 422, tc.code)
	}
	// Exactly 8 KiB reaches semantic validation; one more byte is rejected above.
	body := `{}` + strings.Repeat(" ", 8192-2)
	httpError(t, h, "POST", "/api/operations/scan", body, 422, "request_invalid")
	if m.State().Registry.Revision != 0 || m.State().LastAttempt != nil {
		t.Fatal("rejected bodies changed state")
	}
}

func TestHTTPControlledCancellation(t *testing.T) {
	for _, kind := range []string{"scan", "verify"} {
		t.Run(kind, func(t *testing.T) {
			m := httpManager(t)
			h := newHandler(m, "127.0.0.1:18120", nil)
			entered := make(chan struct{}, 1)
			// Keep the worker behind the hook until both cancel calls have run.
			release := make(chan struct{})
			defer close(release)
			hook := func(ctx context.Context) {
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-m.ctx.Done():
				}
			}
			body := `{"registry_revision":0}`
			if kind == "scan" {
				m.beforeScan = hook
			} else {
				if err := os.WriteFile(filepath.Join(m.root, "models", "955717e8-a.th"), []byte("abc"), 0600); err != nil {
					t.Fatal(err)
				}
				scan := httpStart(t, h, "scan", body)
				done := httpPoll(t, h, scan.ID, "completed")
				var page struct {
					Files []ModelFile `json:"files"`
					Next  string      `json:"next_cursor"`
				}
				httpJSON(t, httpRequest(t, h, "GET", "/api/inventories/"+done.ResultID+"/models/demucs/files", "", 200), &page)
				if len(page.Files) != 1 {
					t.Fatalf("files=%+v", page)
				}
				body = httpBody(t, map[string]any{"inventory_id": done.ResultID, "model_id": "demucs", "file_ids": []string{page.Files[0].ID}})
				m.beforeRead = hook
			}
			op := httpStart(t, h, kind, body)
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("worker did not reach hook")
			}
			httpError(t, h, "POST", "/api/operations/scan", `{"registry_revision":0}`, 409, "operation_busy")
			for n := 0; n < 2; n++ {
				var cancelled Operation
				httpJSON(t, httpRequest(t, h, "POST", "/api/operations/"+op.ID+"/cancel", `{}`, 200), &cancelled)
				if cancelled.ID != op.ID || cancelled.Status != "cancelling" || cancelled.FinishedAt != nil {
					t.Fatalf("cancel=%+v", cancelled)
				}
			}
			// The hook observes release without changing any production behavior.
			release <- struct{}{}
			done := httpPoll(t, h, op.ID, "cancelled")
			for _, hash := range done.Hashes {
				if hash.Status != "cancelled" || hash.SHA256 != "" {
					t.Fatalf("cancel published hash=%+v", hash)
				}
			}
			var again Operation
			httpJSON(t, httpRequest(t, h, "POST", "/api/operations/"+op.ID+"/cancel", `{}`, 200), &again)
			if httpBody(t, done) != httpBody(t, again) {
				t.Fatal("terminal cancellation not idempotent")
			}
		})
	}
}

func TestHTTPRoutes(t *testing.T) {
	m := httpManager(t)
	h := newHandler(m, "127.0.0.1:18120", nil)
	for _, path := range []string{"/", "/styles.css", "/app.js", "/catalog.go", "/web/", "/web/index.html", "/registrations.json", "/missing", "/app.js/", "/%61pp.js"} {
		code := "resource_unknown"
		if strings.Contains(path, "%") {
			code = "path_invalid"
		}
		httpError(t, h, "GET", path, "", 404, code)
	}
	for _, tc := range []struct {
		method, path, allow string
		json                bool
	}{
		{"PUT", "/api/state", "GET, HEAD", false}, {"PUT", "/api/operations/scan", "GET, HEAD, POST", false},
		{"PUT", "/api/inventories/missing", "GET, HEAD", false},
		{"GET", "/api/operations/missing/cancel", "POST", false},
		{"HEAD", "/api/state", "GET", true},
	} {
		w := httpRequest(t, h, tc.method, tc.path, "{}", 405)
		if got := w.Header().Get("Allow"); got != tc.allow {
			t.Errorf("%s %s Allow=%q want=%q", tc.method, tc.path, got, tc.allow)
		}
		// ServeMux's native 405 is plain text; wrapper errors are JSON.
		if tc.json {
			var envelope struct {
				Error apiError `json:"error"`
			}
			httpJSON(t, w, &envelope)
			if envelope.Error.Code != "method_not_allowed" {
				t.Fatalf("error=%+v", envelope)
			}
		} else if w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || w.Body.String() != "Method Not Allowed\n" {
			t.Fatalf("mux 405=%s", w.Body.String())
		}
	}
	httpError(t, h, "GET", "/api/inventories/missing", "", 404, "inventory_unknown")
	httpError(t, h, "POST", "/api/operations/verify", `{"inventory_id":"missing"}`, 404, "inventory_unknown")
	// GET matches /api/operations/{id}, even when the ID spells a POST endpoint.
	httpError(t, h, "GET", "/api/operations/scan", "", 410, "operation_gone")
	httpError(t, h, "GET", "/api/operations/missing", "", 410, "operation_gone")
	httpError(t, h, "POST", "/api/operations/missing/cancel", `{}`, 410, "operation_gone")
	w := httpRequest(t, h, "GET", "/api/unknown", "", 404)
	if w.Body.String() != "404 page not found\n" {
		t.Fatalf("mux 404=%q", w.Body.String())
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	httpError(t, h, "GET", "/api/state", "", 503, "service_closed")
}

func TestResolveRootStrict(t *testing.T) {
	if _, err := resolveRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveRoot(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing directory accepted")
	}
	for _, address := range []string{"0.0.0.0:1", "localhost:1", "example.com:1", "192.168.1.1:1"} {
		if err := validateListen(address); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
	for _, address := range []string{"127.0.0.1:0", "[::1]:0"} {
		if err := validateListen(address); err != nil {
			t.Fatal(err)
		}
	}
}
