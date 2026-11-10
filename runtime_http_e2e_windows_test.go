//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/components"
	"github.com/aura-bootstrap/fengshen_model_center/integration"
	"github.com/aura-bootstrap/fengshen_model_center/registry"
	"github.com/aura-bootstrap/fengshen_model_center/safefs"
)

const runtimeHTTPOptIn = "FENGSHEN_CENTER_RUNTIME_HTTP_E2E"

// This is a trusted developer test driver, not a user-engine authorization test.
// Build and trust it using installer/test_model_center_runtime_http.ps1 only.
func TestRuntimeHTTPE2E(t *testing.T) {
	if !runtimeHTTPEnabled(os.Getenv(runtimeHTTPOptIn)) {
		t.Skip("explicit opt-in required: FENGSHEN_CENTER_RUNTIME_HTTP_E2E=1; real HTTP inference is not tested by default")
	}
	root := runtimeHTTPPath(t, "FENGSHEN_CENTER_RUNTIME_HTTP_ROOT", true)
	repo := runtimeHTTPPath(t, "FENGSHEN_CENTER_RUNTIME_HTTP_REPO", true)
	messi := runtimeHTTPPath(t, "FENGSHEN_CENTER_RUNTIME_HTTP_FACE_JPEG", false)
	if !strings.EqualFold(filepath.Base(messi), "messi5.jpg") {
		t.Fatal("FACE_JPEG must explicitly select the real messi5.jpg")
	}
	profile := ""
	if os.Getenv("FENGSHEN_CENTER_RUNTIME_HTTP_PROFILE_JPEG") != "" {
		profile = runtimeHTTPPath(t, "FENGSHEN_CENTER_RUNTIME_HTTP_PROFILE_JPEG", false)
	}
	self, err := os.Executable()
	if err != nil || !sameScanPath(self, filepath.Join(root, "runtime-http-driver.exe")) {
		t.Fatal("run only the freshly compiled, delivery-trusted runtime-http-driver.exe")
	}
	batch, stream, err := runtimeHTTPModes(os.Getenv("FENGSHEN_CENTER_RUNTIME_HTTP_BATCH"), os.Getenv("FENGSHEN_CENTER_RUNTIME_HTTP_STREAM"))
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"haar-frontal": filepath.Join(repo, "assets", "haar", "haarcascade_frontalface_default.xml"),
		"haar-profile": filepath.Join(repo, "assets", "haar", "haarcascade_profileface.xml"),
	}
	for _, path := range paths {
		runtimeHTTPRegular(t, path)
	}
	speech := ""
	if batch || stream {
		paths["whisper-asr"] = runtimeHTTPPath(t, "FENGSHEN_CENTER_RUNTIME_HTTP_ASR", false)
		speech = runtimeHTTPPath(t, "FENGSHEN_CENTER_RUNTIME_HTTP_SPEECH_WAV", false)
	}
	if stream {
		paths["whisper-vad"] = runtimeHTTPPath(t, "FENGSHEN_CENTER_RUNTIME_HTTP_VAD", false)
	}
	center := filepath.Join(root, "model-center", "fengshen-model-center.exe")
	centerSHA := runtimeHTTPFileSHA(t, center)
	deliverySHA := runtimeHTTPFileSHA(t, filepath.Join(root, "model-center", "delivery.json"))
	// All writable state is new and below this run's unique external build root.
	stateRoot, err := os.MkdirTemp(root, "runtime-http-state-")
	if err != nil {
		t.Fatal(err)
	}
	data, discovery := filepath.Join(stateRoot, "registration"), filepath.Join(stateRoot, "discovery")
	for _, path := range []string{data, discovery} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := integration.Config{InstallRoot: root, Root: root, RootKind: "installed", DataDir: discovery, CenterSHA256: centerSHA, DeliverySHA256: deliverySHA}
	client := e2eClient(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	watcher := e2eWatch(t, center)
	defer watcher.close(t)
	cmd := exec.Command(center, "-listen", "127.0.0.1:0", "-root", root, "-root-kind", "installed", "-install-root", root, "-data", data, "-integration-data", discovery, "-center-sha256", centerSHA, "-delivery-sha256", deliverySHA)
	// Retain only validated descriptor phases, never raw child output.
	diagnostics := &runtimeHTTPDiagnostics{}
	cmd.Stdout = &runtimeHTTPDiagnosticWriter{capture: diagnostics}
	cmd.Stderr = &runtimeHTTPDiagnosticWriter{capture: diagnostics}
	defer func() {
		if t.Failed() {
			t.Logf("safe descriptor phases (done means returned, not success):\n%s", diagnostics.snapshot())
		}
	}()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		// watcher.close owns termination through retained kernel handles, not a PID.
		watcher.scan()
	}()
	var desc integration.Descriptor
	startupCtx, startupCancel := context.WithTimeout(ctx, 30*time.Second)
	defer startupCancel()
	runtimeHTTPWait(t, startupCtx, func() bool {
		select {
		case <-exited:
			t.Fatal("fresh child exited before descriptor confirmation")
		default:
		}
		raw, err := os.ReadFile(filepath.Join(discovery, integration.DescriptorName))
		if err != nil || json.Unmarshal(raw, &desc) != nil {
			return false
		}
		watcher.scan()
		return desc.PID == uint32(cmd.Process.Pid) && watcher.count() == 1 && watcher.owns(desc.PID, desc.ProcessStartedAt)
	})
	// Never call EnsureRunning before a descriptor for the exact owned child exists:
	// otherwise its production fallback could launch on fixed port 18120.
	status, err := client.EnsureRunning(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.InstanceID != desc.InstanceID || status.IdentityState != "confirmed" || status.HealthState != "ready" || status.URL != "http://"+desc.LoopbackAddress+"/" {
		t.Fatal("fresh child identity/readiness mismatch")
	}
	t.Logf("owned center confirmed: pid=%d created=%s instance=%s address=%s center_sha256=%s delivery_sha256=%s state=%s", desc.PID, desc.ProcessStartedAt, desc.InstanceID, desc.LoopbackAddress, centerSHA, deliverySHA, stateRoot)
	browser := &runtimeHTTPBrowser{t: t, ctx: ctx, base: strings.TrimSuffix(status.URL, "/"), http: integration.SessionHTTPClient(30 * time.Second)}
	defer browser.http.CloseIdleConnections()
	browser.loadToken()
	var state State
	browser.json("GET", "/api/state", nil, 200, &state)
	if state.Registry.Revision != 0 || len(state.Registry.Registrations) != 0 || state.CurrentInventory != "" {
		t.Fatal("expected a completely new registration store and no scan")
	}
	// Register every requested model before scanning; later writes invalidate inventory.
	for _, id := range []string{"haar-frontal", "haar-profile", "whisper-asr", "whisper-vad"} {
		path, ok := paths[id]
		if !ok {
			continue
		}
		var snapshot registry.Snapshot
		browser.json("PUT", "/api/registrations/"+id, map[string]any{"path": path, "expected_revision": state.Registry.Revision}, 200, &snapshot)
		if snapshot.Revision != state.Registry.Revision+1 || !sameScanPath(snapshot.Registrations[id].ResolvedPath, path) {
			t.Fatal("registration revision/path mismatch")
		}
		state.Registry = snapshot
	}
	scan := browser.operation("/api/operations/scan", map[string]any{"registry_revision": state.Registry.Revision})
	var inventory Inventory
	browser.json("GET", "/api/inventories/"+scan.ResultID, nil, 200, &inventory)
	if !inventory.Complete || inventory.Stale || inventory.RegistryRevision != state.Registry.Revision {
		t.Fatal("scan did not produce current complete inventory")
	}
	selections := map[string]integration.BindingFileSelection{}
	for _, id := range []string{"haar-frontal", "haar-profile", "whisper-asr", "whisper-vad"} {
		path, ok := paths[id]
		if !ok {
			continue
		}
		// File/group identity comes from actual scan output, never a fixture or guess.
		var page struct {
			Files []ModelFile `json:"files"`
			Next  string      `json:"next_cursor"`
		}
		browser.json("GET", "/api/inventories/"+inventory.ID+"/models/"+id+"/files", nil, 200, &page)
		matches := []ModelFile{}
		for _, file := range page.Files {
			if sameScanPath(file.Path, path) && file.Evidence == "recognized_by_rule" && file.ID != "" && file.Group != "" && file.Identity != "" {
				matches = append(matches, file)
			}
		}
		if page.Next != "" || len(matches) != 1 {
			t.Fatalf("%s: exact registered real file/group is missing or ambiguous", id)
		}
		file := matches[0]
		verified := browser.operation("/api/operations/verify", map[string]any{"inventory_id": inventory.ID, "model_id": id, "file_ids": []string{file.ID}})
		if len(verified.Hashes) != 1 || verified.Hashes[0].FileID != file.ID || verified.Hashes[0].Status != "fingerprint_computed" || len(verified.Hashes[0].SHA256) != 64 {
			t.Fatalf("%s: real file verification failed", id)
		}
		selections[id] = integration.BindingFileSelection{ModelID: id, FileID: file.ID}
	}
	var evidence []components.Evidence
	browser.json("GET", "/api/components", nil, 200, &evidence)
	refs := map[string]integration.ExecutionComponentRef{}
	ids := []string{"haar-consumer"}
	if batch {
		ids = append(ids, "whisper-batch")
	}
	if stream {
		ids = append(ids, "whisper-stream")
	}
	for _, id := range ids {
		candidates := []components.Evidence{}
		for _, item := range evidence {
			if item.ComponentID == id && item.Manifest != nil {
				candidates = append(candidates, item)
			}
		}
		if len(candidates) != 1 {
			t.Fatalf("%s: require one genuine delivered component", id)
		}
		item := candidates[0]
		verified := browser.operation("/api/operations/verify-component", map[string]any{"component_id": id, "version": item.Version})
		e := verified.Evidence
		if e == nil || e.Manifest == nil || e.Integrity != "matched" || e.StartupTest != "passed" || e.FilesState != "complete" || e.Platform != "matched" {
			t.Fatalf("%s: real component verification failed", id)
		}
		protocol := "haar/v1"
		if id == "whisper-batch" {
			protocol = "fengshen.whispercpp.batch/v1"
		} else if id == "whisper-stream" {
			protocol = "fengshen.whispercpp.stream/v1"
		}
		declared := false
		for _, p := range e.Manifest.Protocols {
			declared = declared || p == protocol
		}
		if !declared {
			t.Fatal("component lacks required actual protocol")
		}
		refs[id] = integration.ExecutionComponentRef{ID: id, Version: e.Version, ManifestSHA256: e.ManifestSHA256, Protocol: protocol}
		if id == "haar-consumer" {
			copyPath := filepath.Join(root, "model-center", "components", id, e.Version, filepath.FromSlash(e.Manifest.Entry))
			if runtimeHTTPFileSHA(t, copyPath) != centerSHA {
				t.Fatal("Haar delivery must be the exact same-byte center image")
			}
		}
	}
	bindings, err := client.Bindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	put := func(scene, component string, models ...string) integration.BindingView {
		files := []integration.BindingFileSelection{}
		for _, id := range models {
			files = append(files, selections[id])
		}
		next, err := client.PutBinding(ctx, scene, integration.BindingSelection{ExpectedRevision: bindings.Revision, InventoryID: inventory.ID, Files: files, Component: refs[component], Device: "cpu"})
		if err != nil {
			t.Fatal(err)
		}
		bindings = next
		view, ok := next.Bindings[scene]
		if !ok || view.Ref.Validate() != nil || view.Component != refs[component] {
			t.Fatal("trusted binding identity mismatch")
		}
		return view
	}
	face := put("image.face", "haar-consumer", "haar-frontal", "haar-profile")
	runtimeClient := integration.NewRuntimeClient(client)
	defer runtimeClient.CloseIdleConnections()
	session := runtimeHTTPOpen(t, ctx, client, face.Ref)
	defer runtimeHTTPClose(t, client, session)
	faceBytes, err := os.ReadFile(messi)
	if err != nil {
		t.Fatal(err)
	}
	asset, err := runtimeClient.Upload(ctx, session, "image/jpeg", int64(len(faceBytes)), bytes.NewReader(faceBytes))
	if err != nil {
		t.Fatal(err)
	}
	body := integration.RuntimeBody{Component: face.Component, Face: &integration.RuntimeAssetInput{AssetID: asset.ID}}
	// Cross-task rejection is sent over HTTP with a second genuine pipe session.
	other := runtimeHTTPOpen(t, ctx, client, face.Ref)
	defer runtimeHTTPClose(t, client, other)
	_, err = runtimeClient.Submit(ctx, other, opaqueID(), face.Ref, body)
	runtimeHTTPRejected(t, err)
	// Browser/management credentials and a wrong runtime credential cannot infer.
	raw, _ := json.Marshal(map[string]any{"request_id": opaqueID(), "binding": face.Ref, "body": body})
	browser.reject("POST", integration.RuntimeJobsPath, raw, nil)
	browser.reject("POST", integration.RuntimeJobsPath, raw, map[string]string{integration.RuntimeHeader: strings.Repeat("0", 64)})
	management, err := client.OpenManagementSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, "POST", browser.base+integration.RuntimeJobsPath, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	// A valid management token is obtained on an allowed route, then presented to
	// the runtime route to test the real server permission matrix (not local guards).
	auth, err := http.NewRequestWithContext(ctx, "GET", browser.base+integration.BindingsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AuthenticateSessionRequest(management, auth); err != nil {
		t.Fatal(err)
	}
	request.Header = auth.Header.Clone()
	request.Header.Set("Content-Type", "application/json")
	response, err := browser.http.Do(request)
	if err != nil {
		t.Fatal("management negative transport failed")
	}
	response.Body.Close()
	if response.StatusCode != 401 && response.StatusCode != 403 {
		t.Fatalf("management runtime request not rejected: %d", response.StatusCode)
	}
	runtimeHTTPClose(t, client, management)
	result := runtimeHTTPRun(t, ctx, client, runtimeClient, session, face, body, false)
	if result.Face != integration.RuntimeFrontal {
		t.Fatalf("real messi5.jpg: got %s, want frontal", result.Face)
	}
	var blank bytes.Buffer
	if err := png.Encode(&blank, image.NewGray(image.Rect(0, 0, 320, 240))); err != nil {
		t.Fatal(err)
	}
	blankAsset, err := runtimeClient.Upload(ctx, session, "image/png", int64(blank.Len()), bytes.NewReader(blank.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	body.Face = &integration.RuntimeAssetInput{AssetID: blankAsset.ID}
	result = runtimeHTTPRun(t, ctx, client, runtimeClient, session, face, body, false)
	if result.Face != integration.RuntimeNoFace {
		t.Fatalf("real blank PNG: got %s, want no_face", result.Face)
	}
	t.Log("Haar HTTP inference passed: real messi5.jpg=frontal, generated blank PNG=no_face; cross-task and wrong/browser/management credentials rejected; profile is separate")
	t.Run("real_profile", func(t *testing.T) {
		if profile == "" {
			t.Skip("profile HTTP NOT RUN: optional explicit PROFILE_JPEG with reviewed genuine source/timestamp/SHA required; no synthetic images")
		}
		data, mirrored := runtimeHTTPProfileImages(t, profile)
		t.Logf("profile source=%s sha256=%x; mirror is a lossless PNG derivative, not a second real sample", profile, sha256.Sum256(data))
		for _, sample := range []struct {
			name, mime string
			data       []byte
		}{{"original", "image/jpeg", data}, {"horizontal_reflection", "image/png", mirrored}} {
			t.Run(sample.name, func(t *testing.T) {
				a, err := runtimeClient.Upload(ctx, session, sample.mime, int64(len(sample.data)), bytes.NewReader(sample.data))
				if err != nil {
					t.Fatal(err)
				}
				input := integration.RuntimeBody{Component: face.Component, Face: &integration.RuntimeAssetInput{AssetID: a.ID}}
				got := runtimeHTTPRun(t, ctx, client, runtimeClient, session, face, input, false)
				if got.Face != integration.RuntimeProfile {
					t.Fatalf("profile HTTP got=%s want=profile input_sha256=%x", got.Face, sha256.Sum256(sample.data))
				}
				t.Logf("Haar HTTP profile passed: input_sha256=%x", sha256.Sum256(sample.data))
			})
		}
	})
	runWhisper := func(transcribe integration.BindingView, streaming bool) {
		s := runtimeHTTPOpen(t, ctx, client, transcribe.Ref)
		defer runtimeHTTPClose(t, client, s)
		wav, err := os.Open(speech)
		if err != nil {
			t.Fatal(err)
		}
		defer wav.Close()
		info, err := wav.Stat()
		if err != nil {
			t.Fatal(err)
		}
		a, err := runtimeClient.Upload(ctx, s, "audio/wav", info.Size(), wav)
		if err != nil {
			t.Fatal(err)
		}
		input := integration.RuntimeBody{Component: transcribe.Component, Transcribe: &integration.RuntimeTranscribeInput{AssetID: a.ID, Stream: streaming, VAD: streaming, Options: nil, EpisodeNumber: 1}}
		runtimeHTTPRun(t, ctx, client, runtimeClient, s, transcribe, input, streaming)
		if streaming {
			job, err := runtimeClient.Submit(ctx, s, opaqueID(), transcribe.Ref, input)
			if err != nil {
				t.Fatal(err)
			}
			var cancelSent, terminalCancelled bool
			err = runtimeClient.Events(ctx, s, job, 0, func(event integration.RuntimeEvent) error {
				if !cancelSent && (event.State == "loading" || event.State == "running") {
					cancelSent = true
					snapshot, err := runtimeClient.Cancel(ctx, s, job)
					if err != nil {
						return err
					}
					if snapshot.State != "cancelled" {
						return fmt.Errorf("active stream did not cancel")
					}
				}
				if event.State.Terminal() {
					terminalCancelled = event.State == "cancelled"
				}
				return nil
			})
			if err != nil || !cancelSent || !terminalCancelled {
				t.Fatal("active real HTTP stream cancellation failed", err)
			}
			current, err := runtimeClient.Query(ctx, s, job)
			if err != nil || current.State != "cancelled" || current.Acknowledged {
				t.Fatal("cancelled active stream query mismatch", err)
			}
			if _, err := runtimeClient.Result(ctx, s, job); err == nil {
				t.Fatal("cancelled active stream exposed successful result")
			}
			t.Logf("real HTTP active stream cancellation job=%s state=%s cursor=%d; no result", current.ID, current.State, current.Cursor)
		}
	}
	if batch {
		transcribe := put("highlight.transcribe", "whisper-batch", "whisper-asr")
		runWhisper(transcribe, false)
		t.Log("Whisper batch real HTTP inference passed: nonempty speech, no SSE stream commits, Query=succeeded, typed Result identity matched; stream/VAD disabled")
	} else {
		t.Log("Whisper batch NOT RUN: optional explicit BATCH=1 and real ASR/speech inputs required")
	}
	if stream {
		transcribe := put("highlight.transcribe", "whisper-stream", "whisper-asr", "whisper-vad")
		runWhisper(transcribe, true)
		t.Log("Whisper stream real HTTP inference passed: nonempty speech, SSE commits monotonic/final, Query=succeeded, typed Result identity matched; VAD explicitly enabled")
	} else {
		t.Log("Whisper stream NOT RUN: optional explicit STREAM=1 and real ASR/VAD/speech inputs required")
	}
}

const runtimeHTTPDiagnosticLineLimit = 160
const runtimeHTTPDiagnosticRecordLimit = 32

var runtimeHTTPDiagnosticPattern = regexp.MustCompile(`^(?:[0-9]{4}/[0-9]{2}/[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} )?center_descriptor phase=(write|sync|close|publish) event=(start|done) elapsed_ms=(0|[1-9][0-9]{0,12})$`)

type runtimeHTTPDiagnostics struct {
	mu      sync.Mutex
	records []string
}

func (d *runtimeHTTPDiagnostics) snapshot() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.records, "\n")
}

// Each stream has independent framing; the shared capture retains safe records only.
type runtimeHTTPDiagnosticWriter struct {
	mu      sync.Mutex
	capture *runtimeHTTPDiagnostics
	line    [runtimeHTTPDiagnosticLineLimit]byte
	used    int
	drop    bool
}

func (w *runtimeHTTPDiagnosticWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			if !w.drop {
				line := w.line[:w.used]
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				m := runtimeHTTPDiagnosticPattern.FindSubmatch(line)
				if m != nil && (string(m[2]) != "start" || string(m[3]) == "0") {
					record := "center_descriptor phase=" + string(m[1]) + " event=" + string(m[2]) + " elapsed_ms=" + string(m[3])
					w.capture.mu.Lock()
					if len(w.capture.records) == runtimeHTTPDiagnosticRecordLimit {
						copy(w.capture.records, w.capture.records[1:])
						w.capture.records = w.capture.records[:runtimeHTTPDiagnosticRecordLimit-1]
					}
					w.capture.records = append(w.capture.records, record)
					w.capture.mu.Unlock()
				}
			}
			clear(w.line[:])
			w.used, w.drop = 0, false
		} else if !w.drop {
			if w.used == len(w.line) {
				clear(w.line[:])
				w.used, w.drop = 0, true
			} else {
				w.line[w.used] = b
				w.used++
			}
		}
	}
	return len(p), nil
}

