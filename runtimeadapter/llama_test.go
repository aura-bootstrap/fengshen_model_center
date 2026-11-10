package runtimeadapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/scoring"
	"github.com/aura-bootstrap/fengshen_model_center/bindings"
	"github.com/aura-bootstrap/fengshen_model_center/components"
	"github.com/aura-bootstrap/fengshen_model_center/integration"
	"github.com/aura-bootstrap/fengshen_model_center/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/runtimejobs"
	"github.com/aura-bootstrap/fengshen_model_center/runtimeprocess"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

var helperBlocked atomic.Bool
var helperHealthFail atomic.Bool
var helperInferenceFail atomic.Bool
var helperServiceUnavailable atomic.Bool
var helperHealthCount atomic.Int64
var helperRequestStarted atomic.Bool
var helperRequestReady = make(chan struct{})

// The pinned Go helper serves protocol fixtures only; no weights are opened.
func init() {
	if len(os.Args) < 3 || os.Args[1] != "--host" {
		return
	}
	port := 0
	for i := 1; i+1 < len(os.Args); i++ {
		if os.Args[i] == "--port" {
			port, _ = strconv.Atoi(os.Args[i+1])
		}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		os.Exit(3)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fixture/wait-request" {
			select {
			case <-helperRequestReady:
				w.WriteHeader(204)
			case <-r.Context().Done():
			}
			return
		}
		if r.URL.Path == "/fixture/block" {
			helperBlocked.Store(true)
			w.WriteHeader(204)
			return
		}
		if r.URL.Path == "/fixture/inference-fail" {
			helperInferenceFail.Store(true)
			w.WriteHeader(204)
			return
		}
		if r.URL.Path == "/fixture/service-unavailable" {
			helperServiceUnavailable.Store(true)
			w.WriteHeader(204)
			return
		}
		if r.URL.Path == "/fixture/health-fail" {
			helperHealthFail.Store(true)
			w.WriteHeader(204)
			return
		}
		if r.URL.Path == "/health" {
			if helperHealthFail.Load() {
				w.WriteHeader(503)
				return
			}
			w.Header().Set("X-Fixture-Health", strconv.FormatInt(helperHealthCount.Add(1), 10))
			_, _ = io.WriteString(w, `{"status":"ok"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+os.Getenv("LLAMA_API_KEY") {
			w.WriteHeader(401)
			return
		}
		if helperServiceUnavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"raw-response-secret"}`)
			return
		}
		if helperInferenceFail.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"raw-response-secret api_key=fixture-key http://secret.invalid/private W:/private/model.gguf"}`)
			return
		}
		if helperBlocked.Load() {
			if helperRequestStarted.CompareAndSwap(false, true) {
				close(helperRequestReady)
			}
			w.Header().Set("X-Fixture-Blocked", "1")
			w.WriteHeader(200)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
			return
		}
		var body struct {
			Messages       json.RawMessage `json:"messages"`
			ResponseFormat json.RawMessage `json:"response_format"`
			JSONSchema     json.RawMessage `json:"json_schema"`
			Grammar        string          `json:"grammar"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		vision := bytes.Contains(body.Messages, []byte(`"image_url"`))
		if vision {
			if body.ResponseFormat != nil || body.JSONSchema != nil ||
				!strings.Contains(body.Grammar, `root ::= "{" space "\"events\""`) ||
				!strings.Contains(body.Grammar, `score ::= ("0" ("." [0-9]+)? | "1" ("." "0"+)?) space`) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		} else if body.Grammar != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		content := `{"events":[]}`
		if vision {
			content, _ = scoring.ContractContent()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "helper", "object": "chat.completion", "created": 1, "model": "bound-model", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
	})
	_ = http.Serve(listener, handler)
	os.Exit(0)
}

func TestLlamaPlanRejectsUnknownProtocolBeforeBinary(t *testing.T) {
	for _, vision := range []bool{false, true} {
		for _, tc := range []struct {
			requested string
			declared  []string
		}{
			{"", []string{"llama.cpp.server.http/b12182"}},
			{"fengshen.llama/v1", []string{"fengshen.llama/v1"}},
			{"unknown/v1", []string{"unknown/v1", "llama.cpp.server.http/b12182"}},
			{"llama.cpp.server.http/b12182", nil},
			{"llama.cpp.server.http/b12182", []string{"unknown/v1"}},
		} {
			r := Resolved{Manifest: components.Manifest{Protocols: tc.declared}}
			r.Binding.Component.Protocol = tc.requested
			if _, _, err := llamaPlan(r, vision); err == nil || err.Error() != "center_protocol_unsupported" {
				t.Fatalf("vision=%v requested=%q declared=%v: %v", vision, tc.requested, tc.declared, err)
			}
		}
	}
}

func TestLlamaAVX2Registers(t *testing.T) {
	const leaf1 = uint32(1<<12 | 1<<20 | 1<<26 | 1<<27 | 1<<28 | 1<<29)
	const leaf7 = uint32(1<<5 | 1<<8)
	if !llamaAVX2Registers(leaf1, leaf7, 6, true, true) {
		t.Fatal("complete CPU and OS features rejected")
	}
	for _, bit := range []uint{12, 20, 26, 27, 28, 29} {
		if llamaAVX2Registers(leaf1&^(1<<bit), leaf7, 6, true, true) {
			t.Fatalf("missing leaf1 bit %d accepted", bit)
		}
	}
	for _, bit := range []uint{5, 8} {
		if llamaAVX2Registers(leaf1, leaf7&^(1<<bit), 6, true, true) {
			t.Fatalf("missing leaf7 bit %d accepted", bit)
		}
	}
	for _, xstate := range []uint64{0, 2, 4, 1 << 32} {
		if llamaAVX2Registers(leaf1, leaf7, xstate, true, true) {
			t.Fatalf("missing XState accepted: %x", xstate)
		}
	}
	for _, flags := range [][2]bool{{false, false}, {false, true}, {true, false}} {
		if llamaAVX2Registers(leaf1, leaf7, 6, flags[0], flags[1]) {
			t.Fatal("missing Windows feature accepted")
		}
	}
}

func TestLlamaPlanCPURejectsBeforeBinary(t *testing.T) {
	_, _, r, _ := llamaCacheExecutor(t)
	for _, version := range []string{"b12182-fengshen2-cpu-avx2", "b12182-fengshen2-cpu", "unknown"} {
		r.Manifest.Version = version
		r.Binding.Component.Version = version
		calls := 0
		b, args, err := llamaPlanWithCPU(*r, false, func() bool { calls++; return false })
		if !errors.Is(err, errLlamaBinding) || b != (runtimeprocess.VerifiedBinary{}) || args != nil {
			t.Fatalf("version %q not rejected before binary: %v", version, err)
		}
		if (calls == 1) != (version == "b12182-fengshen2-cpu-avx2") {
			t.Fatalf("unexpected CPU probe count %d for %s", calls, version)
		}
	}
	r.Manifest.Version = "b12182-fengshen2-cpu-avx2"
	r.Binding.Component.Version = r.Manifest.Version
	if _, args, err := llamaPlanWithCPU(*r, false, func() bool { return true }); err != nil || len(args) == 0 {
		t.Fatalf("explicit AVX2 profile rejected: %v", err)
	}
	r.Manifest.Version = "b12182-fengshen1-cpu"
	r.Binding.Component.Version = r.Manifest.Version
	if _, _, err := llamaPlanWithCPU(*r, false, func() bool { t.Fatal("baseline probed AVX2"); return false }); err != nil {
		t.Fatal(err)
	}
}

func TestLlamaHostCPUProbe(t *testing.T) {
	t.Logf("host AVX2 profile compatible: %v", llamaHostAVX2())
}

func TestLlamaVersionGate(t *testing.T) {
	for _, version := range []string{"b12182-fengshen1-cpu", "b12182-fengshen2-cpu-avx2", "", "b12182-fengshen2-cpu", "b12182-fengshen2-cpu-avx2-extra"} {
		for _, supported := range []bool{false, true} {
			want := version == "b12182-fengshen1-cpu" || version == "b12182-fengshen2-cpu-avx2" && supported
			if got := llamaVersionCompatible(version, supported); got != want {
				t.Fatalf("version=%q supported=%v got=%v", version, supported, got)
			}
		}
	}
}

func TestLlamaCompleteShards(t *testing.T) {
	root := t.TempDir()
	p := func(s string) string { return filepath.Join(root, s) }
	for _, tc := range []struct {
		paths []string
		ok    bool
	}{
		{[]string{p("model.gguf")}, true},
		{[]string{p("model-00002-of-00002.gguf"), p("model-00001-of-00002.gguf")}, true},
		{nil, false}, {[]string{p("model-00001-of-00002.gguf")}, false},
		{[]string{p("a-00001-of-00002.gguf"), p("b-00002-of-00002.gguf")}, false},
		{[]string{p("model.gguf"), p("model.gguf")}, false},
		{[]string{"relative.gguf"}, false}, {[]string{p("weights.bin")}, false},
	} {
		_, err := llamaModel(tc.paths)
		if (err == nil) != tc.ok {
			t.Fatalf("%v: %v", tc.paths, err)
		}
	}
}

func TestLlamaHelperProcess(t *testing.T) {
	for _, arg := range os.Args {
		if arg == "--llama-owned-helper" {
			_, _ = io.Copy(io.Discard, os.Stdin)
			os.Exit(0)
		}
	}
}

