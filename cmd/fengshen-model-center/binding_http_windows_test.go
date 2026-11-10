//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
)

// Local DTOs exercise the public framed protocol, not integration test hooks.
type bindingPipeRequest struct {
	Schema       int                         `json:"schema"`
	Protocol     string                      `json:"protocol"`
	InstanceID   string                      `json:"instance_id"`
	RootKind     string                      `json:"root_kind"`
	RootIdentity string                      `json:"root_identity"`
	DataIdentity string                      `json:"data_identity"`
	Request      *integration.SessionRequest `json:"request,omitempty"`
}
type bindingPipeResponse struct {
	Token string                   `json:"token"`
	Grant integration.SessionGrant `json:"grant"`
}

func bindingPipeExchange(t *testing.T, pipe string, request bindingPipeRequest) bindingPipeResponse {
	t.Helper()
	// The single-instance server disconnects and re-arms between handshakes.
	deadline := time.Now().Add(10 * time.Second)
	var conn *os.File
	var err error
	for {
		conn, err = os.OpenFile(pipe, os.O_RDWR, 0)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.Errno(231)) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer conn.Close()
	// Closing the handle bounds a broken handshake without logging credentials.
	timer := time.AfterFunc(10*time.Second, func() { conn.Close() })
	defer timer.Stop()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(raw)))
	if _, err = io.Copy(conn, bytes.NewReader(append(size[:], raw...))); err != nil {
		t.Fatal("pipe request failed")
	}
	if _, err = io.ReadFull(conn, size[:]); err != nil {
		t.Fatal("pipe response failed")
	}
	n := binary.LittleEndian.Uint32(size[:])
	if n == 0 || n > 64*1024 {
		t.Fatal("invalid pipe frame size")
	}
	raw = make([]byte, n)
	if _, err = io.ReadFull(conn, raw); err != nil {
		t.Fatal("truncated pipe response")
	}
	var out bindingPipeResponse
	if json.Unmarshal(raw, &out) != nil || len(out.Token) != 64 {
		t.Fatal("invalid pipe credential response")
	}
	return out
}

