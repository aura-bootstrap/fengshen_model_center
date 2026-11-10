//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/bindings"
	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimehttp"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeprocess"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
)

// Test fixtures record admission claims only; they are not delivery or inference evidence.
func TestRuntimeHaarRequiresCurrentExecutableBytes(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	current := runtimeFileContents(t, executable)
	for _, same := range []bool{false, true} {
		t.Run(strconv.FormatBool(same), func(t *testing.T) {
			m := fixtureManager(t)
			for _, name := range []string{"haarcascade_frontalface_default.xml", "haarcascade_profileface.xml"} {
				put(t, filepath.Join(m.root, "external", "haar", name), "XML fixture; never loaded")
			}
			dir := components.ComponentDirectory(m.componentRoot, "haar-consumer", "1")
			body := append([]byte(nil), current...)
			if !same {
				body[len(body)-1] ^= 1
			}
			entry := filepath.Join(dir, "center.exe")
			put(t, entry, string(body))
			sum := sha256.Sum256(body)
			manifest := components.Manifest{Schema: 1, ComponentID: "haar-consumer", Version: "1", OS: runtime.GOOS, Arch: runtime.GOARCH, EntryType: "executable", Entry: "center.exe", Packaging: "go", Protocols: []string{"haar/v1"}, ModelRefs: []string{"haar-frontal", "haar-profile"}, StartupReportRef: "startup.json", Files: []components.File{{Path: "center.exe", Bytes: strconv.Itoa(len(body)), SHA256: hex.EncodeToString(sum[:])}}}
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			put(t, filepath.Join(dir, "manifest.json"), string(raw))
			sum = sha256.Sum256(raw)
			zero := 0
			report := components.StartupReport{Schema: 1, ComponentID: manifest.ComponentID, Version: manifest.Version, ManifestSHA256: hex.EncodeToString(sum[:]), StartupTest: "passed", CleanEnvironment: "not_verified", ProtocolEvidence: "declared_not_tested", Inference: "not_tested", MetadataOnly: &components.MetadataOnlyEvidence{Arguments: []string{"--haar-component-info"}, ExitCode: &zero, CheckedAt: time.Now().UTC(), NoModelLoad: true, NoNetwork: true, NoCompilation: true}}
			raw, err = json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			put(t, filepath.Join(dir, "startup.json"), string(raw))
			evidence, err := m.components.Verify(context.Background(), manifest.ComponentID, manifest.Version)
			if err != nil || evidence.StartupTest != "passed" || evidence.Integrity != "matched" {
				t.Fatalf("Haar fixture admission: %+v %v", evidence, err)
			}
			request := bindingSelection(t, m, "image.face", "haar-consumer", "haar-frontal", "haar-profile")
			request.Component = integration.ExecutionComponentRef{ID: manifest.ComponentID, Version: manifest.Version, ManifestSHA256: evidence.ManifestSHA256, Protocol: "haar/v1"}
			if _, err := m.PutBinding(context.Background(), "image.face", request); err != nil {
				t.Fatal(err)
			}
			binding := m.bindingStore.Snapshot().Bindings["image.face"]
			resolved, err := m.resolveRuntimeBinding(context.Background(), binding.Ref, binding.Component)
			if resolved.Release != nil {
				defer resolved.Release()
			}
			if same {
				if err != nil || len(resolved.Models) != 2 || resolved.Manifest.ComponentID != "haar-consumer" {
					t.Fatalf("same-byte embedded implementation rejected: %+v %v", resolved, err)
				}
				runtimeAssertFilePinned(t, entry)
				resolved.Release()
			} else if err == nil || err.Error() != "center_component_unverified" {
				t.Fatalf("different center implementation admitted: %+v %v", resolved, err)
			}
			runtimeAssertFileReleased(t, entry)
		})
	}
}