func ownedLlamaHelper(t *testing.T) (*runtimeprocess.Supervisor, *runtimeprocess.Process, runtimeprocess.VerifiedBinary) {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	b, err := runtimeprocess.VerifyBinary(path, hex.EncodeToString(sum[:]), "test-helper", "binding")
	if errors.Is(err, runtimeprocess.ErrUnsupported) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	s := &runtimeprocess.Supervisor{}
	p, err := s.Start(context.Background(), b, runtimeprocess.Options{Args: []string{"-test.run=^TestLlamaHelperProcess$", "--", "--llama-owned-helper"}, MaxStdinBytes: 1, MaxStdoutBytes: 4096, MaxStderrBytes: 4096, BufferBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, p, b
}

// The HTTP fixture is readiness only, not a mock inference success. An actual
// owned process is required before the adapter may send even this health GET.
func TestLlamaReadinessRequiresOwnedIdentity(t *testing.T) {
	_, p, b := ownedLlamaHelper(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; _, _ = io.WriteString(w, `{"status":"ok"}`) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := llamaReady(ctx, p, b, server.URL); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	_ = p.Close()
	if err := llamaReady(ctx, p, b, server.URL); err == nil {
		t.Fatal("accepted dead process")
	}
	if calls != 1 {
		t.Fatal("contacted service without live identity")
	}
}

func TestLlamaTransportNoRedirectOrProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	c := llamaHTTP()
	defer c.CloseIdleConnections()
	resp, err := c.Get(redirect.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 || calls != 0 || c.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("unsafe HTTP transport")
	}
}

func TestLlamaUsesSupervisorEnvironmentAPI(t *testing.T) {
	s := &runtimeprocess.Supervisor{}
	_, err := startLlama(context.Background(), s, runtimeprocess.VerifiedBinary{}, nil, strings.Repeat("a", 64))
	if !errors.Is(err, runtimeprocess.ErrIdentity) {
		t.Fatal(err)
	}
}

func TestLlamaRejectsUnsupportedDeviceAndComposition(t *testing.T) {
	r := Resolved{Binding: bindings.Binding{Device: "cuda"}}
	if _, _, err := llamaPlan(r, false); err == nil {
		t.Fatal("ignored device")
	}
	r.Binding.Device = "cpu"
	r.Models = []ModelFile{{ModelID: "local-text", Path: filepath.Join(t.TempDir(), "weights.gguf")}}
	if _, _, err := llamaPlan(r, false); err == nil {
		t.Fatal("accepted unpinned manifest")
	}
}

func TestLlamaVisionExactPairPlan(t *testing.T) {
	pairs := [][2]string{
		{"SmolVLM-500M-Instruct-Q8_0.gguf", "mmproj-SmolVLM-500M-Instruct-f16.gguf"},
		{"Qwen3-VL-2B-Instruct-Q8_0.gguf", "mmproj-Qwen3-VL-2B-Instruct-Q8_0.gguf"},
	}
	for i, pair := range pairs {
		for _, mode := range []string{"complete", "reverse-order", "mixed-projector", "mixed-model", "split-directory", "missing-model", "missing-projector", "duplicate-model", "duplicate-projector", "both-pairs", "arbitrary-model", "arbitrary-projector", "wrong-quant", "wrong-model-id", "cuda", "unlocked-source", "unlocked-hash", "unknown-protocol", "invalid-manifest", "avx2-unavailable"} {
			t.Run(pair[0]+"/"+mode, func(t *testing.T) {
				_, _, resolved, _ := llamaCacheExecutor(t)
				r := *resolved
				root := filepath.Dir(r.Models[0].Path)
				model, projector := filepath.Join(root, pair[0]), filepath.Join(root, pair[1])
				switch mode {
				case "mixed-projector":
					projector = filepath.Join(root, pairs[1-i][1])
				case "mixed-model":
					model = filepath.Join(root, pairs[1-i][0])
				case "split-directory":
					projector = filepath.Join(root, "other", pair[1])
				case "arbitrary-model":
					model = filepath.Join(root, "other.gguf")
				case "arbitrary-projector":
					projector = filepath.Join(root, "mmproj-other.gguf")
				case "wrong-quant":
					model = strings.Replace(model, "Q8_0", "Q4_K_M", 1)
				case "cuda":
					r.Binding.Device = "cuda"
				case "unlocked-source":
					r.Manifest.BuildInputs[0].Version = strings.Repeat("0", 40)
				case "unlocked-hash":
					r.Manifest.BuildInputs[0].SHA256 = strings.Repeat("0", 64)
				case "unknown-protocol":
					r.Binding.Component.Protocol = "unknown/v1"
				case "invalid-manifest":
					r.Manifest.Arch = "unknown"
				case "avx2-unavailable":
					r.Manifest.Version = "b12182-fengshen2-cpu-avx2"
					r.Binding.Component.Version = r.Manifest.Version
				}
				r.Models = []ModelFile{{ModelID: "local-vision", Path: model}, {ModelID: "local-vision", Path: projector}}
				switch mode {
				case "reverse-order":
					r.Models[0], r.Models[1] = r.Models[1], r.Models[0]
				case "missing-model":
					r.Models = r.Models[1:]
				case "missing-projector":
					r.Models = r.Models[:1]
				case "duplicate-model":
					r.Models = append(r.Models, r.Models[0])
				case "duplicate-projector":
					r.Models = append(r.Models, r.Models[1])
				case "both-pairs":
					for _, name := range pairs[1-i] {
						r.Models = append(r.Models, ModelFile{ModelID: "local-vision", Path: filepath.Join(root, name)})
					}
				case "wrong-model-id":
					r.Models[0].ModelID = "local-text"
				}
				binary, args, err := llamaPlanWithCPU(r, true, func() bool { return false })
				valid := mode == "complete" || mode == "reverse-order"
				if !valid {
					if err == nil || binary != (runtimeprocess.VerifiedBinary{}) || args != nil {
						t.Fatalf("invalid pair/gate accepted: %v", err)
					}
					return
				}
				want := []string{"--host", "127.0.0.1", "--offline", "--no-webui", "--no-warmup", "--parallel", "1", "--ctx-size", "4096", "--n-gpu-layers", "0", "--threads", "4", "--no-mmproj-offload", "--model", model, "--mmproj", projector}
				if err != nil || binary == (runtimeprocess.VerifiedBinary{}) || !reflect.DeepEqual(args, want) {
					t.Fatalf("exact pair plan failed: args=%v err=%v", args, err)
				}
			})
		}
	}
}

func TestLlamaDeliveredManifestExplicit(t *testing.T) {
	root := os.Getenv("FENGSHEN_LLAMA_COMPONENT_TEST_ROOT")
	if root == "" {
		t.Skip("explicit delivered-component test; no weights or inference implied")
	}
	f, err := os.Open(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := components.ParseManifest(f)
	if err != nil {
		t.Fatal(err)
	}
	r := Resolved{Manifest: m, ComponentRoot: root, Binding: bindings.Binding{Device: "cpu"}, Models: []ModelFile{{ModelID: "local-text", Path: filepath.Join(t.TempDir(), "unloaded.gguf")}}}
	r.Binding.Component.ID = m.ComponentID
	r.Binding.Component.Version = m.Version
	r.Binding.Component.Protocol = "llama.cpp.server.http/b12182"
	b, args, err := llamaPlan(r, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) == 0 || b == (runtimeprocess.VerifiedBinary{}) {
		t.Fatal("missing verified internal server")
	}
	// This verifies the real manifest-pinned server, not the metadata entry.
	if m.Entry == "internal/llama-server.exe" {
		t.Fatal("fixture unexpectedly uses server metadata entry")
	}
}

func TestLlamaInferenceFailureSafeDiagnostic(t *testing.T) {
	secret := "raw-response-secret api_key=fixture-key http://secret.invalid/private W:/private/model.gguf"
	for _, tc := range []struct {
		name string
		err  error
		want llamaInferenceError
	}{
		{"timeout", fmt.Errorf("%s: %w", secret, &scoring.Error{Kind: scoring.ErrorKindTimeout, Code: scoring.CodeProviderTimeout, Attempts: 3}), llamaInferenceError{kind: scoring.ErrorKindTimeout, code: scoring.CodeProviderTimeout, attempts: 3}},
		{"http", &scoring.Error{Kind: scoring.ErrorKindHTTP, Code: scoring.CodeProviderUnavailable, StatusCode: 503, Attempts: 3}, llamaInferenceError{kind: scoring.ErrorKindHTTP, code: scoring.CodeProviderUnavailable, statusCode: 503, attempts: 3}},
		{"protocol", scoring.NewStableError(scoring.ErrorKindProtocol, secret), llamaInferenceError{kind: scoring.ErrorKindProtocol, code: scoring.CodeResponseInvalid}},
		{"json", scoring.NewStableError(scoring.ErrorKindJSON, secret), llamaInferenceError{kind: scoring.ErrorKindJSON, code: scoring.CodeResponseInvalid}},
		{"unknown-kind", &scoring.Error{Kind: scoring.ErrorKind(secret), Code: scoring.CodeResponseInvalid}, llamaInferenceError{code: scoring.CodeResponseInvalid}},
		{"unknown-code", &scoring.Error{Kind: scoring.ErrorKindProtocol, Code: secret}, llamaInferenceError{kind: scoring.ErrorKindProtocol}},
		{"untyped", errors.New(secret), llamaInferenceError{}},
		{"nil", nil, llamaInferenceError{}},
		{"typed-nil", (*scoring.Error)(nil), llamaInferenceError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := llamaInferenceFailure(tc.err)
			if *err != tc.want {
				t.Fatalf("unexpected safe classification: %+v", err)
			}
			if err.Error() != "center_llama_inference_failed" {
				t.Fatal("changed public error")
			}
			var classified *scoring.Error
			if errors.Unwrap(err) != nil || errors.As(err, &classified) {
				t.Fatal("retained original scoring error")
			}
			raw, marshalErr := json.Marshal(err)
			if marshalErr != nil || string(raw) != "{}" {
				t.Fatal("local diagnostic crossed JSON boundary")
			}
			formatted := fmt.Sprintf("%s %v %+v %#v %q", err, err, err, err, err)
			for _, forbidden := range []string{"raw-response-secret", "fixture-key", "secret.invalid", "W:/private/model.gguf"} {
				if strings.Contains(formatted, forbidden) {
					t.Fatal("diagnostic retained sensitive text")
				}
			}
		})
	}
	original := &scoring.Error{Kind: scoring.ErrorKindTimeout, Code: scoring.CodeProviderTimeout, Attempts: 3}
	copied := llamaInferenceFailure(original)
	original.Kind, original.Code, original.Attempts = scoring.ErrorKind(secret), secret, 99
	if copied.kind != scoring.ErrorKindTimeout || copied.code != scoring.CodeProviderTimeout || copied.attempts != 3 {
		t.Fatal("diagnostic retained mutable scoring error")
	}
}

func TestLlamaCopiesLocalParseDiagnostic(t *testing.T) {
	const secret = "token=hidden-secret https://private.invalid/x W:/private/model.gguf"
	for _, reason := range []string{"length", secret} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "not-json " + secret}, "finish_reason": reason}}})
		}))
		client, err := scoring.NewClient(scoring.Config{Provider: scoring.ProviderVolcano, BaseURL: server.URL, APIKey: "fixture-key", Model: "fixture-model", TimeoutSec: 2})
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		_, err = client.ScoreEvents(context.Background(), scoring.ContractCandidate())
		server.Close()
		diagnostic := llamaInferenceFailure(err)
		wantReason := scoring.FinishReason("")
		if reason == "length" {
			wantReason = scoring.FinishLength
		}
		if diagnostic.parseCategory != scoring.ParseInvalidJSON || diagnostic.finishReason != wantReason || diagnostic.contentLength != len("not-json "+secret) || diagnostic.attempts != 1 {
			t.Fatal("local parse metadata not copied safely")
		}
		var classified *scoring.Error
		if errors.Unwrap(diagnostic) != nil || errors.As(diagnostic, &classified) {
			t.Fatal("retained scoring cause")
		}
		raw, marshalErr := json.Marshal(diagnostic)
		if marshalErr != nil || string(raw) != "{}" || diagnostic.Error() != "center_llama_inference_failed" {
			t.Fatal("changed adapter wire boundary")
		}
		formatted := fmt.Sprintf("%s %v %+v %#v", diagnostic, diagnostic, diagnostic, diagnostic)
		for _, forbidden := range []string{"hidden-secret", "private.invalid", "W:/private", "fixture-key"} {
			if strings.Contains(formatted, forbidden) {
				t.Fatal("adapter retained sensitive text")
			}
		}
	}
}

func TestLlamaCopiesLocalEventDiagnostic(t *testing.T) {
	const secret = "raw-event-secret token=hidden-secret https://private.invalid/x W:/private/model.gguf"
	for _, tc := range []struct {
		name     string
		category scoring.EventCategory
		mutate   func(map[string]any)
	}{
		{"decode", scoring.EventDecode, func(e map[string]any) { e["hook_score"] = secret }},
		{"score", scoring.EventScore, func(e map[string]any) { delete(e, "hook_score") }},
		{"score-null", scoring.EventScore, func(e map[string]any) { e["hook_score"] = nil }},
		{"score-range", scoring.EventScore, func(e map[string]any) { e["hook_score"] = 1.0123456789 }},
		{"title", scoring.EventTitle, func(e map[string]any) { e["title"] = " " }},
		{"reason", scoring.EventReason, func(e map[string]any) { e["reason"] = " " }},
		{"tags", scoring.EventTags, func(e map[string]any) { e["tags"] = nil }},
		{"bool", scoring.EventBool, func(e map[string]any) { delete(e, "include_in_compilation") }},
		{"recommended-pair", scoring.EventRecommendedPair, func(e map[string]any) { e["recommended_start_sec"] = 1.0 }},
		{"recommended-range", scoring.EventRecommendedRange, func(e map[string]any) { e["recommended_start_sec"], e["recommended_end_sec"] = 2.0, 1.0 }},
		{"object", scoring.EventObject, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(scoring.ContractEvents()[0])
			if err != nil {
				t.Fatal(err)
			}
			var event map[string]any
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			event["title"], event["reason"] = secret, secret
			if tc.mutate != nil {
				tc.mutate(event)
			} else {
				event = nil
			}
			content, err := json.Marshal(map[string]any{"events": []any{scoring.ContractEvents()[0], event, nil}})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": string(content)}, "finish_reason": "stop"}}})
			}))
			defer server.Close()
			client, err := scoring.NewConstrainedLocalVisionClient(scoring.LocalVisionConfig{BaseURL: server.URL, APIKey: "fixture-key", Model: "bound-model", TimeoutSec: 2})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ScoreEvents(context.Background(), scoring.ContractCandidate())
			var original *scoring.Error
			if !errors.As(err, &original) {
				t.Fatal("expected typed scoring event failure")
			}
			diagnostic := llamaInferenceFailure(err)
			if diagnostic.eventCategory != tc.category || diagnostic.eventCount != 3 || diagnostic.invalidEventCount != 2 || diagnostic.parseCategory != "" || diagnostic.finishReason != scoring.FinishStop || diagnostic.contentLength != len(content) || diagnostic.firstInvalidEventIndex != 2 {
				t.Fatal("event diagnostic not copied")
			}
			wantField, wantReason := scoring.ScoreField(""), scoring.ScoreReason("")
			if tc.category == scoring.EventScore {
				wantField, wantReason = scoring.ScoreFieldHook, scoring.ScoreReasonMissing
				if tc.name == "score-null" {
					wantReason = scoring.ScoreReasonNull
				} else if tc.name == "score-range" {
					wantReason = scoring.ScoreReasonOutOfRange
				}
			}
			if diagnostic.scoreField != wantField || diagnostic.scoreReason != wantReason {
				t.Fatal("score diagnostic not copied or attached to a non-score failure")
			}
			original.Kind, original.Code, original.Attempts = scoring.ErrorKind(secret), secret, 99
			if diagnostic.kind != scoring.ErrorKindProtocol || diagnostic.code != scoring.CodeResponseInvalid || diagnostic.attempts != 1 {
				t.Fatal("retained mutable scoring error")
			}
			encoded, marshalErr := json.Marshal(diagnostic)
			if marshalErr != nil || string(encoded) != "{}" || errors.Unwrap(diagnostic) != nil || errors.As(diagnostic, &original) {
				t.Fatal("event diagnostic crossed adapter boundary")
			}
			formatted := fmt.Sprintf("%s %v %+v %#v %q", diagnostic, diagnostic, diagnostic, diagnostic, diagnostic)
			for _, forbidden := range []string{"raw-event-secret", "hidden-secret", "private.invalid", "W:/private", "fixture-key"} {
				if strings.Contains(formatted, forbidden) {
					t.Fatal("event diagnostic retained sensitive text")
				}
			}
		})
	}
}