func TestBindingHTTPWindowsActualPipe(t *testing.T) {
	root, store := t.TempDir(), t.TempDir()
	for _, dir := range []string{"sidecar", "models"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	m, err := NewManager(root, store, testManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	allowFixtureRoots(m, filepath.Join(root, "models"))
	defer m.Close()
	put(t, filepath.Join(root, "models", "ggml-small.bin"), "small")
	selection := bindingSelection(t, m, "highlight.transcribe", "whisper-batch", "whisper-asr")
	m.beforeScan = func(context.Context) { t.Error("binding HTTP triggered a scan") }
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(executable)
	const address = "127.0.0.1:18120"
	data := filepath.Join(t.TempDir(), "private")
	h, err := integration.NewHost(integration.HostConfig{
		Config:  integration.Config{InstallRoot: root, Root: root, RootKind: "installed", DataDir: data},
		Address: address, Build: "binding-test",
		EngineAllowlist: []integration.TrustedEngine{{Path: exe, SHA256: hex.EncodeToString(digest[:]), Edition: "admin", Management: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Register(); err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	descriptorBytes, err := os.ReadFile(filepath.Join(data, "instance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var descriptor integration.Descriptor
	if err = json.Unmarshal(descriptorBytes, &descriptor); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.ServeIPC(ctx) }()
	defer func() { cancel(); <-done }()
	req := bindingPipeRequest{Schema: 1, Protocol: integration.RuntimeProtocol, InstanceID: descriptor.InstanceID, RootKind: descriptor.RootKind, RootIdentity: descriptor.RootIdentity, DataIdentity: descriptor.DataIdentity,
		Request: &integration.SessionRequest{Permission: integration.PermissionManagement}}
	management := bindingPipeExchange(t, descriptor.PipeName, req)
	if management.Grant.Permission != integration.PermissionManagement || management.Grant.Edition != "admin" {
		t.Fatal("actual peer did not receive management grant")
	}
	req.Request = &integration.SessionRequest{Permission: integration.PermissionRuntime, TaskID: "binding-test-task", Capabilities: []string{"highlight.transcribe"}, Bindings: []integration.ExecutionBindingRef{{Scene: "highlight.transcribe", Revision: 1, SHA256: strings.Repeat("a", 64)}}}
	runtime := bindingPipeExchange(t, descriptor.PipeName, req)
	if runtime.Grant.Permission != integration.PermissionRuntime {
		t.Fatal("runtime grant missing")
	}
	req.Protocol, req.Request = integration.Protocol, nil
	read := bindingPipeExchange(t, descriptor.PipeName, req)
	handler := newHandler(m, address, h)
	path := integration.BindingsPath + "/highlight.transcribe"
	body, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, target string, raw []byte, headers http.Header) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://"+address+target, bytes.NewReader(raw))
		r.Header = headers.Clone()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		// Responses must not echo any credential, even on errors.
		for _, secret := range []string{management.Token, runtime.Token, read.Token} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("response exposed credential")
			}
		}
		return w
	}
	admin := http.Header{integration.ManagementHeader: []string{management.Token}}
	persisted := filepath.Join(store, "execution-bindings", "bindings.json")
	unchanged := func(run func()) {
		t.Helper()
		before := m.BindingSnapshot()
		disk, diskErr := os.ReadFile(persisted)
		run()
		after, afterErr := os.ReadFile(persisted)
		if !reflect.DeepEqual(before, m.BindingSnapshot()) || !bytes.Equal(disk, after) || os.IsNotExist(diskErr) != os.IsNotExist(afterErr) || (diskErr != nil && !os.IsNotExist(diskErr)) || (afterErr != nil && !os.IsNotExist(afterErr)) {
			t.Fatal("rejected/read request changed binding state or persistence")
		}
	}
	expect := func(method, target string, raw []byte, headers http.Header, status int) {
		t.Helper()
		if w := send(method, target, raw, headers); w.Code != status {
			t.Errorf("%s %s: status = %d, want %d", method, target, w.Code, status)
		}
	}
	unchanged(func() { expect("GET", integration.BindingsPath, nil, admin, 200) })
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		target := path
		if method == "GET" {
			target = integration.BindingsPath
		}
		for name, headers := range map[string]http.Header{
			"read":                  {integration.ReadOnlyHeader: []string{read.Token}},
			"runtime":               {integration.RuntimeHeader: []string{runtime.Token}},
			"runtime-in-management": {integration.ManagementHeader: []string{runtime.Token}},
			"read-in-management":    {integration.ManagementHeader: []string{read.Token}},
			"missing":               {}, "empty": {integration.ManagementHeader: []string{""}},
			"duplicate": {integration.ManagementHeader: []string{management.Token, management.Token}},
		} {
			t.Run(method+"/"+name, func(t *testing.T) { unchanged(func() { expect(method, target, body, headers, 403) }) })
		}
		for _, other := range []string{integration.RuntimeHeader, integration.ReadOnlyHeader, "Authorization", "Cookie"} {
			for _, value := range []string{"", "mixed"} {
				headers := admin.Clone()
				headers[other] = []string{value}
				unchanged(func() { expect(method, target, body, headers, 403) })
			}
		}
	}
	for _, tc := range []struct {
		method, target string
		status         int
	}{
		{"PUT", integration.BindingsPath + "/unknown", 403}, {"GET", integration.BindingsPath + "?x=1", 403},
		{"GET", integration.BindingsPath + "?", 403}, {"PUT", path + "?x=1", 403},
		{"PUT", integration.BindingsPath + "/highlight%2etranscribe", 404},
		{"POST", path, 403}, {"PATCH", path, 403}, {"HEAD", integration.BindingsPath, 403},
		{"GET", path, 403}, {"PUT", integration.BindingsPath, 403}, {"DELETE", integration.BindingsPath, 403},
	} {
		unchanged(func() { expect(tc.method, tc.target, body, admin, tc.status) })
	}
	for _, header := range []string{"Origin", "Sec-Fetch-Site"} {
		headers := admin.Clone()
		headers.Set(header, "cross-site")
		unchanged(func() { expect("PUT", path, body, headers, 403) })
	}
	t.Run("strict-json", func(t *testing.T) {
		valid := string(body)
		cases := map[string]string{
			"unknown":                    strings.Replace(valid, `"device":`, `"unknown":0,"device":`, 1),
			"duplicate":                  strings.Replace(valid, `"device":`, `"device":"cpu","device":`, 1),
			"escaped-duplicate":          strings.Replace(valid, `"device":`, `"d\u0065vice":"cpu","device":`, 1),
			"case-alias":                 strings.Replace(valid, `"device":`, `"DEVICE":`, 1),
			"case-collision":             strings.Replace(valid, `"device":`, `"DEVICE":"cpu","device":`, 1),
			"nested-file-alias":          strings.Replace(valid, `"file_id":`, `"FILE_ID":`, 1),
			"nested-component-alias":     strings.Replace(valid, `"version":`, `"VERSION":`, 1),
			"nested-component-collision": strings.Replace(valid, `"version":`, `"VERSION":"dev-local","version":`, 1),
			"nested-unknown":             strings.Replace(valid, `"file_id":`, `"unknown":0,"file_id":`, 1),
			"null":                       strings.Replace(valid, `"cpu"`, `null`, 1),
			"nested-null":                strings.Replace(valid, `"dev-local"`, `null`, 1),
			"null-array":                 strings.Replace(valid, `"files":[`, `"files":[null,`, 1),
			"root-null":                  `null`, "trailing": valid + ` {}`, "invalid-utf8": strings.Replace(valid, `cpu`, string([]byte{0xff}), 1),
		}
		for name, raw := range cases {
			t.Run(name, func(t *testing.T) { unchanged(func() { expect("PUT", path, []byte(raw), admin, 400) }) })
		}
		unchanged(func() { expect("PUT", path, append(body, bytes.Repeat([]byte(" "), 64*1024)...), admin, 413) })
		for _, raw := range []string{`{"EXPECTED_REVISION":0}`, `{"expected_revision":0,"EXPECTED_REVISION":0}`, `{"expected_revision":null}`, `{"expected_revision":0,"unknown":0}`} {
			unchanged(func() { expect("DELETE", path, []byte(raw), admin, 400) })
		}
	})
	expect("PUT", path, body, admin, 200)
	if s := m.BindingSnapshot(); s.Revision != 1 || len(s.Bindings) != 1 {
		t.Fatal("PUT did not persist binding")
	}
	unchanged(func() {
		w := send("GET", integration.BindingsPath, nil, admin)
		var got BindingSnapshot
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || !reflect.DeepEqual(got, m.BindingSnapshot()) {
			t.Fatal("GET snapshot mismatch")
		}
		expect("PUT", path, body, admin, 409)
		expect("DELETE", path, []byte(`{"expected_revision":0}`), admin, 409)
	})
	expect("DELETE", path, []byte(`{"expected_revision":1}`), admin, 200)
	if s := m.BindingSnapshot(); s.Revision != 2 || len(s.Bindings) != 0 {
		t.Fatal("DELETE did not remove binding")
	}
	disk, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Revision uint64                     `json:"revision"`
		Bindings map[string]json.RawMessage `json:"bindings"`
	}
	if json.Unmarshal(disk, &saved) != nil || saved.Revision != 2 || len(saved.Bindings) != 0 {
		t.Fatal("persistent deletion mismatch")
	}
}