func TestRuntimePinsProtectAncestorDirectories(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "models")
	leaf := filepath.Join(parent, "group")
	if err := os.MkdirAll(leaf, 0700); err != nil {
		t.Fatal(err)
	}
	pins := &runtimePins{}
	defer pins.release()
	if err := pins.directory(leaf); err != nil {
		t.Fatal(err)
	}
	var ancestors []string
	for path := leaf; ; path = filepath.Dir(path) {
		ancestors = append(ancestors, path)
		if filepath.Dir(path) == path {
			break
		}
	}
	if len(pins.files) != len(ancestors) {
		t.Fatalf("directory pins do not cover volume root through leaf: %d, want %d", len(pins.files), len(ancestors))
	}
	for i, path := range ancestors {
		file := pins.files[len(ancestors)-1-i]
		if !sameScanPath(file.Name(), path) {
			t.Fatalf("wrong pinned ancestor: %q, want %q", file.Name(), path)
		}
		if _, err := file.Stat(); err != nil {
			t.Fatalf("ancestor handle is not live: %s: %v", path, err)
		}
	}
	if err := pins.directory(leaf); err != nil || len(pins.files) != len(ancestors) {
		t.Fatalf("duplicate directory pin: count %d, %v", len(pins.files), err)
	}
	runtimeExpectSharingDenied(t, "model ancestor rename", os.Rename(parent, parent+".moved"))
	pins.release()
	for _, file := range pins.files {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) && !errors.Is(err, syscall.Errno(6)) {
			t.Fatalf("released ancestor handle still live: %s: %v", file.Name(), err)
		}
	}
	runtimeAssertDirectoryReleased(t, parent)
}

type runtimeBindingFixture struct {
	manager             *Manager
	binding             bindings.Binding
	install, dir, model string
	files               []string
}