func TestLlamaResultFailureClassifiesTypedEventMutations(t *testing.T) {
	const secret = "raw-event-secret token=hidden-secret https://private.invalid/x W:/private/model.gguf"
	str := func(s string) *string { return &s }
	num := func(n float64) *float64 { return &n }
	for _, tc := range []struct {
		name     string
		category string
		mutate   func(*scoring.RawEvent, *runtimejobs.ScoreInput)
	}{
		{"title-limit", "text", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.Title = strings.Repeat("x", 8193) }},
		{"reason-limit", "text", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.Reason = strings.Repeat("x", 8193) }},
		{"blank-title", "text", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.Title = " " }},
		{"invalid-utf8", "text", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.Title = string([]byte{0xff}) }},
		{"tags-null", "tags", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.Tags = nil }},
		{"tags-limit", "tags", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.Tags = make([]string, 129) }},
		{"tag-limit", "tags", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.Tags = []string{strings.Repeat("x", 8193)} }},
		{"tag-blank", "tags", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.Tags = []string{" "} }},
		{"id-start-only", "id_pair", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.EndUnitID = nil }},
		{"id-end-only", "id_pair", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.StartUnitID = nil }},
		{"id-start-unknown", "id_not_eligible", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.StartUnitID = str(secret) }},
		{"id-end-unknown", "id_not_eligible", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.EndUnitID = str(secret) }},
		{"context-id", "id_not_eligible", func(e *scoring.RawEvent, s *runtimejobs.ScoreInput) {
			s.ContextUnits = []runtimejobs.SpeechUnit{{ID: "context-only", StartSec: 0, EndSec: 1, Text: secret}}
			e.StartUnitID = str("context-only")
		}},
		{"id-reverse", "id_reverse", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) {
			e.StartUnitID, e.EndUnitID = e.EndUnitID, e.StartUnitID
		}},
		{"seconds-start-only", "seconds_range", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.RecommendedStartSec = num(1) }},
		{"seconds-end-only", "seconds_range", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.RecommendedEndSec = num(2) }},
		{"seconds-reverse", "seconds_range", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) {
			e.RecommendedStartSec, e.RecommendedEndSec = num(2), num(1)
		}},
		{"seconds-negative", "seconds_range", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) {
			e.RecommendedStartSec, e.RecommendedEndSec = num(-1), num(2)
		}},
		{"seconds-day-limit", "seconds_range", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) {
			e.RecommendedStartSec, e.RecommendedEndSec = num(1), num(86401)
		}},
		{"seconds-before", "outside_candidate", func(e *scoring.RawEvent, s *runtimejobs.ScoreInput) {
			s.StartSec = 1
			e.RecommendedStartSec, e.RecommendedEndSec = num(0), num(2)
		}},
		{"seconds-after", "outside_candidate", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) {
			e.RecommendedStartSec, e.RecommendedEndSec = num(1), num(61)
		}},
		{"score", "unknown", func(e *scoring.RawEvent, _ *runtimejobs.ScoreInput) { e.HighlightScore = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := scoring.ContractCandidate()
			for i := range candidate.EligibleUnits {
				candidate.EligibleUnits[i].Words = []domain.TranscriptWord{}
			}
			inputRaw, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			input, err := runtimejobs.ScoreInputFromJSON(inputRaw)
			if err != nil {
				t.Fatal(err)
			}
			events := scoring.ContractEvents()
			events[0].Title, events[0].Reason, events[0].Tags = secret, secret, []string{secret}
			raw, err := json.Marshal(events)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runtimejobs.RawEventsFromJSON(raw, input); err != nil || llamaResultEventCategory(events[0], input) != "" {
				t.Fatal("baseline event is not valid")
			}
			tc.mutate(events[0], &input)
			raw, err = json.Marshal(events)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runtimejobs.RawEventsFromJSON(raw, input); err == nil && tc.name != "invalid-utf8" {
				t.Fatal("mutation must still be rejected by runtime")
			}
			before := string(raw)
			inputBefore, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			diagnostic := llamaResultFailure(events, input)
			if *diagnostic != (llamaResultError{eventCategory: tc.category, eventCount: 2, invalidEventCount: 1}) {
				t.Fatal("wrong typed result classification")
			}
			after, _ := json.Marshal(events)
			inputAfter, _ := json.Marshal(input)
			if string(after) != before || !bytes.Equal(inputBefore, inputAfter) {
				t.Fatal("result diagnostic mutated typed events or input")
			}
			var found *llamaResultError
			if !errors.As(fmt.Errorf("boundary: %w", diagnostic), &found) || found != diagnostic || errors.Unwrap(diagnostic) != nil || diagnostic.Error() != "center_result_invalid" {
				t.Fatal("unsafe result error boundary")
			}
			encoded, err := json.Marshal(diagnostic)
			if err != nil || string(encoded) != "{}" {
				t.Fatal("result diagnostic crossed JSON boundary")
			}
			events[0].Title, input.EligibleUnits[0].ID = secret+"changed", secret+"changed"
			formatted := fmt.Sprintf("%s %v %+v %#v %q", diagnostic, diagnostic, diagnostic, diagnostic, diagnostic)
			for _, forbidden := range []string{"raw-event-secret", "hidden-secret", "private.invalid", "W:/private", "changed"} {
				if strings.Contains(formatted, forbidden) {
					t.Fatal("result diagnostic retained event or input text")
				}
			}
		})
	}
	valid := scoring.ContractEvents()[0]
	valid.StartUnitID, valid.EndUnitID = nil, nil
	limit := make([]*scoring.RawEvent, 129)
	for i := range limit {
		limit[i] = valid
	}
	for _, tc := range []struct {
		name     string
		events   []*scoring.RawEvent
		category string
		invalid  int
	}{
		{"nil-slice", nil, "nil", 0},
		{"nil-slot", []*scoring.RawEvent{nil}, "nil", 1},
		{"mixed-slots", []*scoring.RawEvent{valid, nil, nil, valid}, "nil", 2},
		{"limit", limit, "events_limit", 0},
		{"unknown", []*scoring.RawEvent{}, "unknown", 0},
		{"valid-fallback", []*scoring.RawEvent{valid}, "unknown", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diagnostic := llamaResultFailure(tc.events, runtimejobs.ScoreInput{})
			if diagnostic.eventCategory != tc.category || diagnostic.eventCount != len(tc.events) || diagnostic.invalidEventCount != tc.invalid {
				t.Fatal("wrong result shape classification")
			}
		})
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		event := scoring.ContractEvents()[0]
		event.StartUnitID, event.EndUnitID = nil, nil
		event.RecommendedStartSec, event.RecommendedEndSec = num(0), num(value)
		if llamaResultEventCategory(event, runtimejobs.ScoreInput{}) != "seconds_range" {
			t.Fatal("non-finite seconds not classified")
		}
	}
}

func TestLlamaInferenceFailureAdapterBoundary(t *testing.T) {
	e, req, _, releases := llamaCacheExecutor(t)
	if _, err := e.Execute(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	helperControl(t, e.llama.base, "/fixture/inference-fail")
	_, err := e.Execute(context.Background(), req, nil)
	var diagnostic *llamaInferenceError
	if !errors.As(err, &diagnostic) {
		t.Fatal("adapter discarded typed inference classification")
	}
	if *diagnostic != (llamaInferenceError{kind: scoring.ErrorKindUnauthorized, code: scoring.CodeUnauthorized, statusCode: 401, attempts: 1}) {
		t.Fatalf("unexpected safe classification: %+v", diagnostic)
	}
	if err.Error() != "center_llama_inference_failed" || errors.Unwrap(err) != nil {
		t.Fatal("changed public error boundary")
	}
	raw, marshalErr := json.Marshal(err)
	if marshalErr != nil || string(raw) != "{}" {
		t.Fatal("local diagnostic crossed JSON boundary")
	}
	if e.llama.process != nil || releases.Load() != 2 {
		t.Fatal("failed inference retained owned process or model lease")
	}
}

func TestLlamaTextDoesNotRetryServiceUnavailable(t *testing.T) {
	e, req, _, releases := llamaCacheExecutor(t)
	if _, err := e.Execute(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	helperControl(t, e.llama.base, "/fixture/service-unavailable")
	_, err := e.Execute(context.Background(), req, nil)
	var diagnostic *llamaInferenceError
	if !errors.As(err, &diagnostic) || diagnostic.kind != scoring.ErrorKindHTTP || diagnostic.statusCode != 503 || diagnostic.attempts != 1 {
		t.Fatalf("retryable HTTP failure was not single-attempt: %v", err)
	}
	if e.llama.process != nil || releases.Load() != 2 {
		t.Fatal("failed inference retained owned process or model lease")
	}
}

func TestLlamaOwnedVisionRequestsBoundedGrammar(t *testing.T) {
	e, req, resolved, _ := llamaCacheExecutor(t)
	root := filepath.Dir(resolved.Models[0].Path)
	model := filepath.Join(root, "SmolVLM-500M-Instruct-Q8_0.gguf")
	projector := filepath.Join(root, "mmproj-SmolVLM-500M-Instruct-f16.gguf")
	for _, path := range []string{model, projector} {
		if err := os.WriteFile(path, []byte("protocol fixture, not GGUF weights"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	resolved.Models = []ModelFile{{ModelID: "local-vision", Path: model}, {ModelID: "local-vision", Path: projector}}
	req.Binding.Scene = "highlight.vision"
	resolved.Binding.Scene, resolved.Binding.Ref = req.Binding.Scene, req.Binding
	input := *req.Body.Score
	input.AnalysisChain, input.AudioFeatures = "", nil
	var picture bytes.Buffer
	if err := jpeg.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	asset, err := e.assets.Upload(context.Background(), req.TaskID, "fixture-session", runtimeassets.Image, int64(picture.Len()), &picture)
	if err != nil {
		t.Fatal(err)
	}
	req.Body.Score = nil
	req.Body.Vision = &runtimejobs.VisionInput{AssetID: asset.ID(), Candidate: input}
	result, err := e.Execute(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("owned vision HTTP fixture rejected request: %v", err)
	}
	if len(result.Events) != 2 || result.Events[0].Title != scoring.ContractEvents()[0].Title {
		t.Fatal("owned vision did not preserve non-empty fixture events")
	}
	helperControl(t, e.llama.base, "/fixture/service-unavailable")
	_, err = e.Execute(context.Background(), req, nil)
	var diagnostic *llamaInferenceError
	if !errors.As(err, &diagnostic) || diagnostic.kind != scoring.ErrorKindHTTP || diagnostic.statusCode != 503 || diagnostic.attempts != 1 {
		t.Fatalf("vision retryable HTTP failure was not single-attempt: %v", err)
	}
	if e.llama.process != nil {
		t.Fatal("failed vision inference retained owned process")
	}
}

func TestLlamaHarnessIndependentOptIns(t *testing.T) {
	t.Setenv("FENGSHEN_LLAMA_REAL_COMPONENT_ROOT", "")
	t.Setenv("FENGSHEN_LLAMA_REAL_INFERENCE", "1")
	t.Setenv("FENGSHEN_LLAMA_REAL_DIAGNOSTIC", "0")
	t.Run("diagnostic-disabled", TestLlamaRealBoundedDiagnostic)
	t.Setenv("FENGSHEN_LLAMA_REAL_INFERENCE", "0")
	t.Setenv("FENGSHEN_LLAMA_REAL_DIAGNOSTIC", "1")
	t.Run("scoring-disabled", TestLlamaRealInference)
}

func TestLlamaHarnessContextClassification(t *testing.T) {
	const secret = "rawbody token=secret W:/private/model.gguf"
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, tc := range []struct {
		ctx  context.Context
		err  error
		want string
	}{
		{context.Background(), errors.New(secret), "unknown"},
		{canceled, errors.New(secret), "canceled"},
		{expired, errors.New(secret), "deadline_exceeded"},
		{context.Background(), fmt.Errorf("%s: %w", secret, context.Canceled), "canceled"},
		{context.Background(), fmt.Errorf("%s: %w", secret, context.DeadlineExceeded), "deadline_exceeded"},
		{context.Background(), &llamaInferenceError{kind: scoring.ErrorKindTimeout}, "deadline_exceeded"},
		{context.Background(), &llamaInferenceError{kind: scoring.ErrorKindCanceled}, "canceled"},
	} {
		if got := llamaHarnessContextCategory(tc.ctx, tc.err); got != tc.want {
			t.Fatal("unsafe or incorrect context classification")
		}
	}
	if llamaRealTotalBudget > 15*time.Minute || llamaRealTotalBudget <= 2*time.Minute+time.Duration(scoring.MaxTimeoutSec)*time.Second || scoring.MaxTimeoutSec != 600 || llamaDiagnosticTotalBudget > 8*time.Minute || llamaDiagnosticRequestBudget <= 0 || llamaDiagnosticRequestBudget > 2*time.Minute {
		t.Fatal("invalid harness budgets")
	}
}

func TestLlamaDiagnosticMetadataOffline(t *testing.T) {
	shared, err := scoring.BuildTextRequest(llamaRealCandidate(), scoring.ModelConfig{Model: "bound-model"})
	if err != nil {
		t.Fatal("invalid synthetic candidate")
	}
	const secret = "rawbody token=secret W:/private/model.gguf"
	for _, tc := range []struct {
		name                  string
		maxTokens, completion int
		finish, category      string
	}{
		{"prefill", 0, 0, "length", "prefill_observed"},
		{"native-first-sample", 0, 1, "length", "prefill_not_supported"},
		{"single-decode", 1, 1, "stop", "decode_observed"},
		{"single-length", 1, 1, "length", "truncated"},
		{"single-empty-stop", 1, 0, "stop", "response_invalid"},
		{"single-over-bound", 1, 2, "stop", "token_bound_exceeded"},
		{"single-untrusted-reason", 1, 1, secret, "response_invalid"},
		{"decode", 64, 12, "stop", "decode_observed"},
		{"empty-decode", 64, 0, "stop", "response_invalid"},
		{"truncation", 64, 64, "length", "truncated"},
		{"over-bound", 64, 65, "stop", "token_bound_exceeded"},
		{"untrusted-reason", 64, 1, secret, "response_invalid"},
		{"negative-usage", 64, -1, "stop", "response_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var wire struct {
					scoring.TextRequest
					MaxTokens *int `json:"max_tokens"`
					Stream    bool `json:"stream"`
				}
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-key" || json.NewDecoder(r.Body).Decode(&wire) != nil || wire.MaxTokens == nil || *wire.MaxTokens != tc.maxTokens || wire.Stream {
					t.Error("wrong diagnostic request or omitted zero")
				}
				want, _ := json.Marshal(shared)
				got, _ := json.Marshal(wire.TextRequest)
				if !bytes.Equal(want, got) {
					t.Error("diagnostic changed shared scoring request")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"choices": []any{map[string]any{"message": map[string]any{"content": secret}, "finish_reason": tc.finish}},
					"usage":   map[string]int{"prompt_tokens": 123, "completion_tokens": tc.completion},
					"tokens":  []int{42, 43}, "prompt": secret,
				})
			}))
			defer server.Close()
			body, _ := json.Marshal(struct {
				scoring.TextRequest
				MaxTokens int  `json:"max_tokens"`
				Stream    bool `json:"stream"`
			}{shared, tc.maxTokens, false})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			client := llamaHTTP()
			defer client.CloseIdleConnections()
			status, prompt, completion, finish, category := llamaDiagnosticRequest(ctx, client, server.URL, "fixture-key", body, tc.maxTokens)
			if calls != 1 || status != 200 || prompt != 123 || completion != tc.completion || category != tc.category {
				t.Fatal("wrong bounded metadata classification")
			}
			if finish != tc.finish && !(tc.finish == secret && finish == "unknown") || strings.Contains(fmt.Sprint(status, prompt, completion, finish, category), secret) {
				t.Fatal("unsafe finish reason or retained response text")
			}
		})
	}
}

func TestLlamaDiagnosticFailureOffline(t *testing.T) {
	for _, tc := range []struct {
		name, body, category string
		status               int
	}{
		{"http", `rawbody token=secret W:/private/model.gguf`, "http_failed", 503},
		{"json", `{broken`, "response_invalid", 200},
		{"oversize", strings.Repeat("x", (1<<20)+1), "response_invalid", 200},
		{"missing-usage", `{"choices":[{"finish_reason":"stop"}]}`, "response_invalid", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			client := llamaHTTP()
			defer client.CloseIdleConnections()
			status, _, _, _, category := llamaDiagnosticRequest(ctx, client, server.URL, "fixture-key", []byte(`{}`), 64)
			if calls != 1 || status != tc.status || category != tc.category {
				t.Fatal("diagnostic retried or lost safe failure category")
			}
		})
	}
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	client := llamaHTTP()
	defer client.CloseIdleConnections()
	_, _, _, _, category := llamaDiagnosticRequest(ctx, client, server.URL, "fixture-key", []byte(`{}`), 64)
	if category != "deadline_exceeded" {
		t.Fatal("response read did not retain finite request deadline")
	}
}

