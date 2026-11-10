package integration

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type bindingBoundaryFake struct {
	opens, requests, closes int
	status                  int
	body                    string
	fail                    bool
	closeBounded            bool
	method, path, payload   string
}

func (f *bindingBoundaryFake) OpenBindingManagementSession(context.Context) (BindingManagementSession, error) {
	f.opens++
	return f, nil
}
func (f *bindingBoundaryFake) Request(_ context.Context, method, path string, body []byte) (*http.Response, error) {
	f.requests++
	f.method, f.path, f.payload = method, path, string(body)
	if f.fail {
		return nil, errors.New("C:/private/token stdout")
	}
	return &http.Response{StatusCode: f.status, Header: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(f.body)), ContentLength: int64(len(f.body))}, nil
}
func (f *bindingBoundaryFake) Close(ctx context.Context) error {
	f.closes++
	deadline, ok := ctx.Deadline()
	f.closeBounded = ok && time.Until(deadline) <= 3*time.Second && ctx.Err() == nil
	return nil
}
func TestBindingsSessionLifecycle(t *testing.T) {
	for _, fail := range []bool{false, true} {
		f := &bindingBoundaryFake{status: 200, body: `{"schema_version":1,"revision":0,"bindings":{}}`, fail: fail}
		_, err := NewBindingsClient(f).Bindings(context.Background())
		if (err != nil) != fail || f.opens != 1 || f.requests != 1 || f.closes != 1 || !f.closeBounded {
			t.Fatalf("lifecycle: %+v %v", f, err)
		}
		if err != nil && strings.Contains(err.Error(), "private") {
			t.Fatal(err)
		}
	}
}
func TestBindingsConflictNoRetry(t *testing.T) {
	f := &bindingBoundaryFake{status: 409, body: `{"error":{"code":"center_binding_stale","message":"C:/private token stdout"}}`}
	_, err := NewBindingsClient(f).DeleteBinding(context.Background(), "highlight.score", 7)
	var remote *BindingHTTPError
	if !errors.As(err, &remote) || remote.Status != 409 || remote.Code != "center_binding_stale" || f.requests != 1 || f.closes != 1 || f.payload != `{"expected_revision":7}` {
		t.Fatalf("%+v %v", f, err)
	}
}
func TestBindingStrictDelete(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"expected_revision":null}`, `{"expected_revision":-1}`, `{"expected_revision":1.0}`, `{"expected_revision":18446744073709551616}`, `{"expected_revision":0,"expected_revision":1}`, `{"Expected_revision":1}`, `{"expected_revision":1,"url":"x"}`, `{"expected_revision":1} {}`} {
		if _, err := DecodeBindingDelete([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`{"expected_revision":0}`, `{"expected_revision":18446744073709551615}`} {
		if _, err := DecodeBindingDelete([]byte(raw)); err != nil {
			t.Errorf("rejected %s: %v", raw, err)
		}
	}
}
func TestBindingsRejectInvalidSceneBeforeOpen(t *testing.T) {
	f := &bindingBoundaryFake{}
	for _, scene := range []string{"", "unknown", "highlight.score/other", "../highlight.score"} {
		if _, err := NewBindingsClient(f).DeleteBinding(context.Background(), scene, 0); err == nil {
			t.Fatal(scene)
		}
	}
	if f.opens != 0 {
		t.Fatal("invalid scene opened session")
	}
}
func TestBindingsSnapshotRejectsLeak(t *testing.T) {
	for _, body := range []string{`{"schema_version":1,"revision":0,"bindings":{},"path":"C:/private"}`, `{"schema_version":1,"revision":0,"bindings":{},"error":"C:/private token"}`, `{"schema_version":1,"revision":0,"bindings":null}`, `{"schema_version":1,"revision":0,"revision":1,"bindings":{}}`} {
		f := &bindingBoundaryFake{status: 200, body: body}
		if _, err := NewBindingsClient(f).Bindings(context.Background()); err == nil {
			t.Fatal(body)
		}
	}
}