func newRuntimeBindingFixture(t *testing.T) runtimeBindingFixture {
	t.Helper()
	project, install := t.TempDir(), t.TempDir()
	for _, rel := range []string{"sidecar", "models/group"} {
		if err := os.MkdirAll(filepath.Join(project, rel), 0700); err != nil {
			t.Fatal(err)
		}
	}
	m, err := newManagerWithInstallRoot(project, t.TempDir(), testManifest(t), 0, "", "", "development", install)
	if err != nil {
		t.Fatal(err)
	}
	allowFixtureRoots(m, filepath.Join(project, "models"), filepath.Join(project, "external", "haar"))
	t.Cleanup(func() { m.Close() })
	fixture := runtimeBindingFixture{manager: m, install: install, dir: components.ComponentDirectory(m.componentRoot, "whisper-batch", "1"), model: filepath.Join(project, "models", "group", "ggml-small.bin")}
	put(t, fixture.model, "model fixture; never loaded")
	manifest := components.Manifest{
		Schema: 1, ComponentID: "whisper-batch", Version: "1", OS: runtime.GOOS, Arch: runtime.GOARCH,
		EntryType: "executable", Entry: "bin/runner.exe", Packaging: "native",
		Protocols: []string{domain.CenterWhisperBatchProtocol}, ModelRefs: []string{"whisper-asr"}, StartupReportRef: "startup.json",
	}
	for _, rel := range []string{manifest.Entry, "lib/support.dll"} {
		body := []byte("component fixture; never executed: " + rel)
		path := filepath.Join(fixture.dir, filepath.FromSlash(rel))
		put(t, path, string(body))
		fixture.files = append(fixture.files, path)
		sum := sha256.Sum256(body)
		manifest.Files = append(manifest.Files, components.File{Path: rel, Bytes: strconv.Itoa(len(body)), SHA256: hex.EncodeToString(sum[:])})
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(fixture.dir, "manifest.json"), string(body))
	sum := sha256.Sum256(body)
	zero := 0
	// Synthetic report exercises metadata admission only; no startup or inference is run.
	report := components.StartupReport{
		Schema: 1, ComponentID: manifest.ComponentID, Version: manifest.Version, ManifestSHA256: hex.EncodeToString(sum[:]),
		StartupTest: "passed", CleanEnvironment: "not_verified", ProtocolEvidence: "declared_not_tested", Inference: "not_tested",
		MetadataOnly: &components.MetadataOnlyEvidence{Arguments: []string{"--component-info"}, ExitCode: &zero, CheckedAt: time.Now().UTC(), NoModelLoad: true, NoNetwork: true, NoCompilation: true},
	}
	body, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(fixture.dir, "startup.json"), string(body))
	evidence, err := m.components.Verify(context.Background(), manifest.ComponentID, manifest.Version)
	if err != nil || evidence.Manifest == nil || evidence.Integrity != "matched" || evidence.Platform != "matched" || evidence.FilesState != "complete" || evidence.StartupTest != "passed" || evidence.ManifestSHA256 != report.ManifestSHA256 {
		t.Fatalf("invalid runtime fixture: %+v, %v", evidence, err)
	}
	if evidence.Inference != "not_tested" || evidence.ProtocolEvidence != "declared_not_tested" || evidence.CleanEnvironment != "not_verified" {
		t.Fatalf("fixture invented runtime evidence: %+v", evidence)
	}
	request := bindingSelection(t, m, "highlight.transcribe", manifest.ComponentID, "whisper-asr")
	request.Component = integration.ExecutionComponentRef{ID: manifest.ComponentID, Version: manifest.Version, ManifestSHA256: evidence.ManifestSHA256, Protocol: manifest.Protocols[0]}
	snapshot, err := m.PutBinding(context.Background(), "highlight.transcribe", request)
	if err != nil || snapshot.Revision != 1 {
		t.Fatalf("PutBinding: %+v, %v", snapshot, err)
	}
	fixture.binding = m.bindingStore.Snapshot().Bindings["highlight.transcribe"]
	if fixture.binding.Ref != snapshot.Bindings["highlight.transcribe"].Ref || len(fixture.binding.Files) != 1 || fixture.binding.Files[0].ModelID != "whisper-asr" {
		t.Fatalf("binding not persisted: %+v", fixture.binding)
	}
	return fixture
}

type capabilityTestExecution struct {
	started chan struct{}
	reply   chan runtimejobs.Result
}

func (e *capabilityTestExecution) Execute(_ context.Context, _ runtimejobs.Request, _ runtimejobs.Emit) (runtimejobs.Result, error) {
	e.started <- struct{}{}
	return <-e.reply, nil
}
func (e *capabilityTestExecution) Close() error { return nil }