// The response body stays local; only allowlisted metadata leaves this helper.
func llamaDiagnosticRequest(ctx context.Context, client *http.Client, base, key string, body []byte, maxTokens int) (status, prompt, completion int, finish, category string) {
	finish, category = "unknown", "request_failed"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		category = llamaHarnessContextCategory(ctx, err)
		if category == "unknown" {
			category = "transport_failed"
		}
		return
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	if status != http.StatusOK {
		category = "http_failed"
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		category = llamaHarnessContextCategory(ctx, err)
		if category == "unknown" {
			category = "read_failed"
		}
		return
	}
	category = "response_invalid"
	if len(data) > 1<<20 {
		return
	}
	var metadata struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     *int `json:"prompt_tokens"`
			Completion *int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &metadata) != nil || len(metadata.Choices) != 1 || metadata.Usage.Prompt == nil || metadata.Usage.Completion == nil {
		return
	}
	prompt, completion = *metadata.Usage.Prompt, *metadata.Usage.Completion
	switch metadata.Choices[0].FinishReason {
	case "stop", "length", "content_filter", "tool_calls", "function_call":
		finish = metadata.Choices[0].FinishReason
	}
	if prompt <= 0 || completion < 0 {
		return
	}
	if maxTokens == 0 {
		// fd27b20 schema aliases max_tokens to n_predict and advertises prefill.
		// Its post_decode path samples before checking budget: verify zero usage,
		// never silently relabel that first generated token as prefill-only.
		if completion != 0 {
			category = "prefill_not_supported"
		} else if finish == "stop" || finish == "length" {
			category = "prefill_observed"
		}
		return
	}
	// The independent one-token probe uses the same metadata-only parser.
	if (maxTokens != 1 && maxTokens != 64) || completion > maxTokens {
		category = "token_bound_exceeded"
		return
	}
	if finish == "length" {
		category = "truncated"
	} else if finish == "stop" && completion > 0 {
		category = "decode_observed"
	}
	return
}

// Only these fd27b20 SLT_INF records are accepted. Reformatting verifies printf
// padding as well as the complete function prefix and numeric-only payload.
var llamaDiagnosticNativeRules = [...]struct {
	function, format string
	pattern          *regexp.Regexp
}{
	{"launch_slot_with_task", "processing task, is_child = %d", regexp.MustCompile(`^processing task, is_child = ([01])$`)},
	{"print_timings_pp", "prompt processing, n_tokens = %6d, progress = %.2f, t = %6.2f s / %.2f tokens per second", regexp.MustCompile(`^prompt processing, n_tokens = ( *[0-9]+), progress = ([0-9]+\.[0-9]{2}), t = ( *[0-9]+\.[0-9]{2}) s / ([0-9]+\.[0-9]{2}) tokens per second$`)},
	{"print_timings_tg", "n_gen = %6d, tg = %6.2f t/s, tg_3s = %6.2f t/s", regexp.MustCompile(`^n_gen = ( *[0-9]+), tg = ( *[0-9]+\.[0-9]{2}) t/s, tg_3s = ( *[0-9]+\.[0-9]{2}) t/s$`)},
	{"print_timings", "prompt eval time = %10.2f ms / %5d tokens (%8.2f ms per token, %8.2f tokens per second)", regexp.MustCompile(`^prompt eval time = ( *[0-9]+\.[0-9]{2}) ms / ( *[0-9]+) tokens \(( *[0-9]+\.[0-9]{2}) ms per token, ( *[0-9]+\.[0-9]{2}) tokens per second\)$`)},
}

// common/log.cpp emits either no prefix, I, or M.ss.mmm.uuu I (no colors).
var llamaDiagnosticNativePrefix = regexp.MustCompile(`^(?:I |(?:0|[1-9][0-9]*)\.[0-5][0-9]\.[0-9]{3}\.[0-9]{3} I )?slot (.{12}): id ( *[0-9]+) \| task (-?[0-9]+) \| (.*)$`)

type llamaDiagnosticNativeValue struct {
	Count      uint64
	Slot, Task int64
	Values     [4]float64
}

type llamaDiagnosticNativeSnapshot struct {
	Stages   [4]llamaDiagnosticNativeValue
	Overlong uint64
}

type llamaDiagnosticNativeCounters struct {
	mu   sync.Mutex
	data llamaDiagnosticNativeSnapshot
}

func (c *llamaDiagnosticNativeCounters) snapshot() llamaDiagnosticNativeSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data
}

func (c *llamaDiagnosticNativeCounters) accept(line []byte) {
	m := llamaDiagnosticNativePrefix.FindSubmatch(line)
	if m == nil {
		return
	}
	slot, errSlot := strconv.ParseInt(strings.TrimSpace(string(m[2])), 10, 32)
	task, errTask := strconv.ParseInt(string(m[3]), 10, 32)
	if errSlot != nil || errTask != nil || task < -1 {
		return
	}
	for i, rule := range llamaDiagnosticNativeRules {
		if string(m[1]) != fmt.Sprintf("%12.12s", rule.function) || string(m[2]) != fmt.Sprintf("%2d", slot) || string(m[3]) != strconv.FormatInt(task, 10) {
			continue
		}
		fields := rule.pattern.FindSubmatch(m[4])
		if fields == nil {
			continue
		}
		value := llamaDiagnosticNativeValue{Count: 1, Slot: slot, Task: task}
		var args [4]any
		valid := true
		for j, field := range fields[1:] {
			text := strings.TrimSpace(string(field))
			if strings.Contains(text, ".") {
				n, err := strconv.ParseFloat(text, 64)
				if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
					valid = false
				}
				value.Values[j], args[j] = n, n
			} else {
				n, err := strconv.ParseInt(text, 10, 32)
				if err != nil {
					valid = false
				}
				value.Values[j], args[j] = float64(n), n
			}
		}
		if !valid || string(m[4]) != fmt.Sprintf(rule.format, args[:len(fields)-1]...) {
			return
		}
		c.mu.Lock()
		value.Count += c.data.Stages[i].Count
		c.data.Stages[i] = value
		c.mu.Unlock()
		return
	}
}

// Each stream has independent, fixed-size framing; raw bytes never enter a snapshot.
// An unterminated final line is not evidence. Overflow discards through newline.
type llamaDiagnosticNativeWriter struct {
	mu       sync.Mutex
	counters *llamaDiagnosticNativeCounters
	line     [512]byte
	n        int
	dropping bool
}

func (w *llamaDiagnosticNativeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			if !w.dropping {
				n := w.n
				if n > 0 && w.line[n-1] == '\r' {
					n--
				}
				w.counters.accept(w.line[:n])
			}
			clear(w.line[:])
			w.n, w.dropping = 0, false
			continue
		}
		if w.dropping {
			continue
		}
		if w.n == len(w.line) {
			clear(w.line[:])
			w.n, w.dropping = 0, true
			w.counters.mu.Lock()
			w.counters.data.Overlong++
			w.counters.mu.Unlock()
			continue
		}
		w.line[w.n] = b
		w.n++
	}
	return len(p), nil
}

// This independent opt-in never executes or passes the real scoring contract.
func TestLlamaRealBoundedDiagnostic(t *testing.T) {
	if os.Getenv("FENGSHEN_LLAMA_REAL_DIAGNOSTIC") != "1" {
		t.Skip("explicit independent real diagnostic opt-in required")
	}
	// Reserve 30s of the eight-minute ceiling for owned cleanup.
	ctx, cancel := context.WithTimeout(context.Background(), llamaDiagnosticTotalBudget-30*time.Second)
	defer cancel()
	root, manifest, component := llamaRealComponent(t)
	resolved := llamaRealResolved(t, ctx, root, manifest, component, "highlight.score", filepath.SplitList(os.Getenv("FENGSHEN_LLAMA_REAL_TEXT_MODEL")))
	binary, args, err := llamaPlan(resolved, false)
	if err != nil {
		t.Fatal("category=plan_invalid")
	}
	request, err := scoring.BuildTextRequest(llamaRealCandidate(), scoring.ModelConfig{Model: "bound-model"})
	if err != nil {
		t.Fatal("category=request_invalid")
	}
	firstFailure, failures := llamaDiagnosticPhases(ctx, func(phase context.Context, maxTokens int) string {
		return llamaDiagnosticFreshPhase(t, phase, binary, args, request, maxTokens)
	})
	if failures != 0 {
		t.Fatalf("category=diagnostic_failed first_failure=%s failed_phases=%d", firstFailure, failures)
	}
	// Even an observed stop is not validated scoring; length is only truncation.
	t.Skip("category=diagnostic_only_not_scoring")
}

const (
	llamaDiscriminatingRequestBudget = 30 * time.Second
	llamaDiagnosticLoadBudget        = 80 * time.Second
	llamaDiscriminatingPhaseBudget   = 110 * time.Second
)

type llamaDiscriminatingExperiment struct {
	args []string
	body []byte
}

func llamaDiscriminatingExperiments(args []string, request scoring.TextRequest) (experiments [4]llamaDiscriminatingExperiment, err error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return experiments, err
	}
	var wire map[string]json.RawMessage
	if err = json.Unmarshal(raw, &wire); err != nil {
		return experiments, err
	}
	delete(wire, "response_format")
	wire["max_tokens"], wire["stream"] = json.RawMessage("1"), json.RawMessage("false")
	messages := append([]scoring.TextMessage(nil), request.Messages...)
	for i := range experiments {
		if i == 1 {
			for j := range messages {
				if messages[j].Role == "user" {
					messages[j].Content = "Reply with OK."
				}
			}
			wire["messages"], err = json.Marshal(messages)
			if err != nil {
				return experiments, err
			}
		}
		experiments[i].body, err = json.Marshal(wire)
		if err != nil {
			return experiments, err
		}
		experiments[i].args = append([]string(nil), args...)
		if i >= 2 {
			experiments[i].args = append(experiments[i].args, "--batch-size", "32", "--ubatch-size", "32")
		}
		if i == 3 {
			for j := 0; j+1 < len(experiments[i].args); j++ {
				if experiments[i].args[j] == "--threads" {
					experiments[i].args[j+1] = "1"
				}
			}
		}
	}
	return experiments, nil
}

