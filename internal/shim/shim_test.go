package shim

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
	"github.com/coder/websocket"
)

func authHeader() http.Header { return http.Header{} }

func TestJobLifecycle(t *testing.T) {
	start := func(ctx context.Context, target string, args []string, lim Limits) (Child, error) {
		out := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "-of" {
				out = args[i+1]
			}
		}
		if out == "" {
			return nil, fmt.Errorf("missing -of")
		}
		return &fakeChild{wait: func() error { return os.WriteFile(out+".json", []byte(`{"ok":true}`), 0600) }}, nil
	}
	s, err := New(Config{Mode: ModeWhisperBatch, Target: fakeExe(t), WorkRoot: t.TempDir(), StartChild: start})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	audio := filepath.Join(t.TempDir(), "a.wav")
	model := filepath.Join(t.TempDir(), "m.bin")
	if err := os.WriteFile(audio, []byte("RIFF"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("model"), 0600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"protocol":"fengshen.batch.http/v1","kind":"whisper-batch","request_id":"r1","input":{"audio_path":%q},"models":{"whisper-asr":[%q]},"options":{"vad":false}}`, audio, model)
	req, _ := http.NewRequest("POST", ts.URL+"/v1/jobs", strings.NewReader(body))
	req.Header = authHeader()
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit status=%d", resp.StatusCode)
	}
	var snap componentprotocol.JobSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, _ := http.NewRequest("GET", ts.URL+"/v1/jobs/"+snap.ID, nil)
		r.Header = authHeader()
		got, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewDecoder(got.Body).Decode(&snap)
		got.Body.Close()
		if snap.State.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not finish: %+v", snap)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if snap.State != componentprotocol.JobSucceeded || len(snap.Artifacts) != 1 || snap.Artifacts[0] != "transcript.json" {
		t.Fatalf("unexpected snapshot %+v", snap)
	}
	for _, path := range []string{"/v1/jobs/" + snap.ID + "/artifacts/transcript.json", "/v1/jobs/" + snap.ID + "/artifacts/..%2Fsecret"} {
		r, _ := http.NewRequest("GET", ts.URL+path, nil)
		r.Header = authHeader()
		got, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		got.Body.Close()
		if strings.Contains(path, "secret") {
			if got.StatusCode != http.StatusNotFound {
				t.Fatalf("traversal artifact status=%d", got.StatusCode)
			}
		} else if got.StatusCode != http.StatusOK {
			t.Fatalf("artifact status=%d", got.StatusCode)
		}
	}
}

func TestStreamBridge(t *testing.T) {
	start := func(ctx context.Context, target string, args []string, lim Limits) (Child, error) {
		return newEchoChild(), nil
	}
	s, err := New(Config{Mode: ModeWhisperStream, Target: fakeExe(t), WorkRoot: t.TempDir(), StartChild: start})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/stream"
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: authHeader()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	model := filepath.Join(t.TempDir(), "m.bin")
	startMsg := fmt.Sprintf(`{"protocol":"fengshen.stream.ws/v1","type":"start","request_id":"r1","episode_number":1,"duration_sec":2,"format":"s16le/16000/1","models":{"whisper-asr":%q}}`, model)
	if err := c.Write(ctx, websocket.MessageText, []byte(startMsg)); err != nil {
		t.Fatal(err)
	}
	if typ, data, err := c.Read(ctx); err != nil || typ != websocket.MessageText || !bytes.Contains(data, []byte(`"type":"ready"`)) {
		t.Fatalf("ready: %v %s", err, data)
	}
	audio := `{"protocol":"fengshen.stream.ws/v1","type":"audio","request_id":"r1","episode_number":1,"revision":1,"start_sec":0,"end_sec":2,"sample_count":32000,"pcm":"AAAA"}`
	if err := c.Write(ctx, websocket.MessageText, []byte(audio)); err != nil {
		t.Fatal(err)
	}
	if _, data, err := c.Read(ctx); err != nil || !bytes.Contains(data, []byte(`"type":"hypothesis"`)) {
		t.Fatalf("hypothesis: %v %s", err, data)
	}
	final := `{"protocol":"fengshen.stream.ws/v1","type":"finalize","request_id":"r1","episode_number":1,"revision":2,"committed_through_sec":2}`
	if err := c.Write(ctx, websocket.MessageText, []byte(final)); err != nil {
		t.Fatal(err)
	}
	if _, data, err := c.Read(ctx); err != nil || !bytes.Contains(data, []byte(`"type":"final"`)) {
		t.Fatalf("final: %v %s", err, data)
	}
}

func TestHaarClassify(t *testing.T) {
	s, err := New(Config{Mode: ModeHaar, WorkRoot: t.TempDir(), FrontalXML: haarXML(t, "haarcascade_frontalface_default.xml"), ProfileXML: haarXML(t, "haarcascade_profileface.xml")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: 128, G: 128, B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/v1/face/classify", bytes.NewReader(buf.Bytes()))
	req.Header = authHeader()
	req.Header.Set("Content-Type", "image/png")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("classify status=%d", resp.StatusCode)
	}
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out["kind"] == "" || out["algorithm"] != "haar/v1" {
		t.Fatalf("unexpected response %v %+v", err, out)
	}
}

type fakeChild struct {
	wait   func() error
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func (c *fakeChild) Stdin() io.WriteCloser {
	if c.stdin == nil {
		return nopWriteCloser{io.Discard}
	}
	return c.stdin
}
func (c *fakeChild) Stdout() io.Reader {
	if c.stdout == nil {
		return strings.NewReader("")
	}
	return c.stdout
}
func (c *fakeChild) Stderr() io.Reader {
	if c.stderr == nil {
		return strings.NewReader("")
	}
	return c.stderr
}
func (c *fakeChild) Wait() error {
	if c.wait != nil {
		return c.wait()
	}
	return nil
}
func (c *fakeChild) Close() error { return nil }

func newEchoChild() *fakeChild {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(inR)
		for scanner.Scan() {
			var m map[string]any
			if json.Unmarshal(scanner.Bytes(), &m) != nil {
				continue
			}
			reply := map[string]any{"protocol": componentprotocol.StreamWSv1, "request_id": "r1", "episode_number": 1, "revision": 1, "items": []any{"a"}[:0]}
			switch m["type"] {
			case "start":
				reply["type"] = "ready"
			case "audio":
				reply["type"] = "hypothesis"
				reply["committed_through_sec"] = m["end_sec"]
			case "finalize":
				reply["type"] = "final"
				reply["committed_through_sec"] = m["committed_through_sec"]
				reply["final"] = true
			}
			raw, _ := json.Marshal(reply)
			_, _ = outW.Write(append(raw, '\n'))
			if m["type"] == "finalize" {
				_ = outW.Close()
				return
			}
		}
	}()
	return &fakeChild{stdin: inW, stdout: outR, stderr: io.NopCloser(bytes.NewReader(nil)), wait: func() error { <-done; return nil }}
}

func fakeExe(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "component.exe")
	if err := os.WriteFile(p, []byte("fake"), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func haarXML(t *testing.T, name string) string {
	t.Helper()
	dir := os.Getenv("FENGSHEN_HAAR_TEST_DIR")
	if dir == "" {
		t.Skip("set FENGSHEN_HAAR_TEST_DIR to a directory containing the Haar XMLs")
	}
	p, err := filepath.Abs(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLlamaShimStartProxyStop(t *testing.T) {
	start := func(ctx context.Context, target string, args []string, lim Limits) (Child, error) {
		port := 0
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--port" {
				port, _ = strconv.Atoi(args[i+1])
			}
		}
		if port == 0 {
			return nil, fmt.Errorf("missing --port")
		}
		listener, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return nil, err
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		})
		mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
		srv := &http.Server{Handler: mux}
		go func() { _ = srv.Serve(listener) }()
		return &fakeLlamaChild{srv: srv}, nil
	}
	s, err := New(Config{Mode: ModeLlama, Target: fakeExe(t), WorkRoot: t.TempDir(), StartChild: start})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	model := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(model, []byte("gguf"), 0600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"protocol":"fengshen.llama.http/v1","model":%q}`, model)
	req, _ := http.NewRequest("POST", ts.URL+"/v1/llama/start", strings.NewReader(body))
	req.Header = authHeader()
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status=%d", resp.StatusCode)
	}
	for _, path := range []string{"/health", "/v1/chat/completions"} {
		method := "GET"
		if strings.Contains(path, "chat") {
			method = "POST"
		}
		r, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(`{}`))
		r.Header = authHeader()
		got, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		got.Body.Close()
		if got.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d", path, got.StatusCode)
		}
	}
	r, _ := http.NewRequest("POST", ts.URL+"/v1/llama/stop", nil)
	r.Header = authHeader()
	got, err := http.DefaultClient.Do(r)
	if err != nil || got.StatusCode != http.StatusOK {
		t.Fatal(err, got.StatusCode)
	}
	got.Body.Close()
	h, _ := http.NewRequest("GET", ts.URL+"/health", nil)
	h.Header = authHeader()
	after, err := http.DefaultClient.Do(h)
	if err != nil {
		t.Fatal(err)
	}
	after.Body.Close()
	if after.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("health after stop=%d", after.StatusCode)
	}
}

type fakeLlamaChild struct{ srv *http.Server }

func (c *fakeLlamaChild) Stdin() io.WriteCloser { return nopWriteCloser{io.Discard} }
func (c *fakeLlamaChild) Stdout() io.Reader     { return strings.NewReader("") }
func (c *fakeLlamaChild) Stderr() io.Reader     { return strings.NewReader("") }
func (c *fakeLlamaChild) Wait() error           { select {} }
func (c *fakeLlamaChild) Close() error          { return c.srv.Close() }