func TestRuntimeCapabilitiesProjectJobEvidence(t *testing.T) {
	fixture := newRuntimeBindingFixture(t)
	m := fixture.manager
	e := &capabilityTestExecution{started: make(chan struct{}, 1), reply: make(chan runtimejobs.Result)}
	jobs, err := runtimejobs.New(e, runtimejobs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer jobs.Close()
	m.runtimeHandler = runtimehttp.New(nil, nil, jobs, nil)
	defer func() { m.runtimeHandler = nil }()
	m.beforeScan = func(context.Context) { t.Fatal("passive capability query scanned") }
	get := func() integration.RuntimeCapability {
		t.Helper()
		caps, err := m.runtimeCapabilities()
		if err != nil {
			t.Fatal(err)
		}
		for _, cap := range caps.Capabilities {
			if cap.Scene == fixture.binding.Scene {
				if err := cap.Validate(); err != nil {
					t.Fatal(err)
				}
				return cap
			}
		}
		t.Fatal("missing capability")
		return integration.RuntimeCapability{}
	}
	if c := get(); c.State != integration.CapabilityAvailable || c.Inference != nil {
		t.Fatal(c)
	}
	body := runtimejobs.Body{Component: fixture.binding.Component, Transcribe: &runtimejobs.TranscribeInput{AssetID: "audio", EpisodeNumber: 1}}
	run := func(id string, result runtimejobs.Result) runtimejobs.Snapshot {
		t.Helper()
		s, err := jobs.Submit(context.Background(), "session", "task", id, fixture.binding.Ref, body)
		if err != nil {
			t.Fatal(err)
		}
		<-e.started
		if c := get(); c.State != integration.CapabilityBusy {
			t.Fatal(c)
		}
		e.reply <- result
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for {
			s, err = jobs.Snapshot("session", "task", s.ID)
			if err != nil {
				t.Fatal(err)
			}
			if s.State.Terminal() {
				return s
			}
			if _, _, err := jobs.WaitEvents(ctx, "session", "task", s.ID, s.Cursor); err != nil {
				t.Fatal(err)
			}
		}
	}
	result := runtimejobs.Result{Component: fixture.binding.Component, Items: []runtimejobs.TranscriptItem{{StartSec: 0, EndSec: 1, Text: "fixture"}}}
	s := run("success", result)
	raw, _ := json.Marshal(result)
	sum := sha256.Sum256(raw)
	if c := get(); c.State != integration.CapabilityAvailable || c.Inference == nil || c.Inference.State != "passed" || c.Inference.InputSHA256 != s.BodySHA256 || c.Inference.ResultSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("success: %+v", c)
	}
	failed := run("failure", runtimejobs.Result{})
	if c := get(); c.State != integration.CapabilityFailed || c.Inference == nil || c.Inference.JobID != failed.ID {
		t.Fatalf("failure: %+v", c)
	}
	verified := m.components
	m.components = components.New(m.componentRoot)
	if c := get(); c.State == integration.CapabilityAvailable || c.Inference != nil {
		t.Fatalf("unverified component retained inference: %+v", c)
	}
	m.components = verified
	jobs.ExpireSession("session")
	if c := get(); c.State != integration.CapabilityAvailable || c.Inference != nil {
		t.Fatalf("expiry: %+v", c)
	}
	put(t, fixture.model, "changed model identity")
	if c := get(); c.Reason != "center_binding_stale" || c.Inference != nil {
		t.Fatal(c)
	}
}

func TestRuntimeCapabilitiesUseVerifiedMetadataOnly(t *testing.T) {
	fixture := newRuntimeBindingFixture(t)
	m := fixture.manager
	assert := func(want integration.CapabilityState, reason string) {
		t.Helper()
		caps, err := m.runtimeCapabilities()
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range caps.Capabilities {
			if c.Scene == fixture.binding.Scene {
				if c.State != want || c.Reason != reason || c.Inference != nil {
					t.Fatalf("capability: %+v, want %s %s without inference", c, want, reason)
				}
				return
			}
		}
		t.Fatal("missing scene")
	}
	// A handler marker is enough for metadata projection; no requests are sent.
	m.runtimeHandler = &runtimehttp.Handler{}
	defer func() { m.runtimeHandler = nil }()
	m.beforeScan = func(context.Context) { t.Fatal("capabilities triggered scan") }
	assert(integration.CapabilityAvailable, "")
	m.runtimeHandler = nil
	assert(integration.CapabilityIncompatible, "center_runtime_unavailable")
	m.runtimeHandler = &runtimehttp.Handler{}
	put(t, fixture.files[0], "changed component")
	assert(integration.CapabilityMissingComponent, "center_component_missing")
}

func TestRuntimeCapabilitiesNeverPerformFirstVerification(t *testing.T) {
	fixture := newRuntimeBindingFixture(t)
	m := fixture.manager
	// Discard the verification cache while leaving the delivery and startup report.
	m.components = components.New(m.componentRoot)
	m.runtimeHandler = &runtimehttp.Handler{}
	defer func() { m.runtimeHandler = nil }()
	caps, err := m.runtimeCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	if c := caps.Capabilities[0]; c.State == integration.CapabilityAvailable || c.Inference != nil {
		t.Fatalf("query invented verification/inference: %+v", c)
	}
	for _, e := range m.Components() {
		if e.ComponentID == fixture.binding.Component.ID && e.Integrity == "matched" {
			t.Fatal("query performed first full hash")
		}
	}
}