// This opt-in is independent of both scoring and the original 0/64 diagnostic.
func TestLlamaRealDiscriminatingDiagnostic(t *testing.T) {
	if os.Getenv("FENGSHEN_LLAMA_REAL_DISCRIMINATING_DIAGNOSTIC") != "1" {
		t.Skip("explicit independent discriminating diagnostic opt-in required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), llamaDiagnosticTotalBudget-30*time.Second)
	defer cancel()
	root, manifest, component := llamaRealComponent(t)
	resolved := llamaRealResolved(t, ctx, root, manifest, component, "highlight.score", filepath.SplitList(os.Getenv("FENGSHEN_LLAMA_REAL_TEXT_MODEL")))
	binary, args, err := llamaPlan(resolved, false)
	if err != nil {
		t.Fatal("category=plan_invalid")
	}
	request, err := scoring.BuildTextRequest(llamaRealCandidate(), scoring.ModelConfig{Model: "bound-model"})
	if err != nil {
		t.Fatal("category=request_invalid")
	}
	experiments, err := llamaDiscriminatingExperiments(args, request)
	if err != nil {
		t.Fatal("category=request_invalid")
	}
	llamaDiscriminatingPhases(ctx, func(phase context.Context, index int) string {
		var native llamaDiagnosticNativeCounters
		t.Logf("experiment=%d stage=fresh_start", index+1)
		category := llamaDiagnosticOwnedPhase(t, phase, binary, experiments[index].args, experiments[index].body, 1, llamaDiscriminatingRequestBudget, &native)
		snapshot := native.snapshot()
		// Zero counts are an observation blind spot, not a stalled-stage diagnosis.
		for _, i := range []int{0, 1, 3} {
			stage := snapshot.Stages[i]
			t.Logf("experiment=%d native_stage=%s count=%d value0=%.2f value1=%.2f value2=%.2f value3=%.2f", index+1, [...]string{"received", "prefill_progress", "", "prompt_eval"}[i], stage.Count, stage.Values[0], stage.Values[1], stage.Values[2], stage.Values[3])
		}
		return category
	}, func(index int, category string) {
		t.Logf("experiment=%d category=%s", index+1, category)
	})
	// No response is validated against the scoring contract, including stop.
	t.Skip("category=diagnostic_only")
}

func llamaDiscriminatingPhases(ctx context.Context, run func(context.Context, int) string, observe func(int, string)) {
	for i := 0; i < 4; i++ {
		phase, cancel := context.WithTimeout(ctx, llamaDiscriminatingPhaseBudget)
		category := run(phase, i)
		if phase.Err() != nil {
			category = llamaHarnessContextCategory(phase, nil)
		}
		cancel()
		observe(i, category)
	}
}

func llamaDiagnosticTimedRequest(ctx context.Context, client *http.Client, base, key string, body []byte, maxTokens int, budget time.Duration) (status, prompt, completion int, finish, category string) {
	run, stop := context.WithTimeout(ctx, budget)
	defer stop()
	return llamaDiagnosticRequest(run, client, base, key, body, maxTokens)
}

// Each callback owns and closes its supervisor before the next phase starts.
func llamaDiagnosticPhases(ctx context.Context, run func(context.Context, int) string) (firstFailure string, failures int) {
	for _, maxTokens := range []int{0, 64} {
		phase, cancel := context.WithTimeout(ctx, 210*time.Second)
		category := run(phase, maxTokens)
		if phase.Err() != nil {
			category = llamaHarnessContextCategory(phase, nil)
		}
		cancel()
		switch category {
		case "prefill_observed", "decode_observed", "truncated":
			continue
		}
		if failures == 0 {
			firstFailure = category
		}
		failures++
	}
	return
}

func llamaDiagnosticFreshPhase(t *testing.T, ctx context.Context, binary runtimeprocess.VerifiedBinary, args []string, request scoring.TextRequest, maxTokens int) (category string) {
	t.Helper()
	var native llamaDiagnosticNativeCounters
	// Log after owned cleanup and drains, including timeouts. Zero records mean
	// no observable evidence, not proof of a stalled stage or prefill timing.
	defer func() {
		s := native.snapshot()
		for i, stage := range s.Stages {
			name := [...]string{"received", "prefill_progress", "decode_progress", "prompt_eval"}[i]
			t.Logf("max_tokens=%d native_stage=%s count=%d slot=%d task=%d value0=%.2f value1=%.2f value2=%.2f value3=%.2f", maxTokens, name, stage.Count, stage.Slot, stage.Task, stage.Values[0], stage.Values[1], stage.Values[2], stage.Values[3])
		}
		t.Logf("max_tokens=%d native_overlong=%d", maxTokens, s.Overlong)
	}()
	category = "request_invalid"
	body, err := json.Marshal(struct {
		scoring.TextRequest
		MaxTokens int  `json:"max_tokens"`
		Stream    bool `json:"stream"`
	}{request, maxTokens, false})
	if err != nil {
		return
	}
	return llamaDiagnosticOwnedPhase(t, ctx, binary, args, body, maxTokens, llamaDiagnosticRequestBudget, &native)
}

func llamaDiagnosticOwnedPhase(t *testing.T, ctx context.Context, binary runtimeprocess.VerifiedBinary, args []string, body []byte, maxTokens int, requestBudget time.Duration, native *llamaDiagnosticNativeCounters) (category string) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "port_unavailable"
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	if port == 11434 {
		return "port_reserved"
	}
	var keyBytes [32]byte
	if _, err := rand.Read(keyBytes[:]); err != nil {
		return "key_generation_failed"
	}
	key := hex.EncodeToString(keyBytes[:])
	phaseArgs := append(append([]string(nil), args...), "--port", strconv.Itoa(port))
	owned, cancel := context.WithCancel(ctx)
	var supervisor runtimeprocess.Supervisor
	var drains sync.WaitGroup
	defer func() {
		cancel()
		if supervisor.Close() != nil {
			t.Error("category=owned_cleanup_failed")
			category = "owned_cleanup_failed"
		}
		drains.Wait()
	}()
	started := time.Now()
	process, err := startLlama(owned, &supervisor, binary, phaseArgs, key)
	if err != nil {
		t.Logf("max_tokens=%d category=start_failed context=%s", maxTokens, llamaHarnessContextCategory(ctx, err))
		return "start_failed"
	}
	_ = process.Stdin().Close()
	for _, stream := range []io.Reader{process.Stdout(), process.Stderr()} {
		drains.Add(1)
		go func(src io.Reader) {
			defer drains.Done()
			writer := llamaDiagnosticNativeWriter{counters: native}
			defer func() { clear(writer.line[:]) }()
			_, _ = io.Copy(&writer, src)
		}(stream)
	}
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	loading, stopLoading := context.WithTimeout(ctx, llamaDiagnosticLoadBudget)
	err = llamaReady(loading, process, binary, base)
	stopLoading()
	if err != nil {
		t.Logf("max_tokens=%d stage=loading elapsed_ms=%d category=load_failed", maxTokens, time.Since(started).Milliseconds())
		return "load_failed"
	}
	t.Logf("max_tokens=%d stage=loading elapsed_ms=%d", maxTokens, time.Since(started).Milliseconds())
	if process.CheckIdentity(binary) != nil {
		return "owned_identity_failed"
	}
	client := llamaHTTP()
	defer client.CloseIdleConnections()
	started = time.Now()
	status, prompt, completion, finish, category := llamaDiagnosticTimedRequest(ctx, client, base, key, body, maxTokens, requestBudget)
	t.Logf("max_tokens=%d status=%d prompt_tokens=%d completion_tokens=%d elapsed_ms=%d finish_reason=%s category=%s", maxTokens, status, prompt, completion, time.Since(started).Milliseconds(), finish, category)
	if process.CheckIdentity(binary) != nil {
		return "owned_identity_failed"
	}
	return category
}

func TestLlamaDiagnosticNativeOffline(t *testing.T) {
	lines := [4]string{
		"slot launch_slot_: id  0 | task 7 | processing task, is_child = 0",
		"slot print_timing: id  0 | task 7 | prompt processing, n_tokens =    123, progress = 0.50, t =   3.25 s / 37.85 tokens per second",
		"slot print_timing: id  0 | task 7 | n_gen =     12, tg =   2.50 t/s, tg_3s =   2.00 t/s",
		"slot print_timing: id  0 | task 7 | prompt eval time =    3250.00 ms /   123 tokens (   26.42 ms per token,    37.85 tokens per second)",
	}
	const secret = "api_key=secret-native W:/private/model.gguf prompt=hidden"
	var counters llamaDiagnosticNativeCounters
	stdout := llamaDiagnosticNativeWriter{counters: &counters}
	stderr := llamaDiagnosticNativeWriter{counters: &counters}
	write := func(w *llamaDiagnosticNativeWriter, text string) {
		t.Helper()
		n, err := w.Write([]byte(text))
		if n != len(text) || err != nil {
			t.Fatal("writer did not consume input")
		}
	}
	// Interleave fragments from distinct streams; never join them together.
	write(&stdout, lines[0][:20])
	write(&stderr, lines[1]+"\r")
	write(&stdout, lines[0][20:]+"\r\n")
	write(&stderr, "\n"+lines[2]+"\n"+lines[3]+"\n")
	want := llamaDiagnosticNativeSnapshot{Stages: [4]llamaDiagnosticNativeValue{
		{Count: 1, Slot: 0, Task: 7, Values: [4]float64{0}},
		{Count: 1, Slot: 0, Task: 7, Values: [4]float64{123, 0.5, 3.25, 37.85}},
		{Count: 1, Slot: 0, Task: 7, Values: [4]float64{12, 2.5, 2}},
		{Count: 1, Slot: 0, Task: 7, Values: [4]float64{3250, 123, 26.42, 37.85}},
	}}
	if counters.snapshot() != want {
		t.Fatal("incorrect native stage numbers")
	}
	for _, line := range lines {
		for _, bad := range []string{
			secret + line, line + secret, line + " ", " " + line,
			"unknown " + line, "W " + line, "00.01.002.003 I " + line,
			"0.60.002.003 I " + line, "0.01.002.003 " + line,
			strings.Replace(line, "id  0", "id 0", 1),
			strings.Replace(line, "task 7", "task 07", 1),
			strings.Replace(line, "slot ", "slot secret", 1),
		} {
			write(&stdout, bad+"\n")
		}
	}
	write(&stdout, secret+"\nunknown\n")
	if counters.snapshot() != want {
		t.Fatal("counted unknown or secret-bearing records")
	}
	write(&stdout, strings.Repeat("x", 512))
	if stdout.n != 512 || stdout.dropping {
		t.Fatal("incorrect exact line limit")
	}
	write(&stdout, secret+lines[0])
	if stdout.n != 0 || !stdout.dropping || stdout.line != ([512]byte{}) {
		t.Fatal("overflow did not clear bounded framing")
	}
	write(&stdout, "\n"+lines[0]+"\n")
	want.Overlong, want.Stages[0].Count = 1, 2
	if counters.snapshot() != want || stdout.line != ([512]byte{}) {
		t.Fatal("overflow recovery failed")
	}
	// Prefix forms verified in common/log.cpp, without permissive prefix stripping.
	write(&stdout, "I "+lines[0]+"\n0.01.002.003 I "+lines[0]+"\n")
	want.Stages[0].Count += 2
	for _, b := range []byte(lines[1] + "\r\n") {
		_, _ = stdout.Write([]byte{b})
	}
	want.Stages[1].Count++
	for _, bad := range []string{
		strings.Replace(lines[1], "0.50", "NaN", 1),
		strings.Replace(lines[1], "0.50", "0.500", 1),
		strings.Replace(lines[1], "   123", "000123", 1),
		strings.Replace(lines[2], "2.50", secret, 1),
		strings.Replace(lines[0], "task 7", "task 99999999999999999999999", 1),
	} {
		write(&stdout, bad+"\n")
	}
	// Concurrent aggregation, with a separate framing buffer per stream.
	var done sync.WaitGroup
	for i := 0; i < 2; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			w := llamaDiagnosticNativeWriter{counters: &counters}
			for j := 0; j < 100; j++ {
				_, _ = w.Write([]byte(lines[2] + "\n"))
				_ = counters.snapshot()
			}
		}()
	}
	done.Wait()
	want.Stages[2].Count += 200
	write(&stderr, lines[0]) // No newline: not evidence.
	if counters.snapshot() != want {
		t.Fatal("stream framing or aggregation lost counters")
	}
	snapshot := counters.snapshot()
	encoded, err := json.Marshal(snapshot)
	if err != nil || strings.Contains(string(encoded)+fmt.Sprintf("%+v %#v", snapshot, snapshot), "secret") || strings.Contains(string(encoded), "private") {
		t.Fatal("snapshot retained sensitive text")
	}
	var numericOnly func(reflect.Type) bool
	numericOnly = func(typ reflect.Type) bool {
		switch typ.Kind() {
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				if !numericOnly(typ.Field(i).Type) {
					return false
				}
			}
			return true
		case reflect.Array:
			return numericOnly(typ.Elem())
		case reflect.Int64, reflect.Uint64, reflect.Float64:
			return true
		}
		return false
	}
	if !numericOnly(reflect.TypeOf(snapshot)) {
		t.Fatal("snapshot contains non-numeric or unbounded storage")
	}
}

func TestLlamaDiscriminatingExperimentsOffline(t *testing.T) {
	request, err := scoring.BuildTextRequest(llamaRealCandidate(), scoring.ModelConfig{Model: "bound-model"})
	if err != nil {
		t.Fatal("invalid synthetic candidate")
	}
	before, _ := json.Marshal(request)
	args := []string{"--host", "127.0.0.1", "--offline", "--no-webui", "--no-warmup", "--parallel", "1", "--ctx-size", "4096", "--n-gpu-layers", "0", "--threads", "4", "--no-mmproj-offload", "--model", "W:/private/model.gguf", "--no-mmproj"}
	argsBefore := append([]string(nil), args...)
	experiments, err := llamaDiscriminatingExperiments(args, request)
	if err != nil {
		t.Fatal("invalid experiments")
	}
	var previous map[string]json.RawMessage
	for i, experiment := range experiments {
		var wire map[string]json.RawMessage
		if json.Unmarshal(experiment.body, &wire) != nil || wire["response_format"] != nil || string(wire["max_tokens"]) != "1" || string(wire["stream"]) != "false" {
			t.Fatal("unsafe diagnostic wire request")
		}
		if i == 0 {
			var want map[string]json.RawMessage
			_ = json.Unmarshal(before, &want)
			delete(want, "response_format")
			want["max_tokens"], want["stream"] = json.RawMessage("1"), json.RawMessage("false")
			if !reflect.DeepEqual(wire, want) {
				t.Fatal("first experiment changed scoring fields beyond format and budget")
			}
		} else if i == 1 {
			var messages []scoring.TextMessage
			if json.Unmarshal(wire["messages"], &messages) != nil || len(messages) != len(request.Messages) {
				t.Fatal("changed message structure")
			}
			for j, message := range messages {
				want := request.Messages[j]
				if want.Role == "user" {
					want.Content = "Reply with OK."
				}
				if message != want {
					t.Fatal("changed more than synthetic user prompt")
				}
			}
			copyWire := make(map[string]json.RawMessage, len(wire))
			for key, value := range wire {
				copyWire[key] = value
			}
			copyWire["messages"] = previous["messages"]
			if !reflect.DeepEqual(copyWire, previous) || bytes.Equal(wire["messages"], previous["messages"]) {
				t.Fatal("prompt experiment is not single-variable")
			}
		} else if !reflect.DeepEqual(wire, previous) {
			t.Fatal("argument experiments changed request")
		}
		wantArgs := append([]string(nil), argsBefore...)
		if i >= 2 {
			wantArgs = append(wantArgs, "--batch-size", "32", "--ubatch-size", "32")
		}
		if i == 3 {
			for j, arg := range wantArgs {
				if arg == "--threads" {
					wantArgs[j+1] = "1"
				}
			}
		}
		if !reflect.DeepEqual(experiment.args, wantArgs) {
			t.Fatal("argument experiment is not single-variable")
		}
		seen := map[string]bool{}
		for _, arg := range experiment.args {
			if strings.HasPrefix(arg, "--") {
				if seen[arg] {
					t.Fatal("duplicate diagnostic flag")
				}
				seen[arg] = true
			}
		}
		previous = wire
	}
	after, _ := json.Marshal(request)
	if !bytes.Equal(before, after) || !reflect.DeepEqual(args, argsBefore) {
		t.Fatal("experiments mutated shared scoring request or plan")
	}
}

type llamaDiagnosticRoundTripper func(*http.Request) (*http.Response, error)

func (f llamaDiagnosticRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLlamaDiscriminatingRequestOffline(t *testing.T) {
	const secret = "rawbody api_key=hidden-secret W:/private/model.gguf token=hidden"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.Copy(io.Discard, r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": secret}, "finish_reason": "length"}},
			"usage":   map[string]int{"prompt_tokens": 12, "completion_tokens": 1}, "tokens": []int{42}, "prompt": secret,
		})
	}))
	defer server.Close()
	client := llamaHTTP()
	transport := client.Transport.(*http.Transport)
	defer transport.CloseIdleConnections()
	var requestContext context.Context
	client.Transport = llamaDiagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		requestContext = r.Context()
		deadline, ok := requestContext.Deadline()
		if remaining := time.Until(deadline); !ok || remaining > 30*time.Second || remaining < 29*time.Second {
			t.Fatal("request does not have independent 30-second deadline")
		}
		return transport.RoundTrip(r)
	})
	status, prompt, completion, finish, category := llamaDiagnosticTimedRequest(context.Background(), client, server.URL, "fixture-key", []byte(`{"max_tokens":1,"stream":false}`), 1, llamaDiscriminatingRequestBudget)
	if calls != 1 || status != 200 || prompt != 12 || completion != 1 || finish != "length" || category != "truncated" || requestContext.Err() != context.Canceled {
		t.Fatal("wrong single-token metadata or request cleanup")
	}
	if strings.Contains(fmt.Sprint(status, prompt, completion, finish, category), "hidden") {
		t.Fatal("response text escaped metadata boundary")
	}
	client.Transport = llamaDiagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		requestContext = r.Context()
		return nil, errors.New(secret)
	})
	_, _, _, finish, category = llamaDiagnosticTimedRequest(context.Background(), client, server.URL, "fixture-key", nil, 1, llamaDiscriminatingRequestBudget)
	if finish != "unknown" || category != "transport_failed" || requestContext.Err() != context.Canceled {
		t.Fatal("transport cause escaped or request context retained")
	}

	release := make(chan struct{})
	canceled := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer blocked.Close()
	defer close(release)
	client.Transport = transport
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, _, _, category = llamaDiagnosticTimedRequest(ctx, client, blocked.URL, "fixture-key", nil, 1, llamaDiscriminatingRequestBudget)
	if category != "deadline_exceeded" {
		t.Fatal("request escaped parent cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("canceled request did not release HTTP fixture")
	}
}