// Provenance and a visual profile review remain the caller's responsibility.
func runtimeHTTPProfileImages(t *testing.T, path string) ([]byte, []byte) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (8<<20)+1))
	if err != nil || len(data) == 0 || len(data) > 8<<20 {
		t.Fatal("profile JPEG must be nonempty and at most 8 MiB")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4096 || cfg.Height > 4096 {
		t.Fatal("profile must be a valid JPEG with bounded dimensions")
	}
	source, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b := source.Bounds()
	flipped := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			flipped.Set(x, y, source.At(b.Max.X-1-x, b.Min.Y+y))
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, flipped); err != nil {
		t.Fatal(err)
	}
	if encoded.Len() > 8<<20 {
		t.Fatal("reflected profile exceeds the image byte limit")
	}
	return data, encoded.Bytes()
}

func runtimeHTTPPath(t *testing.T, name string, directory bool) string {
	t.Helper()
	path := os.Getenv(name)
	if path == "" || !filepath.IsAbs(path) {
		t.Fatalf("%s must explicitly name an existing absolute path; no fallback/download", name)
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() != directory {
		t.Fatalf("%s input unavailable", name)
	}
	if !directory {
		runtimeHTTPRegular(t, path)
	}
	return filepath.Clean(path)
}
func runtimeHTTPRegular(t *testing.T, path string) {
	t.Helper()
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		t.Fatalf("required nonempty real file unavailable: %s", path)
	}
}
func runtimeHTTPFileSHA(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}
func runtimeHTTPWait(t *testing.T, ctx context.Context, ready func() bool) {
	t.Helper()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if ready() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for owned center/operation")
		case <-tick.C:
		}
	}
}