func runtimeFileContents(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func runtimeExpectSharingDenied(t *testing.T, operation string, err error) {
	t.Helper()
	if !errors.Is(err, syscall.Errno(32)) && !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		t.Fatalf("%s: want Windows sharing/access denial, got %v", operation, err)
	}
}

func runtimeAssertFilePinned(t *testing.T, path string) {
	t.Helper()
	body := runtimeFileContents(t, path)
	runtimeExpectSharingDenied(t, "write "+path, os.WriteFile(path, []byte("unexpected write"), 0600))
	runtimeExpectSharingDenied(t, "delete "+path, os.Remove(path))
	runtimeExpectSharingDenied(t, "rename "+path, os.Rename(path, path+".moved"))
	if !bytes.Equal(body, runtimeFileContents(t, path)) {
		t.Fatalf("pinned bytes changed: %s", path)
	}
}

func runtimeAssertFileReleased(t *testing.T, path string) {
	t.Helper()
	body := runtimeFileContents(t, path)
	if err := os.WriteFile(path, append(append([]byte(nil), body...), '!'), 0600); err != nil {
		t.Fatalf("write after release %s: %v", path, err)
	}
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatalf("rename after release %s: %v", path, err)
	}
	if err := os.Rename(path+".moved", path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("delete after release %s: %v", path, err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
}

func runtimeAssertDirectoryReleased(t *testing.T, path string) {
	t.Helper()
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatalf("ancestor rename after release %s: %v", path, err)
	}
	if err := os.Rename(path+".moved", path); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeResolveBindingPinsAndRelease(t *testing.T) {
	fixture := newRuntimeBindingFixture(t)
	m, binding := fixture.manager, fixture.binding
	resolved, err := m.resolveRuntimeBinding(context.Background(), binding.Ref, binding.Component)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Release == nil {
		t.Fatal("successful resolution has no Release")
	}
	defer resolved.Release()
	if resolved.Binding.Ref != binding.Ref || resolved.Binding.Component != binding.Component || len(resolved.Models) != 1 || resolved.Models[0].ModelID != "whisper-asr" || !sameScanPath(resolved.Models[0].Path, fixture.model) {
		t.Fatalf("wrong resolved binding/models: %+v", resolved)
	}
	wantRoot := filepath.Join(fixture.install, "model-center", "components", "whisper-batch", "1")
	if !sameScanPath(resolved.ComponentRoot, wantRoot) || sameScanPath(resolved.ComponentRoot, components.ComponentDirectory(filepath.Join(m.root, "model-center", "components"), "whisper-batch", "1")) {
		t.Fatalf("resolver mixed install/project roots: %q, project %q", resolved.ComponentRoot, m.root)
	}
	if resolved.Manifest.ComponentID != binding.Component.ID || resolved.Manifest.Version != binding.Component.Version || resolved.Manifest.Packaging != "native" || !contains(resolved.Manifest.Protocols, domain.CenterWhisperBatchProtocol) || len(resolved.Manifest.Files) != len(fixture.files) {
		t.Fatalf("wrong resolved manifest: %+v", resolved.Manifest)
	}
	entry := resolved.Manifest.Files[0]
	wantEntry, err := runtimeprocess.VerifyBinary(fixture.files[0], entry.SHA256, binding.Component.ID+"/"+binding.Component.Version, binding.Ref.SHA256)
	if err != nil || resolved.Manifest.Entry != "bin/runner.exe" || resolved.Entry != wantEntry {
		t.Fatalf("wrong verified entry: %+v, %v", resolved.Entry, err)
	}
	files := append([]string{fixture.model}, fixture.files...)
	for _, path := range files {
		runtimeAssertFilePinned(t, path)
	}
	directories := []string{
		filepath.Dir(fixture.model), m.root,
		filepath.Dir(fixture.files[0]), filepath.Dir(fixture.files[1]), fixture.dir,
		filepath.Dir(fixture.dir), m.componentRoot, filepath.Join(fixture.install, "model-center"), fixture.install,
	}
	for _, path := range directories {
		runtimeExpectSharingDenied(t, "ancestor rename "+path, os.Rename(path, path+".moved"))
	}
	resolved.Release()
	resolved.Release()
	for _, path := range files {
		runtimeAssertFileReleased(t, path)
	}
	for _, path := range directories {
		// Manager.projectScope independently holds the project root until Close.
		if !sameScanPath(path, m.root) {
			runtimeAssertDirectoryReleased(t, path)
		}
	}
	m.Close()
	runtimeAssertDirectoryReleased(t, m.root)
}

func TestRuntimeResolveRejectsChangedModelMetadata(t *testing.T) {
	for _, change := range []string{"size", "mtime"} {
		t.Run(change, func(t *testing.T) {
			fixture := newRuntimeBindingFixture(t)
			if change == "size" {
				put(t, fixture.model, "different model size")
			} else {
				modified := fixture.binding.Files[0].Modified.Add(time.Hour)
				if err := os.Chtimes(fixture.model, modified, modified); err != nil {
					t.Fatal(err)
				}
			}
			resolved, err := fixture.manager.resolveRuntimeBinding(context.Background(), fixture.binding.Ref, fixture.binding.Component)
			if resolved.Release != nil {
				defer resolved.Release()
			}
			if err == nil || err.Error() != "center_binding_stale" {
				t.Fatalf("changed %s accepted: %+v, %v", change, resolved, err)
			}
			runtimeAssertFileReleased(t, fixture.model)
		})
	}
}

func TestRuntimeResolveRejectsChangedComponentBytes(t *testing.T) {
	for _, index := range []int{0, 1} {
		t.Run([]string{"entry", "support"}[index], func(t *testing.T) {
			fixture := newRuntimeBindingFixture(t)
			path := fixture.files[index]
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			body := runtimeFileContents(t, path)
			body[0] ^= 1
			put(t, path, string(body))
			// Keep size and mtime unchanged so admission must check the digest.
			if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
				t.Fatal(err)
			}
			evidence, verifyErr := fixture.manager.components.Verify(context.Background(), fixture.binding.Component.ID, fixture.binding.Component.Version)
			if verifyErr == nil || evidence.Integrity != "mismatch" {
				t.Fatalf("same-metadata component corruption did not fail SHA-256: %+v, %v", evidence, verifyErr)
			}
			resolved, err := fixture.manager.resolveRuntimeBinding(context.Background(), fixture.binding.Ref, fixture.binding.Component)
			if resolved.Release != nil {
				defer resolved.Release()
			}
			if err == nil || err.Error() != "center_component_unverified" {
				t.Fatalf("changed component bytes accepted: %+v, %v", resolved, err)
			}
			runtimeAssertFileReleased(t, path)
			runtimeAssertDirectoryReleased(t, fixture.install)
		})
	}
}