func TestLlamaDiscriminatingPhasesOffline(t *testing.T) {
	if llamaDiscriminatingRequestBudget != 30*time.Second || llamaDiagnosticLoadBudget != 80*time.Second || llamaDiscriminatingPhaseBudget != llamaDiagnosticLoadBudget+llamaDiscriminatingRequestBudget || 4*llamaDiscriminatingPhaseBudget > llamaDiagnosticTotalBudget-30*time.Second {
		t.Fatal("invalid independent four-phase budgets")
	}
	for _, state := range []string{"active", "canceled", "expired"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			if state == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else if state == "expired" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Unix(0, 0))
				defer cancel()
			}
			calls, closed, observed := 0, 0, 0
			var previous context.Context
			categories := [...]string{"deadline_exceeded", "load_failed", "truncated", "decode_observed"}
			llamaDiscriminatingPhases(ctx, func(phase context.Context, index int) string {
				if calls != index || closed != calls || observed != calls || previous != nil && previous.Err() == nil || phase.Err() != ctx.Err() {
					t.Fatal("fresh-phase order, cancellation or cleanup changed")
				}
				deadline, ok := phase.Deadline()
				if !ok || time.Until(deadline) > llamaDiscriminatingPhaseBudget || state == "active" && time.Until(deadline) < 109*time.Second {
					t.Fatal("missing independent 110-second phase deadline")
				}
				previous = phase
				calls++
				defer func() { closed++ }()
				return categories[index]
			}, func(index int, category string) {
				want := categories[index]
				if state != "active" {
					want = llamaHarnessContextCategory(ctx, nil)
				}
				if previous.Err() == nil || closed != calls || category != want {
					t.Fatal("phase observed before cleanup or lost cancellation")
				}
				observed++
			})
			if calls != 4 || closed != 4 || observed != 4 || previous.Err() == nil {
				t.Fatal("skipped independent experiment or retained phase context")
			}
		})
	}
}

func TestLlamaDiscriminatingIndependentOptInOffline(t *testing.T) {
	t.Setenv("FENGSHEN_LLAMA_REAL_COMPONENT_ROOT", "")
	t.Setenv("FENGSHEN_LLAMA_REAL_INFERENCE", "1")
	t.Setenv("FENGSHEN_LLAMA_REAL_DIAGNOSTIC", "1")
	for _, value := range []string{"", "0", "true"} {
		t.Setenv("FENGSHEN_LLAMA_REAL_DISCRIMINATING_DIAGNOSTIC", value)
		t.Run("disabled-"+value, TestLlamaRealDiscriminatingDiagnostic)
	}
}

func TestLlamaDiagnosticPhasesOffline(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
		failures            int
	}{
		{"timeout-continues", "deadline_exceeded", "decode_observed", 1},
		{"unsupported-continues", "prefill_not_supported", "truncated", 1},
		{"both-fail", "deadline_exceeded", "http_failed", 2},
		{"load-fails", "load_failed", "decode_observed", 1},
		{"metadata-only", "prefill_observed", "truncated", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, closed := 0, 0
			var previous context.Context
			first, failures := llamaDiagnosticPhases(context.Background(), func(ctx context.Context, maxTokens int) string {
				if closed != calls || calls == 1 && (previous.Err() != context.Canceled || maxTokens != 64) || calls == 0 && maxTokens != 0 {
					t.Fatal("phase order, cancellation or cleanup changed")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 210*time.Second {
					t.Fatal("missing phase deadline")
				}
				previous = ctx
				calls++
				defer func() { closed++ }()
				if calls == 1 {
					return tc.first
				}
				return tc.second
			})
			if calls != 2 || closed != 2 || failures != tc.failures || failures > 0 && first != tc.first || failures == 0 && first != "" {
				t.Fatal("lost first failure or skipped independent decode")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	first, failures := llamaDiagnosticPhases(ctx, func(ctx context.Context, _ int) string {
		calls++
		if ctx.Err() != context.Canceled {
			t.Fatal("phase escaped total cancellation")
		}
		return "decode_observed"
	})
	if calls != 2 || failures != 2 || first != "canceled" {
		t.Fatal("cancellation was accepted as success")
	}
	expired, stop := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer stop()
	first, failures = llamaDiagnosticPhases(expired, func(ctx context.Context, _ int) string {
		if ctx.Err() != context.DeadlineExceeded {
			t.Fatal("phase escaped total deadline")
		}
		return "prefill_observed"
	})
	if failures != 2 || first != "deadline_exceeded" {
		t.Fatal("expired budget was accepted as success")
	}
}

// Test-only budgets: loading/setup must not consume the scoring client's 600s.
const (
	llamaRealTotalBudget         = 15 * time.Minute
	llamaDiagnosticTotalBudget   = 8 * time.Minute
	llamaDiagnosticRequestBudget = 2 * time.Minute
)

func llamaHarnessContextCategory(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var inference *llamaInferenceError
	if errors.As(err, &inference) && inference != nil {
		if inference.kind == scoring.ErrorKindTimeout {
			return "deadline_exceeded"
		}
		if inference.kind == scoring.ErrorKindCanceled {
			return "canceled"
		}
	}
	if os.IsTimeout(err) {
		return "deadline_exceeded"
	}
	return "unknown"
}

func llamaRealCandidate() scoring.CandidateInput {
	candidate := scoring.ContractCandidate()
	candidate.TaskID, candidate.RequestID = "real-llama-task", "real-llama-request"
	candidate.AnalysisChain = domain.AnalysisChainAudioTextV1
	candidate.AudioSummary, candidate.ShotSummary = "", ""
	for i := range candidate.EligibleUnits {
		candidate.EligibleUnits[i].Words = []domain.TranscriptWord{}
	}
	candidate.AudioFeatures = &domain.CandidateAudioFeatures{
		Schema: domain.AudioFeatureSchemaV1, Window: domain.AudioInterval{StartSec: 0, EndSec: 60},
		VAD:    domain.VADStatus{Backend: "energy_v1", Reliable: true},
		Speech: []domain.AudioInterval{}, Silence: []domain.AudioInterval{}, Pauses: []domain.AudioInterval{},
		Overlaps: []domain.OverlapInterval{}, UnitMetrics: []domain.UnitAudioMetrics{}, SoundEvents: []domain.SoundEvent{},
		Loudness:   domain.LoudnessMetrics{Reason: "insufficient_voiced_frames"},
		SpeechRate: domain.SpeechRateMetrics{Reason: "insufficient_voiced_frames"}, Pitch: domain.PitchMetrics{Reason: "insufficient_voiced_frames"},
	}
	return candidate
}

func llamaRealComponent(t *testing.T) (string, components.Manifest, integration.ExecutionComponentRef) {
	t.Helper()
	root := os.Getenv("FENGSHEN_LLAMA_REAL_COMPONENT_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("category=component_root_invalid")
	}
	pin, err := runtimeprocess.PinFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal("category=manifest_pin_failed")
	}
	t.Cleanup(func() { _ = pin.Close() })
	data, err := io.ReadAll(io.LimitReader(pin, components.MaxManifestBytes+1))
	if err != nil || len(data) > components.MaxManifestBytes {
		t.Fatal("category=manifest_read_failed")
	}
	m, err := components.ParseManifest(bytes.NewReader(data))
	if err != nil {
		t.Fatal("category=manifest_invalid")
	}
	component := integration.ExecutionComponentRef{ID: m.ComponentID, Version: m.Version, Protocol: domain.CenterLlamaProtocol, ManifestSHA256: sha256Hex(data)}
	return root, m, component
}

// Keep every model shard and manifest file pinned until owned process cleanup.
func llamaRealResolved(t *testing.T, ctx context.Context, root string, m components.Manifest, component integration.ExecutionComponentRef, scene string, paths []string) Resolved {
	t.Helper()
	ref := integration.ExecutionBindingRef{Scene: scene, Revision: 1, SHA256: strings.Repeat("a", 64)}
	resolved := Resolved{ComponentRoot: root, Manifest: m, Binding: bindings.Binding{Scene: scene, Ref: ref, Component: component, Device: "cpu"}}
	modelID := "local-text"
	if scene == "highlight.vision" {
		modelID = "local-vision"
	}
	if len(paths) == 0 {
		t.Fatal("category=model_paths_missing")
	}
	for _, path := range paths {
		pin, err := runtimeprocess.PinFile(path)
		if err != nil {
			t.Fatal("category=model_pin_failed")
		}
		t.Cleanup(func() { _ = pin.Close() })
		resolved.Models = append(resolved.Models, ModelFile{ModelID: modelID, Path: path})
	}
	for _, file := range m.Files {
		pin, err := runtimeprocess.PinFile(filepath.Join(root, filepath.FromSlash(file.Path)))
		if err != nil {
			t.Fatal("category=component_pin_failed")
		}
		t.Cleanup(func() { _ = pin.Close() })
		h := sha256.New()
		buffer := make([]byte, 1<<20)
		var size int64
		for {
			if ctx.Err() != nil {
				t.Fatalf("category=%s", llamaHarnessContextCategory(ctx, nil))
			}
			n, readErr := pin.Read(buffer)
			_, _ = h.Write(buffer[:n])
			size += int64(n)
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				t.Fatal("category=component_read_failed")
			}
		}
		expectedSize, err := strconv.ParseInt(file.Bytes, 10, 64)
		if err != nil || size != expectedSize || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), file.SHA256) {
			t.Fatal("category=component_integrity_failed")
		}
	}
	if _, _, err := llamaPlan(resolved, scene == "highlight.vision"); err != nil {
		t.Fatal("category=plan_invalid")
	}
	return resolved
}

// Diagnostic snapshots contain no model-controlled strings or raw errors.
type llamaVisionEnd uint8

const (
	llamaVisionIncomplete llamaVisionEnd = iota
	llamaVisionComplete
	llamaVisionString
	llamaVisionEscape
	llamaVisionUnicode
	llamaVisionNumber
	llamaVisionWhitespace
)

type llamaVisionLexical struct {
	Bytes, LongestDecimal, NumberPeriod, Whitespace, LongestWhitespace int
	ReasonChars, CompleteEvents, UnclosedEvents, RepeatedEvents        int
	End                                                                llamaVisionEnd
}

// Scan prefixes too: JSON decoding alone loses the unfinished event and token.
func llamaVisionScan(s string) (out llamaVisionLexical) {
	out.Bytes = len(s)
	depth, eventsDepth, eventStart := 0, -1, -1
	key, value, eventsPending := "", false, false
	seen := map[string]bool{} // Local exact comparisons only; never returned or logged.
	for i := 0; i < len(s); {
		b := s[i]
		if strings.ContainsRune(" \t\r\n", rune(b)) {
			start := i
			for i < len(s) && strings.ContainsRune(" \t\r\n", rune(s[i])) {
				i++
			}
			n := i - start
			out.Whitespace += n
			out.LongestWhitespace = max(out.LongestWhitespace, n)
			if i == len(s) {
				out.End = llamaVisionWhitespace
			}
			continue
		}
		if b == '"' {
			start, chars := i, 0
			i++
			closed := false
			out.End = llamaVisionString
			for i < len(s) {
				if s[i] == '"' {
					i++
					closed = true
					break
				}
				if s[i] == '\\' {
					i++
					if i == len(s) {
						out.End = llamaVisionEscape
						break
					}
					if s[i] == 'u' {
						i++
						if len(s)-i < 4 {
							i = len(s)
							out.End = llamaVisionUnicode
							break
						}
						u, err := strconv.ParseUint(s[i:i+4], 16, 16)
						if err != nil {
							i = len(s)
							out.End = llamaVisionUnicode
							break
						}
						i += 4
						if u >= 0xd800 && u <= 0xdbff && i+6 <= len(s) && s[i:i+2] == `\u` {
							v, err := strconv.ParseUint(s[i+2:i+6], 16, 16)
							if err == nil && v >= 0xdc00 && v <= 0xdfff {
								i += 6
							}
						}
					} else {
						i++
					}
					chars++
					continue
				}
				_, size := utf8.DecodeRuneInString(s[i:])
				i += size
				chars++
			}
			if value && key == "reason" && eventStart >= 0 {
				out.ReasonChars += chars
			}
			if !closed {
				break
			}
			out.End = llamaVisionIncomplete
			j := i
			for j < len(s) && strings.ContainsRune(" \t\r\n", rune(s[j])) {
				j++
			}
			if j < len(s) && s[j] == ':' {
				key = ""
				var decoded string
				if json.Unmarshal([]byte(s[start:i]), &decoded) == nil && (decoded == "reason" || decoded == "events") {
					key = decoded
				}
				value = false
			} else {
				value = false
			}
			continue
		}
		if b == '-' || b >= '0' && b <= '9' {
			start := i
			for i < len(s) && strings.ContainsRune("0123456789.eE+-", rune(s[i])) {
				i++
			}
			token := s[start:i]
			if dot := strings.IndexByte(token, '.'); dot >= 0 {
				end := dot + 1
				for end < len(token) && token[end] >= '0' && token[end] <= '9' {
					end++
				}
				out.LongestDecimal = max(out.LongestDecimal, end-dot-1)
			}
			// Minimum exact digit-run period, at least two complete repetitions.
			for j := 0; j < len(token); {
				if token[j] < '0' || token[j] > '9' {
					j++
					continue
				}
				k := j
				for k < len(token) && token[k] >= '0' && token[k] <= '9' {
					k++
				}
				run := token[j:k]
				pi := make([]int, len(run))
				for n := 1; n < len(run); n++ {
					p := pi[n-1]
					for p > 0 && run[n] != run[p] {
						p = pi[p-1]
					}
					if run[n] == run[p] {
						p++
					}
					pi[n] = p
				}
				p := len(run) - pi[len(run)-1]
				if len(run) >= 2*p && len(run)%p == 0 {
					out.NumberPeriod = max(out.NumberPeriod, p)
				}
				j = k
			}
			value = false
			if i == len(s) {
				out.End = llamaVisionNumber
			}
			continue
		}
		out.End = llamaVisionIncomplete
		switch b {
		case ':':
			value = true
			eventsPending = key == "events"
		case '[':
			depth++
			if eventsPending {
				eventsDepth = depth
			}
			eventsPending = false
		case '{':
			if depth == eventsDepth && eventStart < 0 {
				eventStart = i
			}
			depth++
		case '}':
			depth--
			if eventStart >= 0 && depth == eventsDepth {
				event := s[eventStart : i+1]
				if json.Valid([]byte(event)) {
					out.CompleteEvents++
					var compact bytes.Buffer
					_ = json.Compact(&compact, []byte(event))
					if seen[compact.String()] {
						out.RepeatedEvents++
					}
					seen[compact.String()] = true
				}
				eventStart = -1
			}
		case ']':
			if depth == eventsDepth {
				eventsDepth = -1
			}
			depth--
		case ',':
			value = false
			key = ""
		}
		i++
	}
	if eventStart >= 0 {
		out.UnclosedEvents = 1
	}
	if json.Valid([]byte(s)) {
		out.End = llamaVisionComplete
	}
	return
}

