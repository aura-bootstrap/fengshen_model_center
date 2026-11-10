package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func dashboardFixture() Dashboard {
	return Dashboard{State: json.RawMessage(`{"catalog":[]}`), Inventory: json.RawMessage(`null`), LastCompleteInventory: json.RawMessage(`null`), Components: json.RawMessage(`[]`)}
}
func TestDashboardHostReadOnlyBindingAndLimits(t *testing.T) {
	h := &Host{now: time.Now, tokens: map[string]time.Time{}, desc: Descriptor{InstanceID: "bound", LoopbackAddress: "127.0.0.1:18120"}, cfg: HostConfig{Build: "test-build"}}
	token, err := h.issue()
	if err != nil {
		t.Fatal(err)
	}
	v := dashboardFixture()
	h.cfg.Dashboard = func() (Dashboard, error) { return v, nil }
	calls := 0
	h.cfg.ModelFiles = func(i, m, c string) (FilesPage, error) {
		calls++
		if i != strings.Repeat("a", 32) || m != "seed-vc" || c != "page-1" {
			t.Fatal(i, m, c)
		}
		return FilesPage{Files: json.RawMessage(`[]`), NextCursor: "page-2"}, nil
	}
	request := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:18120"+path, nil)
		r.Header.Set(ReadOnlyHeader, token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request("GET", DashboardPath)
	var out Dashboard
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Schema != 1 || out.Build != "test-build" || out.InstanceID != "bound" {
		t.Fatal(w.Code, w.Body.String())
	}
	query := "?inventory_id=" + strings.Repeat("a", 32) + "&model_id=seed-vc&cursor=page-1"
	w = request("GET", FilesPath+query)
	var page FilesPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || page.InstanceID != "bound" || page.Build != "test-build" || page.Schema != 1 || page.NextCursor != "page-2" || calls != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, path := range []string{HealthPath + "?", DependenciesPath + "?a=1", DashboardPath + "?", DashboardPath + "?x=1", DashboardPath + "/", "/api/state", "/api/operations/scan", FilesPath, FilesPath + query + "&limit=200", FilesPath + query + "&model_id=other", FilesPath + query + "&cursor=x", FilesPath + "?inventory_id=bad&model_id=seed-vc", FilesPath + "?inventory_id=" + strings.Repeat("a", 32) + "&model_id=../evil", FilesPath + query + "%2Fpath"} {
		if got := request("GET", path); got.Code != 403 {
			t.Errorf("%s: %d", path, got.Code)
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "HEAD"} {
		if request(method, DashboardPath).Code != 403 {
			t.Fatal(method)
		}
	}
	if calls != 1 {
		t.Fatal("invalid requests called callback")
	}
	v.State = json.RawMessage(`{"large":"` + strings.Repeat("x", MaxResponse) + `"}`)
	if request("GET", DashboardPath).Code != 200 {
		t.Fatal("used old small limit")
	}
	v.State = json.RawMessage(`{"large":"` + strings.Repeat("x", MaxDashboardResponse) + `"}`)
	if got := request("GET", DashboardPath); got.Code != 503 {
		t.Fatal("oversize", got.Code)
	}
	h.cfg.Dashboard = func() (Dashboard, error) { return Dashboard{}, errors.New("C:/private/token-secret") }
	if got := request("GET", DashboardPath); got.Code != 503 || strings.Contains(got.Body.String(), "private") {
		t.Fatal(got.Code, got.Body.String())
	}
}
func reportFixture() Report {
	v := Report{Dashboard: dashboardFixture(), ObservedAt: time.Now().UTC(), InventoryID: strings.Repeat("a", 32), RegistryRevision: 7, Complete: true,
		Summary: ModelSummary{Total: 3, Present: 1, Missing: 1, Unknown: 1, Recognized: 2, CompositionComplete: 1},
		Models:  json.RawMessage(`[{"id":"present","exists":true},{"id":"missing","exists":false},{"id":"unknown","exists":null}]`)}
	v.Schema = 1
	return v
}

func TestReportHostReadOnlyBindingAndLimits(t *testing.T) {
	h := &Host{now: time.Now, tokens: map[string]time.Time{}, desc: Descriptor{InstanceID: "bound", LoopbackAddress: "127.0.0.1:18120"}, cfg: HostConfig{Build: "test-build"}}
	token, err := h.issue()
	if err != nil {
		t.Fatal(err)
	}
	v := reportFixture()
	v.Schema, v.Build, v.InstanceID = 99, "untrusted", "untrusted"
	calls := 0
	h.cfg.Report = func() (Report, error) { calls++; return v, nil }
	request := func(method, path, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:18120"+path, nil)
		if auth != "" {
			r.Header.Set(ReadOnlyHeader, auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request("GET", ReportPath, token)
	var got Report
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Schema != 1 || got.Build != "test-build" || got.InstanceID != "bound" || validateReport(got) != nil || got.RegistryRevision != 7 || got.Summary != v.Summary || !got.ObservedAt.Equal(v.ObservedAt) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("report was cacheable")
	}
	for _, method := range []string{"POST", "PUT", "DELETE", "HEAD", "OPTIONS"} {
		if request(method, ReportPath, token).Code != 403 {
			t.Fatal(method)
		}
	}
	for _, path := range []string{ReportPath + "?", ReportPath + "?x=1", ReportPath + "/", "/api/integration/%72eport"} {
		if request("GET", path, token).Code != 403 {
			t.Fatal(path)
		}
	}
	for _, auth := range []string{"", strings.Repeat("0", 64)} {
		if request("GET", ReportPath, auth).Code != 403 {
			t.Fatal("unauthenticated report")
		}
	}
	if calls != 1 {
		t.Fatal("denied requests invoked callback", calls)
	}
	v.State = json.RawMessage(`{"large":"` + strings.Repeat("x", MaxResponse) + `"}`)
	if request("GET", ReportPath, token).Code != 200 {
		t.Fatal("report used small response limit")
	}
	v.State = json.RawMessage(`{"large":"` + strings.Repeat("x", MaxDashboardResponse) + `"}`)
	if request("GET", ReportPath, token).Code != 503 {
		t.Fatal("oversize report accepted")
	}
	v = reportFixture()
	v.Summary.Present++
	if request("GET", ReportPath, token).Code != 503 {
		t.Fatal("malformed summary repaired")
	}
	h.cfg.Report = func() (Report, error) { return Report{}, errors.New("C:/private/token-secret") }
	if w = request("GET", ReportPath, token); w.Code != 503 || strings.Contains(w.Body.String(), "private") {
		t.Fatal(w.Code, w.Body.String())
	}
	h.cfg.Report = nil
	if request("GET", ReportPath, token).Code != 503 {
		t.Fatal("missing callback accepted")
	}
}

func TestReportValidation(t *testing.T) {
	v := reportFixture()
	if err := validateReport(v); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Report){
		"schema":               func(v *Report) { v.Schema = 2 },
		"dashboard":            func(v *Report) { v.Components = json.RawMessage(`null`) },
		"observed":             func(v *Report) { v.ObservedAt = time.Time{} },
		"total":                func(v *Report) { v.Summary.Total++ },
		"present":              func(v *Report) { v.Summary.Present++ },
		"missing":              func(v *Report) { v.Summary.Missing = -1 },
		"unknown":              func(v *Report) { v.Summary.Unknown++ },
		"recognized negative":  func(v *Report) { v.Summary.Recognized = -1 },
		"recognized large":     func(v *Report) { v.Summary.Recognized = 4 },
		"composition negative": func(v *Report) { v.Summary.CompositionComplete = -1 },
		"composition large":    func(v *Report) { v.Summary.CompositionComplete = 4 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := v
			mutate(&bad)
			if validateReport(bad) == nil {
				t.Fatal("invalid report accepted")
			}
		})
	}
	for _, raw := range []string{`null`, `{}`, `[null]`, `[1]`, `[[]]`, `[{}]`, `[{"id":"a"}]`, `[{"id":"a","exists":0}]`, `[{"id":"a","exists":"true"}]`, `[{"id":"","exists":true}]`, `[{"id":"../a","exists":true}]`, `[{"id":"a","exists":true},{"id":"a","exists":false}]`, `[{"id":"a","exists":true}] trailing`, "[" + strings.TrimSuffix(strings.Repeat(`{"id":"a","exists":true},`, 65), ",") + "]"} {
		bad := v
		bad.Models = json.RawMessage(raw)
		if validateReport(bad) == nil {
			t.Fatal("invalid models accepted", raw)
		}
	}
	models := make([]map[string]any, 64)
	for i := range models {
		models[i] = map[string]any{"id": "model-" + strings.Repeat("a", i+1), "exists": nil}
	}
	v.Models, _ = json.Marshal(models)
	v.Summary = ModelSummary{Total: 64, Unknown: 64, Recognized: 64, CompositionComplete: 64}
	if err := validateReport(v); err != nil {
		t.Fatal("64 models", err)
	}
	models = append(models, map[string]any{"id": "extra", "exists": nil})
	v.Models, _ = json.Marshal(models)
	v.Summary = ModelSummary{Total: 65, Unknown: 65}
	if validateReport(v) == nil {
		t.Fatal("65 unique models accepted")
	}
	v.Models, v.Summary = json.RawMessage(`[]`), ModelSummary{}
	if err := validateReport(v); err != nil {
		t.Fatal("empty report", err)
	}
	c := &Client{cfg: Config{DataDir: t.TempDir()}}
	if _, err := c.Report(context.Background()); err == nil || c.cache.Expected != "" {
		t.Fatal("passive report performed initial verification", err)
	}
}

func TestFilesQueryStrict(t *testing.T) {
	base := "inventory_id=" + strings.Repeat("a", 32) + "&model_id=seed-vc"
	for _, query := range []string{base, base + "&cursor=", base + "&cursor=" + strings.Repeat("b", 128)} {
		if _, _, _, e := ParseFilesQuery(&url.URL{RawQuery: query}); e != nil {
			t.Fatal(query, e)
		}
	}
	for _, query := range []string{"", base + "&cursor=" + strings.Repeat("b", 129), base + "&url=x", base + "&cursor=%zz", base + ";x=1", base + "&inventory_id=x", base + "&cursor=..", base + "&cursor=%5C", base + "&model_id="} {
		if _, _, _, e := ParseFilesQuery(&url.URL{RawQuery: query}); e == nil {
			t.Fatal(query)
		}
	}
	if _, _, _, e := ParseFilesQuery(&url.URL{RawQuery: base, ForceQuery: true}); e == nil {
		t.Fatal("force query")
	}
}
func TestDashboardValidationAndFetch(t *testing.T) {
	v := dashboardFixture()
	v.Schema = 1
	if validateDashboard(v) != nil {
		t.Fatal("valid")
	}
	for _, mutate := range []func(*Dashboard){func(v *Dashboard) { v.State = nil }, func(v *Dashboard) { v.State = json.RawMessage(`{ }`) }, func(v *Dashboard) { v.State = json.RawMessage(`[]`) }, func(v *Dashboard) { v.Inventory = json.RawMessage(`[]`) }, func(v *Dashboard) { v.LastCompleteInventory = nil }, func(v *Dashboard) { v.Components = json.RawMessage(`null`) }} {
		bad := v
		mutate(&bad)
		if validateDashboard(bad) == nil {
			t.Fatal("invalid accepted", bad)
		}
	}
	for _, raw := range []string{`null`, `{}`, `[1]`, "[" + strings.TrimSuffix(strings.Repeat("{},", 201), ",") + "]"} {
		if validateFilesPage(FilesPage{Schema: 1, Files: json.RawMessage(raw)}) == nil {
			t.Fatal(raw)
		}
	}
	body, _ := json.Marshal(v)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != DashboardPath || r.Header.Get(ReadOnlyHeader) != "session" {
			t.Error("unbound request")
		}
		w.Write(body)
	}))
	defer srv.Close()
	h := handshakeResponse{Token: "session", Descriptor: Descriptor{LoopbackAddress: strings.TrimPrefix(srv.URL, "http://")}}
	var got Dashboard
	if err := fetchLimited(context.Background(), h, DashboardPath, &got, time.Second, MaxDashboardResponse); err != nil {
		t.Fatal(err)
	}
	if err := fetchLimited(context.Background(), h, DashboardPath, &got, time.Second, 10); err == nil {
		t.Fatal("limit ignored")
	}
	c := &Client{cfg: Config{DataDir: t.TempDir()}}
	if _, e := c.Dashboard(context.Background()); e == nil {
		t.Fatal("unconfirmed dashboard")
	}
	if _, e := c.ModelFiles(context.Background(), strings.Repeat("a", 32), "seed-vc", ""); e == nil {
		t.Fatal("unconfirmed files")
	}
	if c.cache.Expected != "" {
		t.Fatal("initial verification performed")
	}
}