func TestRuntimeResolveFailureReleasesAcquiredPins(t *testing.T) {
	fixture := newRuntimeBindingFixture(t)
	// Verification can read this file, but PinFile must refuse a pre-existing writer.
	name, err := syscall.UTF16PtrFromString(fixture.files[1])
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_WRITE, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	writer := os.NewFile(uintptr(handle), fixture.files[1])
	defer writer.Close()
	evidence, err := fixture.manager.components.Verify(context.Background(), fixture.binding.Component.ID, fixture.binding.Component.Version)
	if err != nil || evidence.Integrity != "matched" || evidence.StartupTest != "passed" {
		t.Fatalf("writer must allow initial component verification: %+v, %v", evidence, err)
	}
	resolved, err := fixture.manager.resolveRuntimeBinding(context.Background(), fixture.binding.Ref, fixture.binding.Component)
	if resolved.Release != nil {
		defer resolved.Release()
	}
	if err == nil || err.Error() != "center_component_unverified" || resolved.Release == nil || len(resolved.Models) != 1 || resolved.ComponentRoot != fixture.dir {
		t.Fatalf("did not reach failure after model/component directory pins: %+v, %v", resolved, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	// Do not call Release here: the resolver's error path must have done it already.
	for _, path := range append([]string{fixture.model}, fixture.files...) {
		runtimeAssertFileReleased(t, path)
	}
	for _, path := range []string{filepath.Dir(fixture.model), fixture.dir, fixture.manager.componentRoot, fixture.install} {
		runtimeAssertDirectoryReleased(t, path)
	}
	fixture.manager.Close()
	runtimeAssertDirectoryReleased(t, fixture.manager.root)
}

func runtimeSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if errors.Is(err, syscall.Errno(1314)) || errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			t.Skipf("cannot create intermediate directory reparse point: Windows symlink privilege/access unavailable: %v", err)
		}
		t.Fatalf("create intermediate directory reparse point: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	})
}

func TestRuntimeRejectsIntermediateReparse(t *testing.T) {
	t.Run("directory-pins", func(t *testing.T) {
		root := t.TempDir()
		target, link := filepath.Join(root, "target"), filepath.Join(root, "linked")
		leaf := filepath.Join(target, "leaf")
		if err := os.MkdirAll(leaf, 0700); err != nil {
			t.Fatal(err)
		}
		runtimeSymlinkOrSkip(t, target, link)
		pins := &runtimePins{}
		defer pins.release()
		if err := pins.directory(filepath.Join(link, "leaf")); !errors.Is(err, runtimeprocess.ErrIdentity) {
			t.Fatalf("intermediate reparse not rejected: %v", err)
		}
		if len(pins.files) == 0 {
			t.Fatal("test did not acquire pins before the reparse point")
		}
		pins.release()
		runtimeAssertDirectoryReleased(t, root)
	})
	for _, kind := range []string{"model", "component"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newRuntimeBindingFixture(t)
			parent, want := filepath.Dir(fixture.model), "center_binding_stale"
			if kind == "component" {
				parent, want = filepath.Dir(fixture.files[0]), "center_component_unverified"
			}
			target := parent + ".original"
			if err := os.Rename(parent, target); err != nil {
				t.Fatal(err)
			}
			runtimeSymlinkOrSkip(t, target, parent)
			resolved, err := fixture.manager.resolveRuntimeBinding(context.Background(), fixture.binding.Ref, fixture.binding.Component)
			if resolved.Release != nil {
				defer resolved.Release()
			}
			if err == nil || err.Error() != want {
				t.Fatalf("%s intermediate reparse accepted: %+v, %v", kind, resolved, err)
			}
		})
	}
}

func TestRuntimeComponentsUseInstallRoot(t *testing.T) {
	project := t.TempDir()
	for _, rel := range []string{"sidecar", "models"} {
		if err := os.MkdirAll(filepath.Join(project, rel), 0700); err != nil {
			t.Fatal(err)
		}
	}
	install := t.TempDir()
	m, err := newManagerWithInstallRoot(project, t.TempDir(), testManifest(t), 0, "", "", "development", install)
	if err != nil {
		t.Fatal(err)
	}
	allowFixtureRoots(m, filepath.Join(project, "models"), filepath.Join(project, "external", "haar"))
	defer m.Close()
	want := filepath.Join(install, "model-center", "components")
	if m.componentRoot != want || m.root != project {
		t.Fatalf("component/project roots mixed: %q %q", m.componentRoot, m.root)
	}
}