type llamaVisionTimings struct {
	CacheN              float64 `json:"cache_n"`
	PromptN             float64 `json:"prompt_n"`
	PromptMS            float64 `json:"prompt_ms"`
	PromptPerTokenMS    float64 `json:"prompt_per_token_ms"`
	PromptPerSecond     float64 `json:"prompt_per_second"`
	PredictedN          float64 `json:"predicted_n"`
	PredictedMS         float64 `json:"predicted_ms"`
	PredictedPerTokenMS float64 `json:"predicted_per_token_ms"`
	PredictedPerSecond  float64 `json:"predicted_per_second"`
	DraftN              float64 `json:"draft_n"`
	DraftAccepted       float64 `json:"draft_n_accepted"`
}

type llamaVisionSnapshot struct {
	Status, Prompt, Completion, Total, NPredict int
	MessagesSHA, GrammarSHA                     [sha256.Size]byte
	Truncated                                   bool
	Stop                                        uint8 // 0 unknown, 1 eos, 2 word, 3 limit, 4 none.
	Valid                                       bool
	Timings, VerboseTimings                     llamaVisionTimings
	Lexical                                     llamaVisionLexical
}

func llamaVisionMetadata(data []byte, status int) (out llamaVisionSnapshot) {
	out.Status = status
	if status != http.StatusOK {
		return
	}
	var wire struct {
		Usage struct {
			Prompt     *int `json:"prompt_tokens"`
			Completion *int `json:"completion_tokens"`
			Total      *int `json:"total_tokens"`
		} `json:"usage"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Verbose struct {
			Truncated *bool   `json:"truncated"`
			Stop      *string `json:"stop_type"`
			Settings  struct {
				NPredict *int `json:"n_predict"`
			} `json:"generation_settings"`
			Timings llamaVisionTimings `json:"timings"`
		} `json:"__verbose"`
		Timings llamaVisionTimings `json:"timings"`
	}
	if json.Unmarshal(data, &wire) != nil || len(wire.Choices) != 1 || wire.Usage.Prompt == nil || wire.Usage.Completion == nil || wire.Usage.Total == nil || wire.Verbose.Truncated == nil || wire.Verbose.Stop == nil || wire.Verbose.Settings.NPredict == nil {
		return
	}
	if *wire.Usage.Prompt < 0 || *wire.Usage.Completion < 0 || *wire.Usage.Total < *wire.Usage.Prompt || *wire.Usage.Total-*wire.Usage.Prompt != *wire.Usage.Completion {
		return
	}
	switch *wire.Verbose.Stop {
	case "eos":
		out.Stop = 1
	case "word":
		out.Stop = 2
	case "limit":
		out.Stop = 3
	case "none":
		out.Stop = 4
	default:
		return
	}
	out.Valid = true
	out.Prompt, out.Completion, out.Total = *wire.Usage.Prompt, *wire.Usage.Completion, *wire.Usage.Total
	out.NPredict, out.Truncated = *wire.Verbose.Settings.NPredict, *wire.Verbose.Truncated
	out.Timings, out.VerboseTimings = wire.Timings, wire.Verbose.Timings
	out.Lexical = llamaVisionScan(wire.Choices[0].Message.Content)
	return
}

const llamaVisionRequestBudget = time.Duration(scoring.MaxTimeoutSec) * time.Second

// Single-use owned loopback bridge. Only two top-level fields are changed.
func llamaVisionForward(ctx context.Context, client *http.Client, base, key string, identity func() bool, observe func(llamaVisionSnapshot)) http.Handler {
	var used atomic.Bool
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !used.CompareAndSwap(false, true) {
			w.WriteHeader(409)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || !identity() {
			w.WriteHeader(400)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
		var wire map[string]json.RawMessage
		if err != nil || len(data) > 4<<20 || json.Unmarshal(data, &wire) != nil || wire == nil {
			w.WriteHeader(400)
			return
		}
		if _, exists := wire["max_tokens"]; exists {
			w.WriteHeader(400)
			return
		}
		wire["verbose"], wire["stream"] = json.RawMessage("true"), json.RawMessage("false")
		body, err := json.Marshal(wire)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		run, cancel := context.WithTimeout(ctx, llamaVisionRequestBudget)
		defer cancel()
		stop := context.AfterFunc(r.Context(), cancel)
		defer stop()
		req, err := http.NewRequestWithContext(run, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			w.WriteHeader(400)
			return
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		// Verbose duplicates content and includes prompt; bound memory, never log it.
		data, err = io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
		if err != nil || len(data) > 16<<20 || !identity() {
			w.WriteHeader(502)
			return
		}
		snapshot := llamaVisionMetadata(data, resp.StatusCode)
		snapshot.MessagesSHA = sha256.Sum256(wire["messages"])
		snapshot.GrammarSHA = sha256.Sum256(wire["grammar"])
		observe(snapshot)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
	})
}

// Independent opt-in: original image, candidate, grammar and ctx4096, one scoring call.
func TestLlamaRealVisionDiagnostic(t *testing.T) {
	if os.Getenv("FENGSHEN_LLAMA_REAL_VISION_DIAGNOSTIC") != "1" {
		t.Skip("explicit independent vision diagnostic opt-in required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), llamaRealTotalBudget-30*time.Second)
	defer cancel()
	root, manifest, component := llamaRealComponent(t)
	if manifest.Version != "b12182-fengshen2-cpu-avx2" {
		t.Fatal("category=component_profile_invalid")
	}
	resolved := llamaRealResolved(t, ctx, root, manifest, component, "highlight.vision", []string{os.Getenv("FENGSHEN_LLAMA_REAL_VISION_MODEL"), os.Getenv("FENGSHEN_LLAMA_REAL_PROJECTOR")})
	// Content identity is not evidence of model capability. Keep pins until cleanup.
	for i, model := range resolved.Models {
		pin, err := runtimeprocess.PinFile(model.Path)
		if err != nil {
			t.Fatal("category=diagnostic_model_pin_failed")
		}
		t.Cleanup(func() { _ = pin.Close() })
		h := sha256.New()
		buffer := make([]byte, 1<<20)
		for {
			if ctx.Err() != nil {
				t.Fatal("category=diagnostic_identity_timeout")
			}
			n, readErr := pin.Read(buffer)
			_, _ = h.Write(buffer[:n])
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				t.Fatal("category=diagnostic_model_read_failed")
			}
		}
		t.Logf("model_index=%d content_sha256=%x", i, h.Sum(nil))
	}
	// This manifest SHA was computed by llamaRealComponent, not read from model text.
	componentSHA, err := hex.DecodeString(component.ManifestSHA256)
	if err != nil || len(componentSHA) != sha256.Size {
		t.Fatal("category=diagnostic_component_identity_invalid")
	}
	t.Logf("component_manifest_sha256=%x", componentSHA)
	for i, file := range manifest.Files {
		// llamaRealResolved already verified these content hashes against pinned files.
		contentSHA, err := hex.DecodeString(file.SHA256)
		if err != nil || len(contentSHA) != sha256.Size {
			t.Fatal("category=diagnostic_component_identity_invalid")
		}
		t.Logf("component_file_index=%d content_sha256=%x", i, contentSHA)
	}
	binary, args, err := llamaPlan(resolved, true)
	if err != nil {
		t.Fatal("category=plan_invalid")
	}
	candidate := llamaRealCandidate()
	candidate.AnalysisChain, candidate.AudioFeatures = "", nil
	store, err := runtimeassets.New(t.TempDir())
	if err != nil {
		t.Fatal("category=asset_store_failed")
	}
	defer store.Close()
	picture, err := os.Open(os.Getenv("FENGSHEN_LLAMA_REAL_IMAGE"))
	if err != nil {
		t.Fatal("category=image_open_failed")
	}
	defer picture.Close()
	if !filepath.IsAbs(os.Getenv("FENGSHEN_LLAMA_REAL_IMAGE")) {
		t.Fatal("category=image_path_invalid")
	}
	st, err := picture.Stat()
	if err != nil {
		t.Fatal("category=image_stat_failed")
	}
	asset, err := store.Upload(ctx, candidate.TaskID, "real-llama-session", runtimeassets.Image, st.Size(), picture)
	if err != nil {
		t.Fatal("category=image_upload_failed")
	}
	// Use the adapter's exact image validation and encoding path.
	e := &Executor{assets: store}
	candidate.Image, err = e.llamaImage(ctx, runtimejobs.Request{TaskID: candidate.TaskID, Body: runtimejobs.Body{Vision: &runtimejobs.VisionInput{AssetID: asset.ID()}}})
	if err != nil {
		t.Fatal("category=image_invalid")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("category=port_unavailable")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	if port == 11434 {
		t.Fatal("category=port_reserved")
	}
	var keyBytes [32]byte
	if _, err := rand.Read(keyBytes[:]); err != nil {
		t.Fatal("category=key_generation_failed")
	}
	key := hex.EncodeToString(keyBytes[:])
	var supervisor runtimeprocess.Supervisor
	var drains sync.WaitGroup
	defer func() {
		cancel()
		if supervisor.Close() != nil {
			t.Error("category=owned_cleanup_failed")
		}
		drains.Wait()
	}()
	process, err := startLlama(ctx, &supervisor, binary, append(args, "--port", strconv.Itoa(port)), key)
	if err != nil {
		t.Fatal("category=start_failed")
	}
	_ = process.Stdin().Close()
	for _, src := range []io.Reader{process.Stdout(), process.Stderr()} {
		drains.Add(1)
		go func(src io.Reader) { defer drains.Done(); _, _ = io.Copy(io.Discard, src) }(src)
	}
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	if llamaReady(ctx, process, binary, base) != nil {
		t.Fatal("category=load_failed")
	}
	client := llamaHTTP()
	client.Timeout = llamaVisionRequestBudget
	defer client.CloseIdleConnections()
	snapshots := make(chan llamaVisionSnapshot, 1)
	bridge := httptest.NewServer(llamaVisionForward(ctx, client, base, key, func() bool { return process.CheckIdentity(binary) == nil }, func(s llamaVisionSnapshot) { snapshots <- s }))
	defer bridge.Close()
	c, err := scoring.NewConstrainedLocalVisionClient(scoring.LocalVisionConfig{BaseURL: bridge.URL + "/v1", APIKey: key, Model: "bound-model", TimeoutSec: scoring.MaxTimeoutSec, DisableRetries: true})
	if err != nil {
		t.Fatal("category=client_invalid")
	}
	_, scoreErr := c.ScoreEvents(ctx, candidate)
	var snapshot llamaVisionSnapshot
	select {
	case snapshot = <-snapshots:
	default:
	}
	// No raw scoring error or verbose object crosses this boundary.
	t.Logf("vision_metadata=%+v", snapshot)
	diagnostic := llamaInferenceFailure(scoreErr)
	t.Logf("attempts=%d parse_category=%q finish_reason=%q content_length=%d", diagnostic.attempts, diagnostic.parseCategory, diagnostic.finishReason, diagnostic.contentLength)
	if !snapshot.Valid {
		t.Fatal("category=metadata_unavailable")
	}
	if process.CheckIdentity(binary) != nil {
		t.Fatal("category=owned_identity_failed")
	}
}

func TestLlamaVisionLexicalOffline(t *testing.T) {
	for _, tc := range []struct {
		input                                                                      string
		end                                                                        llamaVisionEnd
		decimal, period, whitespace, longest, reason, complete, unclosed, repeated int
	}{
		{`{"events":[{"reason":"敏感\n\uD83D\uDE00`, llamaVisionString, 0, 0, 0, 0, 4, 0, 1, 0},
		{`{"events":[{"reason":"hidden\`, llamaVisionEscape, 0, 0, 0, 0, 6, 0, 1, 0},
		{`{"events":[{"reason":"hidden\u12`, llamaVisionUnicode, 0, 0, 0, 0, 6, 0, 1, 0},
		{`{"events":[{"score":0.123123123123`, llamaVisionNumber, 12, 3, 0, 0, 0, 0, 1, 0},
		{"{\"events\":[{\"score\":0.0000 \n\t ", llamaVisionWhitespace, 4, 1, 4, 4, 0, 0, 1, 0},
		{`{"events":[{"reason":"a b"}, {"reason":"a b"},{"reason":"x`, llamaVisionString, 0, 0, 1, 1, 7, 2, 1, 1},
		{`{"events":[]}`, llamaVisionComplete, 0, 0, 0, 0, 0, 0, 0, 0},
		{`{"events":[{"reason":"x","tags":["reason"," "]}]}`, llamaVisionComplete, 0, 0, 0, 0, 1, 1, 0, 0},
	} {
		got := llamaVisionScan(tc.input)
		want := llamaVisionLexical{Bytes: len(tc.input), End: tc.end, LongestDecimal: tc.decimal, NumberPeriod: tc.period, Whitespace: tc.whitespace, LongestWhitespace: tc.longest, ReasonChars: tc.reason, CompleteEvents: tc.complete, UnclosedEvents: tc.unclosed, RepeatedEvents: tc.repeated}
		if got != want {
			t.Fatalf("lexical numbers mismatch got=%+v want=%+v", got, want)
		}
	}
}

func TestLlamaVisionMetadataStrictOffline(t *testing.T) {
	const valid = `{"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12},"choices":[{"message":{"content":"private-body"}}],"__verbose":{"truncated":false,"stop_type":"eos","generation_settings":{"n_predict":-1}}}`
	for _, field := range []string{"prompt_tokens", "completion_tokens", "total_tokens", "truncated", "stop_type", "n_predict"} {
		for _, replacement := range []string{"missing", `null`, `"private-token W:/secret"`, `{}`, `[]`, `true`, `1.5`} {
			t.Run(field+"/"+replacement, func(t *testing.T) {
				var wire map[string]any
				_ = json.Unmarshal([]byte(valid), &wire)
				parent := wire["usage"].(map[string]any)
				if field == "truncated" || field == "stop_type" {
					parent = wire["__verbose"].(map[string]any)
				} else if field == "n_predict" {
					parent = wire["__verbose"].(map[string]any)["generation_settings"].(map[string]any)
				}
				if replacement == "missing" {
					delete(parent, field)
				} else {
					var value any
					_ = json.Unmarshal([]byte(replacement), &value)
					parent[field] = value
				}
				if field == "truncated" && replacement == "true" {
					return
				}
				body, _ := json.Marshal(wire)
				s := llamaVisionMetadata(body, 200)
				if s.Valid {
					t.Fatal("missing or mistyped metadata accepted")
				}
				encoded, _ := json.Marshal(s)
				if strings.Contains(string(encoded)+fmt.Sprintf("%+v %#v", s, s), "private") || strings.Contains(string(encoded), "W:/") {
					t.Fatal("untrusted text leaked")
				}
			})
		}
	}
	if !llamaVisionMetadata([]byte(valid), 200).Valid {
		t.Fatal("valid metadata rejected")
	}
	for _, body := range []string{
		`{"choices":[{"message":{"content":"private-body"},"finish_reason":"length"}]}`,
		strings.Replace(valid, `"prompt_tokens":10`, `"prompt_tokens":-1`, 1),
		strings.Replace(valid, `"total_tokens":12`, `"total_tokens":13`, 1),
	} {
		if llamaVisionMetadata([]byte(body), 200).Valid {
			t.Fatal("unavailable or inconsistent usage accepted")
		}
	}
}

func TestLlamaVisionForwardOffline(t *testing.T) {
	const secret = "hidden-private-prompt api_key=secret W:/private/model.gguf"
	content := `{"events":[{"reason":"` + secret + `","score":0.121212`
	response, _ := json.Marshal(map[string]any{
		"choices":   []any{map[string]any{"message": map[string]any{"content": content}}},
		"usage":     map[string]int{"prompt_tokens": 234, "completion_tokens": 567, "total_tokens": 801},
		"__verbose": map[string]any{"prompt": secret, "content": secret, "stop_type": "limit", "truncated": true, "generation_settings": map[string]any{"n_predict": -1, "prompt": secret}, "timings": map[string]any{"prompt_ms": 123.5, "unknown": secret}},
		"timings":   map[string]any{"predicted_n": 567, "predicted_ms": 456.5, "unknown": secret}, "unknown": secret,
	})
	original := []byte(`{"messages":[{"role":"user","content":"hidden-private-prompt"}],"grammar":"original grammar","temperature":0.2,"model":"bound-model"}`)
	calls := atomic.Int64{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var got, want map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&got) != nil {
			t.Error("request invalid")
		}
		_ = json.Unmarshal(original, &want)
		want["verbose"], want["stream"] = json.RawMessage("true"), json.RawMessage("false")
		if !reflect.DeepEqual(got, want) || r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Error("changed scoring request")
		}
		_, _ = w.Write(response)
	}))
	defer upstream.Close()
	client := llamaHTTP()
	client.Timeout = llamaVisionRequestBudget
	defer client.CloseIdleConnections()
	transport := client.Transport
	client.Transport = llamaDiagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > llamaVisionRequestBudget || time.Until(deadline) < llamaVisionRequestBudget-time.Second {
			t.Error("missing finite request budget")
		}
		return transport.RoundTrip(r)
	})
	observed := make(chan llamaVisionSnapshot, 1)
	handler := llamaVisionForward(context.Background(), client, upstream.URL, "fixture-key", func() bool { return true }, func(s llamaVisionSnapshot) { observed <- s })
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1/chat/completions", bytes.NewReader(original))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if i == 0 && (w.Code != 200 || !bytes.Equal(w.Body.Bytes(), response)) || i == 1 && w.Code != 409 {
			t.Fatal("response changed or repeated request accepted")
		}
	}
	s := <-observed
	var originalWire map[string]json.RawMessage
	_ = json.Unmarshal(original, &originalWire)
	if s.MessagesSHA != sha256.Sum256(originalWire["messages"]) || s.GrammarSHA != sha256.Sum256(originalWire["grammar"]) || s.Total != 801 {
		t.Fatal("wrong request identity or total usage")
	}
	if calls.Load() != 1 || !s.Valid || s.Prompt != 234 || s.Completion != 567 || s.NPredict != -1 || !s.Truncated || s.Stop != 3 || s.Timings.PredictedMS != 456.5 || s.VerboseTimings.PromptMS != 123.5 || s.Lexical.LongestDecimal != 6 || s.Lexical.NumberPeriod != 2 {
		t.Fatal("wrong verbose mapping or request count")
	}
	encoded, _ := json.Marshal(s)
	formatted := string(encoded) + fmt.Sprintf("%+v %#v", s, s)
	for _, forbidden := range []string{"hidden", "private", "secret", "fixture-key", "original grammar"} {
		if strings.Contains(formatted, forbidden) {
			t.Fatal("snapshot leaked sensitive content")
		}
	}
	for _, stop := range []struct {
		input string
		want  uint8
	}{{"eos", 1}, {"word", 2}, {"none", 4}, {secret, 0}} {
		body := bytes.Replace(response, []byte(`"stop_type":"limit"`), []byte(`"stop_type":`+strconv.Quote(stop.input)), 1)
		if llamaVisionMetadata(body, 200).Stop != stop.want {
			t.Fatal("unsafe stop enum")
		}
	}
	for _, body := range [][]byte{[]byte("raw-error-" + secret), []byte(`{"timings":{"prompt_ms":"hidden"}}`)} {
		if llamaVisionMetadata(body, 200).Valid || llamaVisionMetadata(response, 503).Valid {
			t.Fatal("invalid metadata accepted")
		}
	}
	// A numeric/boolean/enum-only recursive shape prevents accidental raw storage.
	var safe func(reflect.Type) bool
	safe = func(typ reflect.Type) bool {
		switch typ.Kind() {
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				if !safe(typ.Field(i).Type) {
					return false
				}
			}
			return true
		case reflect.Array:
			return typ == reflect.TypeOf([sha256.Size]byte{})
		case reflect.Bool, reflect.Int, reflect.Uint8, reflect.Float64:
			return true
		}
		return false
	}
	if !safe(reflect.TypeOf(s)) {
		t.Fatal("snapshot permits sensitive storage")
	}
	if llamaVisionRequestBudget != 600*time.Second || llamaRealTotalBudget-30*time.Second <= llamaVisionRequestBudget+2*time.Minute {
		t.Fatal("invalid total or request budget")
	}
}