type runtimeHTTPBrowser struct {
	t           *testing.T
	ctx         context.Context
	base, token string
	http        *http.Client
}

func (b *runtimeHTTPBrowser) loadToken() {
	b.t.Helper()
	r, err := http.NewRequestWithContext(b.ctx, "GET", b.base+"/", nil)
	if err != nil {
		b.t.Fatal(err)
	}
	response, err := b.http.Do(r)
	if err != nil {
		b.t.Fatal("homepage transport failed")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode != 200 {
		b.t.Fatal("real homepage unavailable")
	}
	matches := regexp.MustCompile(`<meta\s+name="model-center-token"\s+content="([^"]+)"`).FindAllSubmatch(raw, -1)
	if len(matches) != 1 {
		b.t.Fatal("real homepage token meta missing/ambiguous")
	}
	b.token = html.UnescapeString(string(matches[0][1]))
	if len(b.token) != 64 {
		b.t.Fatal("invalid real homepage credential")
	}
}
func (b *runtimeHTTPBrowser) json(method, path string, value any, status int, out any) {
	b.t.Helper()
	var raw []byte
	var err error
	if value != nil {
		raw, err = json.Marshal(value)
		if err != nil {
			b.t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(b.ctx, method, b.base+path, bytes.NewReader(raw))
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("X-Model-Center-Token", b.token)
	if value != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.GetBody = nil
	response, err := b.http.Do(req)
	if err != nil {
		b.t.Fatalf("browser HTTP %s %s transport failed", method, path)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil || response.StatusCode != status {
		b.t.Fatalf("browser HTTP %s %s status=%d want=%d", method, path, response.StatusCode, status)
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		b.t.Fatalf("invalid JSON from %s", path)
	}
}
func (b *runtimeHTTPBrowser) operation(path string, body any) Operation {
	b.t.Helper()
	var op Operation
	b.json("POST", path, body, 202, &op)
	ctx, cancel := context.WithTimeout(b.ctx, 10*time.Minute)
	defer cancel()
	runtimeHTTPWait(b.t, ctx, func() bool {
		b.json("GET", "/api/operations/"+op.ID, nil, 200, &op)
		return op.Status != "running" && op.Status != "cancelling"
	})
	if op.Status != "completed" {
		b.t.Fatalf("real operation %s status=%s", op.Kind, op.Status)
	}
	return op
}
func (b *runtimeHTTPBrowser) reject(method, path string, raw []byte, headers map[string]string) {
	b.t.Helper()
	req, err := http.NewRequestWithContext(b.ctx, method, b.base+path, bytes.NewReader(raw))
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if headers == nil {
		req.Header.Set("X-Model-Center-Token", b.token)
	} else {
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}
	response, err := b.http.Do(req)
	if err != nil {
		b.t.Fatal("negative HTTP transport failed")
	}
	response.Body.Close()
	if response.StatusCode != 401 && response.StatusCode != 403 {
		b.t.Fatalf("wrong credential not rejected: status=%d", response.StatusCode)
	}
}
func runtimeHTTPRejected(t *testing.T, err error) {
	t.Helper()
	e, ok := err.(*integration.RuntimeHTTPError)
	if !ok || e.Status != 403 && e.Status != 404 {
		t.Fatalf("cross-task request must be rejected by actual HTTP server, got %T", err)
	}
}
func runtimeHTTPOpen(t *testing.T, ctx context.Context, c *integration.Client, ref integration.ExecutionBindingRef) *integration.SessionCredentials {
	t.Helper()
	s, err := c.OpenRuntimeSession(ctx, opaqueID(), []string{ref.Scene}, []integration.ExecutionBindingRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func runtimeHTTPClose(t *testing.T, c *integration.Client, s *integration.SessionCredentials) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.CloseRuntimeSession(ctx, s); err != nil {
		t.Error("runtime/management session close failed")
	}
}
func runtimeHTTPRun(t *testing.T, ctx context.Context, c *integration.Client, r *integration.RuntimeClient, s *integration.SessionCredentials, b integration.BindingView, body integration.RuntimeBody, stream bool) integration.RuntimeResult {
	t.Helper()
	job, err := r.Submit(ctx, s, opaqueID(), b.Ref, body)
	if err != nil {
		t.Fatal(err)
	}
	if stream {
		queued, err := r.Submit(ctx, s, opaqueID(), b.Ref, body)
		if err != nil {
			t.Fatal(err)
		}
		if queued.Snapshot().State != "queued" {
			t.Fatal("real stream must still own the heavy slot for queued cancellation verification")
		}
		cancelled, err := r.Cancel(ctx, s, queued)
		if err != nil || cancelled.State != "cancelled" || cancelled.ID != queued.Snapshot().ID || cancelled.Acknowledged {
			t.Fatal("explicit queued HTTP cancellation failed", err)
		}
		repeated, err := r.Cancel(ctx, s, queued)
		if err != nil || repeated != cancelled {
			t.Fatal("explicit queued HTTP cancellation is not idempotent", err)
		}
		current, err := r.Query(ctx, s, queued)
		if err != nil || current != cancelled {
			t.Fatal("cancelled HTTP job changed terminal identity", err)
		}
		if _, err := r.Result(ctx, s, queued); err == nil {
			t.Fatal("cancelled HTTP job exposed a successful result")
		}
		var cancelledSequence uint64
		err = r.Events(ctx, s, queued, 0, func(event integration.RuntimeEvent) error {
			if event.Sequence <= cancelledSequence || event.State != "queued" && event.State != "cancelled" || event.Progress != nil || event.Commit != nil {
				return fmt.Errorf("cancelled queued job executed or changed event order")
			}
			cancelledSequence = event.Sequence
			return nil
		})
		if err != nil || cancelledSequence != cancelled.Cursor {
			t.Fatal("cancelled queued job event history mismatch", err)
		}
		t.Logf("real HTTP queued cancellation job=%s state=%s cursor=%d; repeated cancellation stable; no result", cancelled.ID, cancelled.State, cancelled.Cursor)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeat := make(chan error, 1)
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-runCtx.Done():
				heartbeat <- nil
				return
			case <-tick.C:
				if err := c.HeartbeatRuntimeSession(runCtx, s); err != nil {
					heartbeat <- err
					cancel()
					return
				}
			}
		}
	}()
	var commits int
	var receiptCommits []integration.RuntimeStreamCommit
	var last *integration.RuntimeStreamCommit
	var terminal integration.RuntimeState
	var sequence uint64
	err = r.Events(runCtx, s, job, 0, func(event integration.RuntimeEvent) error {
		if event.Sequence <= sequence {
			return fmt.Errorf("nonmonotonic SSE sequence")
		}
		sequence = event.Sequence
		if event.State.Terminal() {
			terminal = event.State
		}
		if v := event.Commit; v != nil {
			if last != nil && (last.Final || v.Revision <= last.Revision || v.CommittedThroughSec < last.CommittedThroughSec) {
				return fmt.Errorf("nonmonotonic stream commit")
			}
			copy := runtimeHTTPCloneCommit(*v)
			last = &copy
			receiptCommits = append(receiptCommits, copy)
			commits++
		}
		return nil
	})
	cancel()
	heartbeatErr := <-heartbeat
	if err != nil {
		t.Fatal(err)
	}
	if heartbeatErr != nil {
		t.Fatal("runtime session heartbeat failed")
	}
	// SSE EOF is not success: independently require terminal event, query and typed result.
	snap, err := r.Query(ctx, s, job)
	if err != nil {
		t.Fatal(err)
	}
	if terminal != "succeeded" || snap.State != "succeeded" || snap.ID != job.Snapshot().ID || snap.Binding != b.Ref || snap.TaskID != s.Grant().TaskID || snap.SessionID != s.Grant().SessionID || snap.Cursor != sequence {
		t.Fatalf("job did not succeed with consistent HTTP identity: state=%s code=%s", snap.State, snap.Code)
	}
	result, err := r.Result(ctx, s, job)
	if err != nil {
		t.Fatal(err)
	}
	if result.Component != b.Component {
		t.Fatal("typed result component identity mismatch")
	}
	if stream {
		if commits == 0 || last == nil || !last.Final || result.Stream == nil || !result.Stream.Final || result.Stream.Revision != last.Revision || result.Stream.CommittedThroughSec != last.CommittedThroughSec || result.Stream.EpisodeNumber != body.Transcribe.EpisodeNumber {
			t.Fatal("missing final/consistent real stream commit")
		}
		nonempty := false
		for _, item := range result.Stream.Items {
			nonempty = nonempty || strings.TrimSpace(item.Text) != ""
		}
		if !nonempty {
			t.Fatal("real speech must produce nonempty transcription")
		}
	} else if body.Transcribe != nil {
		if commits != 0 || last != nil || result.Stream != nil || len(result.Items) == 0 {
			t.Fatal("batch must return transcript items without stream commits")
		}
		nonempty := false
		for _, item := range result.Items {
			nonempty = nonempty || strings.TrimSpace(item.Text) != ""
		}
		if !nonempty {
			t.Fatal("real speech must produce nonempty batch transcription")
		}
	}
	afterRead, err := r.Query(ctx, s, job)
	if err != nil || afterRead != snap || afterRead.Acknowledged {
		t.Fatal("reading the complete result must not acknowledge it", err)
	}
	receipt := integration.RuntimeCompletion{Snapshot: snap, Body: body, Result: result, Commits: receiptCommits}
	if err := receipt.Validate(); err != nil {
		t.Fatal("invalid complete durable receipt", err)
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(os.Getenv("FENGSHEN_CENTER_RUNTIME_HTTP_ROOT"), "runtime-http-receipt-"+snap.ID+".json")
	file, err := os.OpenFile(receiptPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	n, writeErr := file.Write(raw)
	if writeErr == nil && n != len(raw) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal("receipt persistence failed", writeErr, closeErr)
	}
	if err := safefs.PersistParent(receiptPath); err != nil {
		t.Fatal("receipt directory persistence failed", err)
	}
	persisted, err := os.ReadFile(receiptPath)
	if err != nil || !bytes.Equal(persisted, raw) {
		t.Fatal("durable receipt readback failed", err)
	}
	acked, err := r.Acknowledge(ctx, s, job)
	expectedAck := snap
	expectedAck.Acknowledged = true
	if err != nil || acked != expectedAck {
		t.Fatal("explicit acknowledgement identity mismatch", err)
	}
	repeated, err := r.Acknowledge(ctx, s, job)
	if err != nil || repeated != acked {
		t.Fatal("repeated explicit acknowledgement is not idempotent", err)
	}
	confirmed, err := r.Query(ctx, s, job)
	if err != nil || confirmed != acked {
		t.Fatal("acknowledgement not reflected in job query", err)
	}
	t.Logf("real HTTP job=%s task=%s scene=%s state=%s events=%d commits=%d component=%s/%s binding_revision=%d acknowledged=%t receipt_sha256=%x", snap.ID, snap.TaskID, b.Ref.Scene, snap.State, sequence, commits, b.Component.ID, b.Component.Version, b.Ref.Revision, confirmed.Acknowledged, sha256.Sum256(raw))
	return result
}

func runtimeHTTPCloneCommit(v integration.RuntimeStreamCommit) integration.RuntimeStreamCommit {
	v.Items = append(v.Items[:0:0], v.Items...)
	return v
}

func TestRuntimeHTTPCloneCommit(t *testing.T) {
	for _, items := range [][]integration.RuntimeTranscriptItem{nil, {}, {{Text: "speech"}}} {
		original := integration.RuntimeStreamCommit{Items: items}
		copy := runtimeHTTPCloneCommit(original)
		before, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		after, err := json.Marshal(copy)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("commit copy changed durable JSON identity", err)
		}
		if len(copy.Items) > 0 {
			copy.Items[0].Text = "changed"
			if original.Items[0].Text != "speech" {
				t.Fatal("commit copy aliases source items")
			}
		}
	}
}

func runtimeHTTPEnabled(value string) bool { return value == "1" }

func runtimeHTTPModes(batchValue, streamValue string) (batch, stream bool, err error) {
	for _, mode := range []struct{ name, value string }{{"BATCH", batchValue}, {"STREAM", streamValue}} {
		if mode.value != "" && mode.value != "0" && mode.value != "1" {
			return false, false, fmt.Errorf("%s must be empty, 0 or 1", mode.name)
		}
	}
	return batchValue == "1", streamValue == "1", nil
}

func TestRuntimeHTTPDiagnosticWriter(t *testing.T) {
	capture := &runtimeHTTPDiagnostics{}
	w := &runtimeHTTPDiagnosticWriter{capture: capture}
	valid := "center_descriptor phase=sync event=done elapsed_ms=12"
	secret := "SECRET_TOKEN_W_private_http_private"
	for _, line := range []string{
		secret, "2026/11/09 12:00:00 本地模型管理中心 http://private · 根目录 W:\\private",
		secret + valid, valid + " token=" + secret, valid + "\x00", valid + "\x1b[0m",
		"center_descriptor phase=" + secret + " event=done elapsed_ms=1",
		"center_descriptor phase=write event=error elapsed_ms=1",
		"center_descriptor phase=write event=start elapsed_ms=1",
		"center_descriptor phase=write event=done elapsed_ms=-1",
		"center_descriptor phase=write event=done elapsed_ms=" + strings.Repeat("9", 1000),
		strings.Repeat(secret, 10000) + valid,
	} {
		if n, err := w.Write([]byte(line + "\n")); err != nil || n != len(line)+1 {
			t.Fatal("writer must consume discarded output")
		}
	}
	if capture.snapshot() != "" || w.used != 0 || w.drop || w.line != [runtimeHTTPDiagnosticLineLimit]byte{} {
		t.Fatal("unapproved output retained")
	}
	// Fragments and CRLF must work, without accepting an unterminated line.
	for _, fragment := range []string{"2026/11/09 12:00:00 center_descriptor phase=", "sync event=done elapsed_ms=12\r", "\n"} {
		w.Write([]byte(fragment))
	}
	if capture.snapshot() != valid {
		t.Fatal("valid fragmented diagnostic lost")
	}
	w.Write([]byte(secret))
	if capture.snapshot() != valid {
		t.Fatal("partial raw output exposed")
	}
	w.Write([]byte("\n"))
	other := &runtimeHTTPDiagnosticWriter{capture: capture}
	w.Write([]byte(secret))
	other.Write([]byte("center_descriptor phase=write event=start elapsed_ms=0\n"))
	w.Write([]byte(valid + "\n"))
	if !strings.HasSuffix(capture.snapshot(), "center_descriptor phase=write event=start elapsed_ms=0") {
		t.Fatal("stream framing mixed or valid phase lost")
	}
	for _, phase := range []string{"write", "sync", "close", "publish"} {
		for _, event := range []string{"start", "done"} {
			record := "center_descriptor phase=" + phase + " event=" + event + " elapsed_ms=0"
			other.Write([]byte("2026/11/09 12:00:00 " + record + "\n"))
			if !strings.HasSuffix(capture.snapshot(), record) {
				t.Fatal("valid timestamped phase lost")
			}
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				w.Write([]byte(valid + "\n"))
				other.Write([]byte("center_descriptor phase=publish event=start elapsed_ms=0\n"))
				_ = capture.snapshot()
			}
		}()
	}
	wg.Wait()
	lines := strings.Split(capture.snapshot(), "\n")
	if len(lines) != runtimeHTTPDiagnosticRecordLimit {
		t.Fatal("record retention not bounded")
	}
	for _, line := range lines {
		if line != valid && line != "center_descriptor phase=publish event=start elapsed_ms=0" {
			t.Fatal("concurrent capture corrupted or leaked output")
		}
	}
	w.Write([]byte("center_descriptor phase=close event=done elapsed_ms=2\n"))
	if !strings.HasSuffix(capture.snapshot(), "center_descriptor phase=close event=done elapsed_ms=2") {
		t.Fatal("latest phase lost at capacity")
	}
}

func TestRuntimeHTTPModes(t *testing.T) {
	for _, batch := range []string{"", "0", "1"} {
		for _, stream := range []string{"", "0", "1"} {
			gotBatch, gotStream, err := runtimeHTTPModes(batch, stream)
			if err != nil || gotBatch != (batch == "1") || gotStream != (stream == "1") {
				t.Fatalf("mode parse mismatch: batch=%q stream=%q", batch, stream)
			}
		}
	}
	for _, value := range []string{"true", "yes", " 1", "1 ", "01", "2", "-1"} {
		if _, _, err := runtimeHTTPModes(value, "1"); err == nil {
			t.Fatalf("invalid batch mode accepted: %q", value)
		}
		if _, _, err := runtimeHTTPModes("1", value); err == nil {
			t.Fatalf("invalid stream mode accepted: %q", value)
		}
	}
}

// The default path must not resolve inputs, build or launch a center.
func TestRuntimeHTTPHarnessContracts(t *testing.T) {
	for _, value := range []string{"", "0", "true", "yes", " 1", "1 ", "01"} {
		if runtimeHTTPEnabled(value) {
			t.Fatalf("implicit opt-in accepted: %q", value)
		}
	}
	if !runtimeHTTPEnabled("1") {
		t.Fatal("explicit opt-in rejected")
	}
}

// This validates encoded reflection only, not HTTP inference or genuine imagery.
func TestRuntimeHTTPProfileImagesReflection(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 5, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 5; x++ {
			src.Set(x, y, color.NRGBA{R: uint8(40 * x), G: uint8(70 * y), B: 30, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, src, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "helper-only.jpg")
	if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	data, mirror := runtimeHTTPProfileImages(t, path)
	original, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	flipped, err := png.Decode(bytes.NewReader(mirror))
	if err != nil {
		t.Fatal(err)
	}
	if flipped.Bounds() != original.Bounds() {
		t.Fatal("reflection dimensions changed")
	}
	for y := 0; y < 3; y++ {
		for x := 0; x < 5; x++ {
			if color.NRGBAModel.Convert(flipped.At(x, y)) != color.NRGBAModel.Convert(original.At(4-x, y)) {
				t.Fatal("reflection changed decoded pixels")
			}
		}
	}
}

func TestRuntimeHTTPDefaultDoesNotResolveInputs(t *testing.T) {
	t.Setenv(runtimeHTTPOptIn, "0")
	t.Setenv("FENGSHEN_CENTER_RUNTIME_HTTP_BATCH", "1")
	t.Setenv("FENGSHEN_CENTER_RUNTIME_HTTP_STREAM", "1")
	for _, name := range []string{"ROOT", "REPO", "FACE_JPEG", "PROFILE_JPEG", "ASR", "VAD", "SPEECH_WAV"} {
		t.Setenv("FENGSHEN_CENTER_RUNTIME_HTTP_"+name, "not-an-absolute-existing-input")
	}
	if !t.Run("disabled_before_any_input_or_process_access", TestRuntimeHTTPE2E) {
		t.Fatal("disabled real inference suite touched unavailable inputs")
	}
}
