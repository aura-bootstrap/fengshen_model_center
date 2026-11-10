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
	m, err := NewManager(root, filepath.Join(t.TempDir(), "paths.json"), []string{filepath.Join(root, "models")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}
func TestHTTPSecurityAndContracts(t *testing.T) {
	m := httpManager(t)
	h := newHandler(m, "test-secret", "127.0.0.1:18120", nil)
	tests := []struct {
		name, method, path, body, host, origin, site, token string
		want                                                int
	}{
		{"state", "GET", "/api/state", "", "127.0.0.1:18120", "", "", "test-secret", 200},
		{"missing token", "GET", "/api/state", "", "127.0.0.1:18120", "", "", "", 403},
		{"report token", "GET", "/api/report", "", "127.0.0.1:18120", "", "", "", 403},
		{"report origin", "GET", "/api/report", "", "127.0.0.1:18120", "null", "", "test-secret", 403},
		{"bad host", "GET", "/", "", "localhost:18120", "", "", "", 403},
		{"null origin", "GET", "/", "", "127.0.0.1:18120", "null", "", "", 403},
		{"same site", "GET", "/", "", "127.0.0.1:18120", "", "same-site", "", 403},
		{"cross site", "GET", "/api/state", "", "127.0.0.1:18120", "", "cross-site", "test-secret", 403},
		{"static whitelist", "GET", "/catalog.go", "", "127.0.0.1:18120", "", "", "", 404},
		{"static no directory", "GET", "/web/", "", "127.0.0.1:18120", "", "", "", 404},
		{"method", "PUT", "/api/state", "{}", "127.0.0.1:18120", "", "", "test-secret", 405},
		{"unknown field", "POST", "/api/operations/scan", "{\"registry_revision\":0,\"path\":\"x\"}", "127.0.0.1:18120", "", "", "test-secret", 400},
		{"null body", "POST", "/api/operations/scan", "null", "127.0.0.1:18120", "", "", "test-secret", 400},
		{"two objects", "POST", "/api/operations/scan", "{}{}", "127.0.0.1:18120", "", "", "test-secret", 400},
		{"missing revision", "POST", "/api/operations/scan", "{}", "127.0.0.1:18120", "", "", "test-secret", 422},
		{"oversized", "POST", "/api/operations/scan", strings.Repeat(" ", 8193), "127.0.0.1:18120", "", "", "test-secret", 413},
		{"file ID not path", "GET", "/api/inventories/unknown/models/local-text/files", "", "127.0.0.1:18120", "", "", "test-secret", 404},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, strings.NewReader(tc.body))
			r.Host = tc.host
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			r.Header.Set("X-Model-Center-Token", tc.token)
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

// Requests go through the production handler, including authentication and JSON decoding.
func httpRequest(t *testing.T, h http.Handler, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1:18120"+path, strings.NewReader(body))
	r.Header.Set("X-Model-Center-Token", "secret")
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

func TestHTTPScanLifecycle(t *testing.T) {
	m := httpManager(t)
	h := newHandler(m, "secret", "127.0.0.1:18120", nil)
	dir := t.TempDir()
	for _, name := range []string{"a.gguf", "b.gguf", "c.gguf"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("abc"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	registration := httpBody(t, map[string]any{"path": dir, "expected_revision": 0})
	for _, token := range []string{"", "wrong-secret"} {
		r := httptest.NewRequest("PUT", "http://127.0.0.1:18120/api/registrations/whisper-asr", strings.NewReader(registration))
		r.Header.Set("X-Model-Center-Token", token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var envelope struct {
			Error apiError `json:"error"`
		}
		if w.Code != 403 {
			t.Fatalf("unauthorized PUT: %d %s", w.Code, w.Body.String())
		}
		httpJSON(t, w, &envelope)
		if envelope.Error.Code != "session_invalid" {
			t.Fatalf("unauthorized PUT error=%+v", envelope)
		}
	}
	if m.State().Registry.Revision != 0 || len(m.State().Registry.Registrations) != 0 {
		t.Fatal("unauthorized PUT changed registry")
	}
	var registered struct {
		Schema        int                        `json:"schema_version"`
		Revision      uint64                     `json:"revision"`
		Registrations map[string]json.RawMessage `json:"registrations"`
	}
	httpJSON(t, httpRequest(t, h, "PUT", "/api/registrations/whisper-asr", registration, 200), &registered)
	if registered.Revision != 1 || registered.Schema != 1 || len(registered.Registrations) != 1 {
		t.Fatalf("registration=%+v", registered)
	}
	httpError(t, h, "PUT", "/api/registrations/whisper-asr", httpBody(t, map[string]any{"path": t.TempDir(), "expected_revision": 0}), 409, "registry_conflict")
	var state State
	httpJSON(t, httpRequest(t, h, "GET", "/api/state", "", 200), &state)
	if state.Registry.Revision != 1 || state.Registry.Registrations["whisper-asr"].InputPath != dir || state.CurrentInventory != "" || state.ActiveOperation != nil {
		t.Fatalf("conflict mutated state or GET scanned: %+v", state)
	}
	httpError(t, h, "POST", "/api/operations/scan", `{"registry_revision":0}`, 409, "registry_conflict")
	scan := httpStart(t, h, "scan", `{"registry_revision":1}`)
	done := httpPoll(t, h, scan.ID, "completed")
	httpOpaqueID(t, done.ResultID)
	var inv Inventory
	httpJSON(t, httpRequest(t, h, "GET", "/api/inventories/"+done.ResultID, "", 200), &inv)
	if inv.ID != done.ResultID || !inv.Complete || inv.Stale || inv.RegistryRevision != 1 || len(inv.Models) != 12 {
		t.Fatalf("inventory=%+v", inv)
	}
	base := "/api/inventories/" + inv.ID + "/models/whisper-asr/files"
	var files []ModelFile
	cursor := ""
	seen := map[string]bool{}
	for n := 0; ; n++ {
		if n >= 3 {
			t.Fatal("pagination did not terminate")
		}
		var page struct {
			Files []ModelFile `json:"files"`
			Next  string      `json:"next_cursor"`
		}
		httpJSON(t, httpRequest(t, h, "GET", base+"?limit=1&cursor="+cursor, "", 200), &page)
		if len(page.Files) != 1 {
			t.Fatalf("page=%+v", page)
		}
		f := page.Files[0]
		httpOpaqueID(t, f.ID)
		if seen[f.ID] || f.Bytes != "3" || filepath.Dir(f.Path) != dir || f.Hash != nil {
			t.Fatalf("file=%+v", f)
		}
		seen[f.ID] = true
		files = append(files, f)
		if page.Next == "" {
			break
		}
		httpOpaqueID(t, page.Next)
		if page.Next == cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = page.Next
	}
	if len(files) != 3 {
		t.Fatalf("files=%+v", files)
	}
	paths := map[string]bool{}
	for _, f := range files {
		paths[filepath.Base(f.Path)] = true
	}
	if len(paths) != 3 || !paths["a.gguf"] || !paths["b.gguf"] || !paths["c.gguf"] {
		t.Fatalf("pagination lost files: %v", paths)
	}
	// Expire a real cursor under the package lock, without waiting 30 minutes.
	m.mu.Lock()
	p := m.cursors[cursor]
	p.createdAt = time.Now().Add(-cursorLifetime - time.Second)
	m.cursors[cursor] = p
	m.collect()
	m.mu.Unlock()
	httpError(t, h, "GET", base+"?limit=1&cursor="+cursor, "", 410, "cursor_invalid")
	verifyBody := httpBody(t, map[string]any{"inventory_id": inv.ID, "model_id": "whisper-asr", "file_ids": []string{files[0].ID}})
	for _, tc := range []struct {
		query, code string
		status      int
	}{
		{"?limit=no", "page_invalid", 422}, {"?limit=-1", "request_invalid", 422}, {"?limit=1001", "request_invalid", 422}, {"?cursor=missing", "cursor_invalid", 410},
	} {
		httpError(t, h, "GET", base+tc.query, "", tc.status, tc.code)
	}
	httpError(t, h, "GET", "/api/inventories/"+inv.ID+"/models/unknown/files", "", 404, "model_unknown")
	for _, ids := range [][]string{nil, {files[0].ID, files[0].ID}, {files[0].Path}} {
		code := "file_not_allowed"
		if len(ids) == 0 {
			code = "request_invalid"
		}
		httpError(t, h, "POST", "/api/operations/verify", httpBody(t, map[string]any{"inventory_id": inv.ID, "model_id": "whisper-asr", "file_ids": ids}), 422, code)
	}
	verify := httpStart(t, h, "verify", verifyBody)
	verified := httpPoll(t, h, verify.ID, "completed")
	if verified.CheckedEntries != 1 || verified.ProcessedBytes != "3" || len(verified.Hashes) != 1 {
		t.Fatalf("verify=%+v", verified)
	}
	hash := verified.Hashes[0]
	if hash.FileID != files[0].ID || hash.Status != "fingerprint_computed" || hash.Bytes != "3" || hash.SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" || hash.CheckedAt.IsZero() {
		t.Fatalf("hash=%+v", hash)
	}
	var all struct {
		Files []ModelFile `json:"files"`
		Next  string      `json:"next_cursor"`
	}
	httpJSON(t, httpRequest(t, h, "GET", base, "", 200), &all)
	found := false
	for _, f := range all.Files {
		if f.ID == hash.FileID {
			found = f.Hash != nil && f.Hash.SHA256 == hash.SHA256 && f.Hash.Status == hash.Status
		}
	}
	if !found {
		t.Fatal("verified hash not published in files response")
	}
	for n := 0; n < 2; n++ {
		var cancelled Operation
		httpJSON(t, httpRequest(t, h, "POST", "/api/operations/"+verify.ID+"/cancel", `{}`, 200), &cancelled)
		if httpBody(t, cancelled) != httpBody(t, verified) {
			t.Fatalf("cancel changed completed operation: %+v", cancelled)
		}
	}
	httpError(t, h, "DELETE", "/api/registrations/whisper-asr", `{"expected_revision":0}`, 409, "registry_conflict")
	registered.Registrations = nil // encoding/json reuses non-nil maps; decode this snapshot afresh.
	httpJSON(t, httpRequest(t, h, "DELETE", "/api/registrations/whisper-asr", `{"expected_revision":1}`, 200), &registered)
	if registered.Revision != 2 || len(registered.Registrations) != 0 {
		t.Fatalf("delete=%+v", registered)
	}
	for _, f := range files {
		b, err := os.ReadFile(f.Path)
		if err != nil || string(b) != "abc" {
			t.Fatalf("unregister changed source: %q %v", b, err)
		}
	}
	httpJSON(t, httpRequest(t, h, "GET", "/api/inventories/"+inv.ID, "", 200), &inv)
	if !inv.Stale {
		t.Fatal("unregister did not stale inventory")
	}
	httpError(t, h, "POST", "/api/operations/verify", verifyBody, 409, "inventory_stale")
	httpJSON(t, httpRequest(t, h, "GET", base, "", 200), &all)
	for _, f := range all.Files {
		if f.Hash != nil && f.Hash.Status != "stale" {
			t.Fatalf("old hash still valid: %+v", f.Hash)
		}
	}
	// A real replacement scan releases protection of the old result. Age only the
	// completed operation, then use collect rather than fabricating a tombstone.
	next := httpStart(t, h, "scan", `{"registry_revision":2}`)
	httpPoll(t, h, next.ID, "completed")
	m.mu.Lock()
	old := time.Now().Add(-31 * time.Minute)
	m.operations[scan.ID].FinishedAt = &old
	m.collect()
	m.mu.Unlock()
	httpError(t, h, "GET", "/api/inventories/"+inv.ID, "", 410, "inventory_gone")
	httpError(t, h, "GET", base, "", 410, "inventory_gone")
	httpError(t, h, "POST", "/api/operations/verify", verifyBody, 410, "inventory_gone")
	httpError(t, h, "GET", "/api/operations/"+scan.ID, "", 410, "operation_gone")
}
func TestHTTPStrictBodies(t *testing.T) {
	m := httpManager(t)
	h := newHandler(m, "secret", "127.0.0.1:18120", nil)
	for _, endpoint := range []struct{ method, path, valid, unknown, wrongType string }{
		{"POST", "/api/operations/scan", `{"registry_revision":0}`, `{"registry_revision":0,"extra":1}`, `{"registry_revision":"0"}`},
		{"POST", "/api/operations/verify", `{"inventory_id":"missing","model_id":"whisper-asr","file_ids":[]}`, `{"extra":1}`, `{"file_ids":"x"}`},
		{"POST", "/api/operations/verify-component", `{"component_id":"missing","version":"1"}`, `{"extra":1}`, `{"component_id":7}`},
		{"POST", "/api/operations/missing/cancel", `{}`, `{"extra":1}`, `{"extra":[]}`},
		{"PUT", "/api/registrations/whisper-asr", `{"path":"x","expected_revision":0}`, `{"extra":1}`, `{"path":7,"expected_revision":0}`},
		{"DELETE", "/api/registrations/whisper-asr", `{"expected_revision":0}`, `{"extra":1}`, `{"expected_revision":false}`},
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
		{"PUT", "/api/registrations/whisper-asr", `{"expected_revision":0,"path":" "}`, "request_invalid"},
		{"PUT", "/api/registrations/whisper-asr", `{"path":"x"}`, "request_invalid"},
		{"PUT", "/api/registrations/unknown", `{"expected_revision":0,"path":"x"}`, "model_unknown"},
		{"DELETE", "/api/registrations/whisper-asr", `{}`, "request_invalid"},
		{"DELETE", "/api/registrations/whisper-asr", `{"expected_revision":null}`, "request_invalid"},
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
			h := newHandler(m, "secret", "127.0.0.1:18120", nil)
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

func TestHTTPRoutesAndStaticWhitelist(t *testing.T) {
	m := httpManager(t)
	h := newHandler(m, "secret", "127.0.0.1:18120", nil)
	for _, path := range []string{"/", "/styles.css", "/app.js"} {
		get := httpRequest(t, h, "GET", path, "", 200)
		if get.Body.Len() == 0 {
			t.Fatalf("empty asset %s", path)
		}
		want := "text/html; charset=utf-8"
		if path == "/styles.css" {
			want = "text/css; charset=utf-8"
		}
		if path == "/app.js" {
			want = "text/javascript; charset=utf-8"
		}
		if get.Header().Get("Content-Type") != want {
			t.Fatalf("asset type=%q", get.Header().Get("Content-Type"))
		}
		if path == "/" && (strings.Contains(get.Body.String(), "__SESSION_TOKEN__") || !strings.Contains(get.Body.String(), "secret")) {
			t.Fatal("session token not substituted")
		}
		head := httpRequest(t, h, "HEAD", path, "", 200)
		if head.Body.Len() != 0 || head.Header().Get("Content-Type") != want {
			t.Fatal("invalid static HEAD")
		}
	}
	for _, path := range []string{"/catalog.go", "/web/", "/web/index.html", "/registrations.json", "/missing", "/app.js/", "/%61pp.js"} {
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
		{"POST", "/", "GET, HEAD", true}, {"DELETE", "/app.js", "GET, HEAD", true},
		{"PUT", "/api/state", "GET, HEAD", false}, {"PUT", "/api/operations/scan", "GET, HEAD, POST", false},
		{"GET", "/api/registrations/whisper-asr", "DELETE, PUT", false},
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

func TestHTTPCorruptRegistryReadOnly(t *testing.T) {
	// Corrupt storage before opening a separate manager: do not replace its store
	// or manufacture a mock error on the healthy lifecycle fixture.
	root := t.TempDir()
	for _, p := range []string{"sidecar", "models"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0700); err != nil {
			t.Fatal(err)
		}
	}
	store := t.TempDir()
	path := filepath.Join(store, "registrations.json")
	const corrupt = "{broken registry"
	if err := os.WriteFile(path, []byte(corrupt), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root, store, []string{filepath.Join(root, "models")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	h := newHandler(m, "secret", "127.0.0.1:18120", nil)
	var state State
	httpJSON(t, httpRequest(t, h, "GET", "/api/state", "", 200), &state)
	if !strings.Contains(state.StorageError, "registry_corrupt") {
		t.Fatalf("missing storage diagnosis: %+v", state)
	}
	httpError(t, h, "PUT", "/api/registrations/whisper-asr", httpBody(t, map[string]any{"expected_revision": 0, "path": filepath.Join(root, "models")}), 503, "registry_read_only")
	httpError(t, h, "DELETE", "/api/registrations/whisper-asr", `{"expected_revision":0}`, 503, "registry_read_only")
	b, err := os.ReadFile(path)
	if err != nil || string(b) != corrupt {
		t.Fatalf("read-only request changed storage: %q %v", b, err)
	}
}

func TestResolveRootStrict(t *testing.T) {
	if _, err := resolveRoot(t.TempDir()); err == nil {
		t.Fatal("arbitrary directory accepted")
	}
	m := httpManager(t)
	if _, err := resolveRoot(m.root); err != nil {
		t.Fatal(err)
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