func TestLlamaVisionForwardFailureOffline(t *testing.T) {
	var calls atomic.Int64
	client := llamaHTTP()
	defer client.CloseIdleConnections()
	client.Transport = llamaDiagnosticRoundTripper(func(r *http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("raw-secret") })
	for _, body := range []string{`{"max_tokens":1}`, `{broken`, `{}`} {
		h := llamaVisionForward(context.Background(), client, "http://127.0.0.1:1", "fixture-key", func() bool { return true }, func(llamaVisionSnapshot) { t.Error("unexpected metadata") })
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if strings.Contains(w.Body.String(), "secret") || (body == `{}` && w.Code != 502) || (body != `{}` && w.Code != 400) {
			t.Fatal("unsafe error mapping")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid request forwarded or transport retried")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	client.Transport = llamaDiagnosticRoundTripper(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	h := llamaVisionForward(ctx, client, "http://127.0.0.1:1", "fixture-key", func() bool { return true }, func(llamaVisionSnapshot) { t.Error("unexpected metadata") })
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)))
	if w.Code != 502 || ctx.Err() != context.DeadlineExceeded {
		t.Fatal("forward escaped parent deadline")
	}
}

func TestLlamaVisionScoringFlowOffline(t *testing.T) {
	candidate := llamaRealCandidate()
	candidate.AnalysisChain, candidate.AudioFeatures = "", nil
	var picture bytes.Buffer
	if jpeg.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil) != nil {
		t.Fatal("image fixture invalid")
	}
	candidate.Image = &scoring.ImageInput{MIMEType: "image/jpeg", DataBase64: base64.StdEncoding.EncodeToString(picture.Bytes())}
	calls := atomic.Int64{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var wire map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&wire) != nil {
			t.Error("request invalid")
		}
		var grammar string
		_ = json.Unmarshal(wire["grammar"], &grammar)
		if string(wire["verbose"]) != "true" || string(wire["stream"]) != "false" || wire["max_tokens"] != nil || !strings.Contains(grammar, `score ::= ("0" ("." [0-9]+)?`) || !bytes.Contains(wire["messages"], []byte(`image_url`)) {
			t.Error("production constrained request changed")
		}
		content, _ := scoring.ContractContent()
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 200, "total_tokens": 300}, "__verbose": map[string]any{"truncated": false, "stop_type": "eos", "generation_settings": map[string]int{"n_predict": -1}}})
	}))
	defer upstream.Close()
	client := llamaHTTP()
	defer client.CloseIdleConnections()
	observed := make(chan llamaVisionSnapshot, 1)
	bridge := httptest.NewServer(llamaVisionForward(context.Background(), client, upstream.URL, "fixture-key", func() bool { return true }, func(s llamaVisionSnapshot) { observed <- s }))
	defer bridge.Close()
	c, err := scoring.NewConstrainedLocalVisionClient(scoring.LocalVisionConfig{BaseURL: bridge.URL + "/v1", APIKey: "fixture-key", Model: "bound-model", TimeoutSec: 2, DisableRetries: true})
	if err != nil {
		t.Fatal("client setup invalid")
	}
	events, err := c.ScoreEvents(context.Background(), candidate)
	if err != nil || len(events) != 2 || calls.Load() != 1 {
		t.Fatal("production scoring flow failed")
	}
	if s := <-observed; !s.Valid || s.Lexical.CompleteEvents != 2 || s.Stop != 1 {
		t.Fatal("production response metadata invalid")
	}
}

func TestLlamaVisionIndependentOptInOffline(t *testing.T) {
	t.Setenv("FENGSHEN_LLAMA_REAL_COMPONENT_ROOT", "")
	t.Setenv("FENGSHEN_LLAMA_REAL_INFERENCE", "1")
	for _, value := range []string{"", "0", "true"} {
		t.Setenv("FENGSHEN_LLAMA_REAL_VISION_DIAGNOSTIC", value)
		t.Run("disabled-"+value, TestLlamaRealVisionDiagnostic)
	}
}

func TestLlamaRealInference(t *testing.T) {
	if os.Getenv("FENGSHEN_LLAMA_REAL_INFERENCE") != "1" {
		t.Skip("explicit real inference opt-in required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), llamaRealTotalBudget-30*time.Second)
	defer cancel()
	root, m, component := llamaRealComponent(t)
	for _, scene := range []string{"highlight.score", "highlight.vision"} {
		t.Run(scene, func(t *testing.T) {
			vision := scene == "highlight.vision"
			paths := filepath.SplitList(os.Getenv("FENGSHEN_LLAMA_REAL_TEXT_MODEL"))
			if len(paths) == 0 && !vision {
				t.Fatal("explicit absolute text model paths required")
			}
			if vision {
				paths = []string{os.Getenv("FENGSHEN_LLAMA_REAL_VISION_MODEL"), os.Getenv("FENGSHEN_LLAMA_REAL_PROJECTOR")}
			}
			resolved := llamaRealResolved(t, ctx, root, m, component, scene, paths)
			ref := resolved.Binding.Ref
			candidate := llamaRealCandidate()
			if vision {
				candidate.AnalysisChain = ""
				candidate.AudioFeatures = nil
			}
			raw, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal("category=candidate_encode_failed")
			}
			input, err := runtimejobs.ScoreInputFromJSON(raw)
			if err != nil {
				t.Fatal("category=candidate_invalid")
			}
			store, err := runtimeassets.New(t.TempDir())
			if err != nil {
				t.Fatal("category=asset_store_failed")
			}
			defer store.Close()
			req := runtimejobs.Request{TaskID: candidate.TaskID, RequestID: candidate.RequestID, SessionID: "real-llama-session", Binding: ref, Body: runtimejobs.Body{Component: component, Score: &input}}
			if vision {
				imagePath := os.Getenv("FENGSHEN_LLAMA_REAL_IMAGE")
				if !filepath.IsAbs(imagePath) {
					t.Fatal("explicit absolute image path required")
				}
				picture, err := os.Open(imagePath)
				if err != nil {
					t.Fatal("category=image_open_failed")
				}
				defer picture.Close()
				st, err := picture.Stat()
				if err != nil {
					t.Fatal("category=image_stat_failed")
				}
				asset, err := store.Upload(ctx, req.TaskID, req.SessionID, runtimeassets.Image, st.Size(), picture)
				if err != nil {
					t.Fatalf("category=image_upload_failed context=%s", llamaHarnessContextCategory(ctx, err))
				}
				req.Body.Score = nil
				req.Body.Vision = &runtimejobs.VisionInput{AssetID: asset.ID(), Candidate: input}
			}
			e, err := New(func(context.Context, integration.ExecutionBindingRef, integration.ExecutionComponentRef) (Resolved, error) {
				return resolved, nil
			}, store, t.TempDir())
			if err != nil {
				t.Fatal("category=executor_setup_failed")
			}
			defer func() {
				if e.Close() != nil {
					t.Error("category=owned_cleanup_failed")
				}
			}()
			loading, running := false, false
			stage, stageStart := "setup", time.Now()
			result, err := e.Execute(ctx, req, func(p runtimejobs.Progress) error {
				next := "unknown"
				switch p.Stage {
				case runtimejobs.StageLoading:
					loading, next = true, "loading"
				case runtimejobs.StageRunning:
					running, next = true, "running"
				}
				t.Logf("stage=%s elapsed_ms=%d", stage, time.Since(stageStart).Milliseconds())
				stage, stageStart = next, time.Now()
				return nil
			})
			t.Logf("stage=%s elapsed_ms=%d", stage, time.Since(stageStart).Milliseconds())
			if err != nil {
				if category := llamaHarnessContextCategory(ctx, err); category != "unknown" {
					t.Fatalf("category=%s", category)
				}
				var diagnostic *llamaInferenceError
				if errors.As(err, &diagnostic) {
					t.Fatalf("real adapter inference scene=%s: center_llama_inference_failed; scoring kind=%q code=%q status_code=%d attempts=%d parse_category=%q finish_reason=%q content_length=%d event_category=%q event_count=%d invalid_event_count=%d first_invalid_event_index=%d score_field=%q score_reason=%q", scene, diagnostic.kind, diagnostic.code, diagnostic.statusCode, diagnostic.attempts, diagnostic.parseCategory, diagnostic.finishReason, diagnostic.contentLength, diagnostic.eventCategory, diagnostic.eventCount, diagnostic.invalidEventCount, diagnostic.firstInvalidEventIndex, diagnostic.scoreField, diagnostic.scoreReason)
				}
				var resultDiagnostic *llamaResultError
				if errors.As(err, &resultDiagnostic) {
					t.Fatalf("real adapter inference scene=%s: center_result_invalid; event_category=%q event_count=%d invalid_event_count=%d", scene, resultDiagnostic.eventCategory, resultDiagnostic.eventCount, resultDiagnostic.invalidEventCount)
				}
				t.Fatal("real adapter inference failed; category=unknown")
			}
			if !loading || !running || result.Component != component || result.Events == nil {
				t.Fatal("real inference returned incomplete result")
			}
			t.Logf("real adapter inference scene=%s events=%d; contract input is synthetic, not a source-video highlight", scene, len(result.Events))
		})
	}
}
