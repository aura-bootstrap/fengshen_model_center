package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

func defaultScopeManager(t *testing.T) *Manager {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := t.TempDir()
	for _, rel := range []string{"sidecar", "models"} {
		if err := os.MkdirAll(filepath.Join(root, rel), 0700); err != nil {
			t.Fatal(err)
		}
	}
	m, err := NewManager(root, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func directoryAlias(t *testing.T, kind, target, path string) {
	t.Helper()
	if kind == "junction" {
		if runtime.GOOS != "windows" {
			t.Skip("junction is Windows-only")
		}
		if out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", path, target).CombinedOutput(); err != nil {
			t.Skipf("junction unavailable: %v: %s", err, out)
		}
	} else if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })
}

func partialInventory(t *testing.T, m *Manager) Inventory {
	t.Helper()
	o, err := m.StartScan(context.Background(), m.State().Registry.Revision)
	if err != nil {
		t.Fatal(err)
	}
	done := await(t, m, o.ID)
	if done.Status != "partial" {
		t.Fatalf("expected partial: %+v", done)
	}
	inv, err := m.Inventory(done.ResultID)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestCoreDefaultRootsRejectDirectoryAliases(t *testing.T) {
	for _, kind := range []string{"symlink", "junction"} {
		for _, entry := range []string{"models", "sidecar", "home-cache", "haar-parent"} {
			t.Run(kind+"/"+entry, func(t *testing.T) {
				m := defaultScopeManager(t)
				outside := t.TempDir()
				path := filepath.Join(m.root, "models")
				switch entry {
				case "sidecar":
					path = filepath.Join(m.root, "sidecar")
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					put(t, filepath.Join(outside, "seed-vc", "checkpoints", "955717e8-secret.th"), "secret")
					put(t, filepath.Join(outside, "venv", "Scripts", "python.exe"), "secret")
				case "home-cache":
					path = filepath.Join(m.home, ".cache")
					put(t, filepath.Join(outside, "torch", "hub", "checkpoints", "955717e8-secret.th"), "secret")
				case "haar-parent":
					path = filepath.Join(m.root, "assets", "haar")
					put(t, filepath.Join(outside, "haarcascade_frontalface_default.xml"), "secret")
				default:
					put(t, filepath.Join(outside, "955717e8-secret.th"), "secret")
				}
				directoryAlias(t, kind, outside, path)
				inv := partialInventory(t, m)
				if fs := mustFiles(t, m, inv.ID, "demucs"); len(fs) != 0 {
					t.Fatal("default root escaped", fs)
				}
				if fs := mustFiles(t, m, inv.ID, "haar-frontal"); len(fs) != 0 {
					t.Fatal("Haar parent escaped", fs)
				}
				for _, scope := range inv.Scopes {
					rel, err := filepath.Rel(path, scope.Path)
					if err == nil && filepath.IsLocal(rel) && scope.Status != "unavailable" {
						t.Fatalf("linked entry misclassified: %+v", scope)
					}
				}
			})
		}
	}
}

func TestCoreDefaultHaarLeafScopeAndMissingRoots(t *testing.T) {
	m := defaultScopeManager(t)
	inv := scanFixture(t, m)
	for _, scope := range inv.Scopes {
		if scope.Path == filepath.Join(m.root, "models") {
			if scope.Status != "complete" || !scope.Complete {
				t.Fatal("empty models default must scan complete", scope)
			}
			continue
		}
		if scope.Status != "missing" || !scope.Complete {
			t.Fatal("missing default must remain missing", scope)
		}
	}
	model, _ := findModel("haar-frontal")
	path := filepath.Join(m.root, model.Embedded)
	put(t, path, "haar")
	put(t, filepath.Join(filepath.Dir(path), "955717e8-secret.th"), "sibling")
	inv = scanFixture(t, m)
	fs := mustFiles(t, m, inv.ID, "haar-frontal")
	if len(fs) != 1 || len(mustFiles(t, m, inv.ID, "demucs")) != 0 || *inv.AssetSize.Bytes != "4" {
		t.Fatal("Haar authorized its parent", fs, inv.AssetSize)
	}
	m.mu.Lock()
	access := m.inventories[inv.ID].access[fs[0].ScopeID]
	m.mu.Unlock()
	if f, _, err := access.scope.OpenFile("955717e8-secret.th"); err == nil {
		f.Close()
		t.Fatal("Haar sibling readable")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("955717e8-secret.th", path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	inv = partialInventory(t, m)
	if fs := mustFiles(t, m, inv.ID, "haar-frontal"); len(fs) != 0 {
		t.Fatal("default Haar leaf followed a link", fs)
	}
}

func TestCoreProjectAnchorSurvivesReplacementAndCloses(t *testing.T) {
	m := defaultScopeManager(t)
	put(t, filepath.Join(m.root, "models", "955717e8-old.th"), "old")
	moved := m.root + "-moved"
	err := os.Rename(m.root, moved)
	if err != nil {
		if runtime.GOOS != "windows" || (!os.IsPermission(err) && !errors.Is(err, syscall.Errno(32))) {
			t.Fatal(err)
		}
		t.Logf("Windows root sharing prevents replacement: %v", err)
	} else {
		t.Cleanup(func() { os.RemoveAll(moved) })
		put(t, filepath.Join(m.root, "models", "955717e8-new.th"), "new tree")
	}
	inv := scanFixture(t, m)
	fs := mustFiles(t, m, inv.ID, "demucs")
	if len(fs) != 1 || filepath.Base(fs[0].Path) != "955717e8-old.th" {
		t.Fatal("project anchor redirected to replacement", fs)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.projectScope.Derive("models"); !errors.Is(err, os.ErrClosed) {
		t.Fatal("project anchor leaked after Close", err)
	}
	if err != nil {
		if err := os.Rename(m.root, moved); err != nil {
			t.Fatal("Close did not release root handle", err)
		}
		if err := os.Rename(moved, m.root); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCoreCustomExternalRootsStayExplicit(t *testing.T) {
	m := defaultScopeManager(t)
	external := t.TempDir()
	put(t, filepath.Join(external, "955717e8-custom.th"), "custom")
	put(t, filepath.Join(m.root, "models", "955717e8-hidden.th"), "excluded")
	m.roots = []string{external}
	inv := scanFixture(t, m)
	fs := mustFiles(t, m, inv.ID, "demucs")
	if len(fs) != 1 || fs[0].Path != filepath.Join(external, "955717e8-custom.th") {
		t.Fatal("custom root semantics changed", fs)
	}
}

func TestCoreSnapshotLinksKeepRecognitionGroupsAndSafeRead(t *testing.T) {
	m := fixtureManager(t)
	for _, item := range []struct{ id, repo, name string }{
		{"seed-whisper", "models--openai--whisper-small", "model.safetensors"},
		{"bigvgan", "models--nvidia--bigvgan_v2_22khz_80band_256x", "bigvgan_generator.pt"},
	} {
		base := filepath.Join(m.root, "models", item.repo)
		blob := filepath.Join(base, "blobs", "opaque-blob")
		put(t, blob, "abc")
		for _, rev := range []string{"r1", "r2"} {
			path := filepath.Join(base, "snapshots", rev, item.name)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("..", "..", "blobs", "opaque-blob"), path); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
		}
	}
	inv := scanFixture(t, m)
	if *inv.AssetSize.Bytes != "6" || *inv.WeightSize.Bytes != "6" {
		t.Fatal("snapshot/blob counted twice", inv.AssetSize, inv.WeightSize)
	}
	for _, id := range []string{"seed-whisper", "bigvgan"} {
		fs := mustFiles(t, m, inv.ID, id)
		if len(fs) != 1 || fs[0].Evidence != "recognized_by_rule" || fs[0].LinkKind != "symlink" || len(fs[0].Aliases) != 2 {
			t.Fatal("snapshot source lost", id, fs)
		}
		for _, state := range inv.Models {
			if state.ID == id {
				if len(state.Groups) != 2 || state.CompositionStatus != "complete" {
					t.Fatal("snapshot revisions collapsed into blob directory", state)
				}
				for group, status := range state.Groups {
					if filepath.Base(filepath.Dir(group)) != "snapshots" || status != "complete" {
						t.Fatal("invalid snapshot group", group, status)
					}
				}
			}
		}
		o, err := m.StartVerify(context.Background(), inv.ID, id, []string{fs[0].ID})
		if err != nil {
			t.Fatal(err)
		}
		done := await(t, m, o.ID)
		if done.Status != "completed" || len(done.Hashes) != 1 || done.Hashes[0].SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
			t.Fatal("snapshot cannot be safely verified", done)
		}
		// Change the checked source to a relative link outside the asset root.
		outside := filepath.Join(t.TempDir(), "secret")
		put(t, outside, "secret")
		rel, err := filepath.Rel(filepath.Dir(fs[0].Path), outside)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(fs[0].Path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(rel, fs[0].Path); err != nil {
			t.Fatal(err)
		}
		o, err = m.StartVerify(context.Background(), inv.ID, id, []string{fs[0].ID})
		if err != nil {
			t.Fatal(err)
		}
		done = await(t, m, o.ID)
		if done.Status != "failed" || done.Hashes[0].SHA256 != "" {
			t.Fatal("display source weakened read boundary", done)
		}
	}
	inv = partialInventory(t, m)
	for _, id := range []string{"seed-whisper", "bigvgan"} {
		fs := mustFiles(t, m, inv.ID, id)
		if len(fs) != 1 || filepath.Base(filepath.Dir(fs[0].Path)) != "r2" {
			t.Fatal("escaped snapshot was counted", fs)
		}
	}
}

func TestCoreAliasesRetainEveryCompositionGroup(t *testing.T) {
	m := fixtureManager(t)
	for _, name := range []string{"f7e0c4bc-a.th", "d12395a8-a.th", "92cfc3b6-a.th", "04573f0d-a.th"} {
		first := filepath.Join(m.root, "models", "a", name)
		put(t, first, "x")
		second := filepath.Join(m.root, "models", "b", name)
		if err := os.MkdirAll(filepath.Dir(second), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(first, second); err != nil {
			t.Skipf("hardlink unavailable: %v", err)
		}
		third := filepath.Join(m.root, "models", "c", name)
		if err := os.MkdirAll(filepath.Dir(third), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..", "a", name), third); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
	}
	inv := scanFixture(t, m)
	fs := mustFiles(t, m, inv.ID, "demucs-ft")
	if len(fs) != 4 || *inv.WeightSize.Bytes != "4" {
		t.Fatal("aliases inflated accounting", fs, inv.WeightSize)
	}
	for _, file := range fs {
		if len(file.Aliases) != 2 {
			t.Fatal("alias lost", file)
		}
	}
	for _, state := range inv.Models {
		if state.ID == "demucs-ft" {
			if len(state.Groups) != 3 {
				t.Fatal("alias groups lost", state.Groups)
			}
			for _, status := range state.Groups {
				if status != "complete" {
					t.Fatal("alias group incomplete", state.Groups)
				}
			}
		}
	}
}

func TestCoreOverlapCannotBorrowParentLinkBoundary(t *testing.T) {
	m := fixtureManager(t)
	base := filepath.Join(m.root, "models", "models--openai--whisper-small")
	put(t, filepath.Join(base, "blobs", "opaque"), "abc")
	snapshot := filepath.Join(base, "snapshots", "rev")
	path := filepath.Join(snapshot, "model.safetensors")
	if err := os.MkdirAll(snapshot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "blobs", "opaque"), path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := m.Registration(context.Background(), "seed-whisper", snapshot, 0, false); err != nil {
		t.Fatal(err)
	}
	inv := partialInventory(t, m)
	if fs := mustFiles(t, m, inv.ID, "seed-whisper"); len(fs) != 0 {
		t.Fatal("overlap borrowed broader authorization", fs)
	}
}

func TestCoreOverlappingRegistrationRetainsRelativeAccess(t *testing.T) {
	m := fixtureManager(t)
	path := filepath.Join(m.root, "models", "nested", "955717e8-a.th")
	put(t, path, "abc")
	if _, err := m.Registration(context.Background(), "demucs", filepath.Dir(path), 0, false); err != nil {
		t.Fatal(err)
	}
	inv := scanFixture(t, m)
	fs := mustFiles(t, m, inv.ID, "demucs")
	if len(fs) != 1 || fs[0].Path != path || *inv.AssetSize.Bytes != "3" {
		t.Fatal("overlapping scope lost or duplicated file", fs, inv.AssetSize)
	}
	o, err := m.StartVerify(context.Background(), inv.ID, "demucs", []string{fs[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if done := await(t, m, o.ID); done.Status != "completed" {
		t.Fatal("overlapping scope lost restricted relative path", done)
	}
}

func TestCoreUnconfirmedHomeAnchorIsUnavailable(t *testing.T) {
	m := defaultScopeManager(t)
	m.homeScope.Close()
	m.homeScope = nil
	m.homeError = os.ErrNotExist
	inv := partialInventory(t, m)
	for _, scope := range inv.Scopes {
		if scope.Path == m.torchRoot && (scope.Status != "unavailable" || scope.Complete) {
			t.Fatal("unknown home boundary classified missing", scope)
		}
	}
}

func TestCoreRuntimeAndDepth(t *testing.T) {
	m := fixtureManager(t)
	put(t, filepath.Join(m.root, "sidecar", "venv", "Scripts", "python.exe"), "runtime")
	put(t, filepath.Join(m.root, "models", "deep", "nested", "955717e8-a.th"), "weight")
	inv := scanFixture(t, m)
	if inv.RuntimeSize.Bytes == nil || *inv.RuntimeSize.Bytes != "7" || *inv.AssetSize.Bytes != "6" {
		t.Fatalf("运行环境未独立分栏: %+v %+v", inv.RuntimeSize, inv.AssetSize)
	}
	m.scanDepth = 1
	o, e := m.StartScan(context.Background(), 0)
	if e != nil {
		t.Fatal(e)
	}
	if done := await(t, m, o.ID); done.Status != "partial" {
		t.Fatal(done.Status)
	}
}
func TestCoreRuntimeInterpreterMetadataOnly(t *testing.T) {
	for _, mode := range []string{"missing", "found", "empty", "directory", "limited"} {
		t.Run(mode, func(t *testing.T) {
			m := fixtureManager(t)
			rel := filepath.Join("bin", "python")
			if runtime.GOOS == "windows" {
				rel = filepath.Join("Scripts", "python.exe")
			}
			path := filepath.Join(m.root, "sidecar", "venv", rel)
			switch mode {
			case "found", "limited":
				// Deliberately not executable: discovery must not run it or read a version.
				put(t, path, "not executable python")
			case "empty":
				put(t, path, "")
			case "directory":
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "limited" {
				m.scanEntries = 1
			}
			o, err := m.StartScan(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			done := await(t, m, o.ID)
			inv, err := m.Inventory(done.ResultID)
			if err != nil || len(inv.RuntimeEnvironments) != 1 {
				t.Fatal(inv.RuntimeEnvironments, err)
			}
			e := inv.RuntimeEnvironments[0]
			want := map[string]string{"missing": "missing", "found": "found", "empty": "empty_file", "directory": "missing", "limited": "not_checked"}[mode]
			if e.Kind != "development_venv" || e.InterpreterPath != path || e.InterpreterStatus != want || e.Inference != "not_tested" || e.DeliveryStatus != "pending_build" {
				t.Fatalf("解释器证据不正确: %+v", e)
			}
			if mode == "found" && (e.InterpreterBytes == nil || *e.InterpreterBytes != "21") {
				t.Fatal("缺少精确元数据", e)
			}
		})
	}
}

func TestCoreInstalledScanUsesOnlyValidatedLayout(t *testing.T) {
	root := t.TempDir()
	layout := resourceLayout{Schema: 1, Center: "model-center/fengshen-model-center.exe", Components: "model-center/components", RootKind: "installed", ModelRoots: []string{"assets/weights"}, HaarFiles: []string{"assets/haarcascade_frontalface_default.xml"}}
	body, err := json.Marshal(layout)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(root, "resource-layout.json"), string(body))
	put(t, filepath.Join(root, "assets", "weights", "955717e8-a.th"), "ok")
	put(t, filepath.Join(root, "assets", "haarcascade_frontalface_default.xml"), "haar")
	put(t, filepath.Join(root, "sidecar", "venv", "Scripts", "python.exe"), "excluded")
	put(t, filepath.Join(root, "sidecar", "seed-vc", "checkpoints", "955717e8-hidden.th"), "excluded")
	for _, model := range catalog() {
		if model.Embedded != "" {
			put(t, filepath.Join(root, model.Embedded), "excluded")
		}
	}
	resolved, validated, err := resolveStartupRoot(root, "installed")
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	for _, rel := range validated.ModelRoots {
		roots = append(roots, filepath.Join(resolved, filepath.FromSlash(rel)))
	}
	m, err := newManagerAt(resolved, t.TempDir(), roots, "installed")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	m.haarFiles = validated.HaarFiles
	inv := scanFixture(t, m)
	if *inv.AssetSize.Bytes != "6" || inv.RuntimeSize.Bytes != nil || len(inv.RuntimeEnvironments) != 0 || len(inv.Scopes) != 2 {
		t.Fatal("安装模式扫描越出布局", inv)
	}
	if fs := mustFiles(t, m, inv.ID, "demucs"); len(fs) != 1 {
		t.Fatal(fs)
	}
	if fs := mustFiles(t, m, inv.ID, "haar-profile"); len(fs) != 0 {
		t.Fatal("回落开发 Haar", fs)
	}
}

func TestCoreScopeLimitPartialKeepsComplete(t *testing.T) {
	m := fixtureManager(t)
	put(t, filepath.Join(m.root, "models", "955717e8-a.th"), "a")
	first := scanFixture(t, m)
	m.scanEntries = 1
	put(t, filepath.Join(m.root, "models", "955717e8-b.th"), "bb")
	o, e := m.StartScan(context.Background(), 0)
	if e != nil {
		t.Fatal(e)
	}
	last := await(t, m, o.ID)
	if last.Status != "partial" {
		t.Fatalf("%+v", last)
	}
	if m.State().LastCompleteInventory != first.ID {
		t.Fatal("上次完整结果被覆盖")
	}
}
func TestCorePaginationAndHardlinks(t *testing.T) {
	m := fixtureManager(t)
	for n := 0; n < 205; n++ {
		put(t, filepath.Join(m.root, "models", fmt.Sprintf("955717e8-%03d.th", n)), "x")
	}
	inv := scanFixture(t, m)
	a, next, e := m.Files(inv.ID, "demucs", "", 0)
	if e != nil || len(a) != 200 || next == "" {
		t.Fatal(len(a), next, e)
	}
	b, end, e := m.Files(inv.ID, "demucs", next, 200)
	if e != nil || len(b) != 5 || end != "" {
		t.Fatal(len(b), end, e)
	}
	if _, _, e = m.Files(inv.ID, "campplus", next, 200); e == nil {
		t.Fatal("跨模型游标被接受")
	}
	source := filepath.Join(m.root, "models", "955717e8-000.th")
	alias := filepath.Join(m.root, "models", "955717e8-alias.th")
	if e = os.Link(source, alias); e != nil {
		t.Skipf("平台不允许创建硬链接: %v", e)
	}
	inv = scanFixture(t, m)
	for _, s := range inv.Models {
		if s.ID == "demucs" && s.FileCount != 205 {
			t.Fatal("硬链接重复计数", s.FileCount)
		}
	}
}
func TestCoreEveryModelDiscoveryAndComposition(t *testing.T) {
	m := defaultScopeManager(t)
	for _, rel := range []string{
		"models/955717e8-main.th",
		"models/ft/f7e0c4bc-a.th", "models/ft/d12395a8-a.th", "models/ft/92cfc3b6-a.th", "models/ft/04573f0d-a.th",
		"models/seed/DiT_seed_v2_uvit_whisper_small_wavenet_bigvgan_pruned.pth",
		"models/models--openai--whisper-small/snapshots/rev/model.safetensors",
		"models/campplus_cn_common.bin",
		"models/models--nvidia--bigvgan_v2_22khz_80band_256x/snapshots/rev/bigvgan_generator.pt",
	} {
		put(t, filepath.Join(m.root, filepath.FromSlash(rel)), "x")
	}
	for _, id := range []string{"haar-frontal", "haar-profile"} {
		model, _ := findModel(id)
		put(t, filepath.Join(m.root, model.Embedded), "x")
	}
	for _, id := range []string{"whisper-asr", "whisper-vad", "local-text", "local-vision"} {
		extension := ".gguf"
		if id == "whisper-vad" {
			extension = ".onnx"
		}
		p := filepath.Join(t.TempDir(), id+extension)
		put(t, p, "candidate")
		if _, err := m.Registration(context.Background(), id, p, m.State().Registry.Revision, false); err != nil {
			t.Fatal(err)
		}
	}
	put(t, filepath.Join(m.root, "models", "wrong-repository", "model.safetensors"), "excluded")
	put(t, filepath.Join(m.root, "models", "955717e8-empty.th"), "")
	inv := scanFixture(t, m)
	for _, model := range inv.Models {
		if model.Inference != "not_tested" || model.VerificationStatus != "not_checked" {
			t.Fatal("discovery promoted evidence", model)
		}
		switch model.ID {
		case "whisper-asr", "whisper-vad", "local-text", "local-vision":
			if model.FileCount != 1 || model.DiscoveryStatus != "registered_identity_unconfirmed" || model.Size.Bytes != nil {
				t.Fatalf("candidate acquired identity or capacity: %+v", model)
			}
		case "demucs":
			if model.FileCount != 2 || model.DiscoveryStatus != "empty_file" || model.CompositionStatus != "complete" {
				t.Fatal(model)
			}
		default:
			want := 1
			if model.ID == "demucs-ft" {
				want = 4
			}
			if model.FileCount != want || model.IdentityStatus != "recognized_by_rule" || model.CompositionStatus != "complete" {
				t.Fatalf("recognized composition missing: %+v", model)
			}
		}
		if model.ID == "seed-vc" && model.DependencyStatus != "files_found" {
			t.Fatal("Seed-VC dependency evidence missing", model)
		}
	}
	put(t, filepath.Join(m.root, "models", "ft", "04573f0d-a.th"), "")
	if err := os.Remove(filepath.Join(m.root, "models", "campplus_cn_common.bin")); err != nil {
		t.Fatal(err)
	}
	inv = scanFixture(t, m)
	for _, model := range inv.Models {
		if model.ID == "demucs-ft" && model.CompositionStatus != "incomplete" {
			t.Fatal("empty submodel completed suite", model)
		}
		if model.ID == "seed-vc" && model.DependencyStatus != "incomplete" {
			t.Fatal("missing dependency completed Seed-VC", model)
		}
	}
}

func TestCoreSeedSuiteUsesFileObjectUnion(t *testing.T) {
	m := defaultScopeManager(t)
	paths := []string{
		"models/DiT_seed_v2_uvit_whisper_small_wavenet_bigvgan_pruned.pth",
		"models/models--openai--whisper-small/snapshots/rev/model.safetensors",
		"models/campplus_cn_common.bin",
		"models/models--nvidia--bigvgan_v2_22khz_80band_256x/snapshots/rev/bigvgan_generator.pt",
	}
	source := filepath.Join(m.root, filepath.FromSlash(paths[0]))
	put(t, source, "abc")
	for _, rel := range paths[1:] {
		path := filepath.Join(m.root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(source, path); err != nil {
			t.Fatal(err)
		}
	}
	put(t, filepath.Join(m.root, "sidecar", "venv", "runtime.dat"), "runtime")
	inv := scanFixture(t, m)
	for _, size := range []SizeMetric{inv.WeightSize, inv.AssetSize, inv.SeedSuiteSize} {
		if size.Bytes == nil || *size.Bytes != "3" || size.CountedFiles != 1 || !size.Complete {
			t.Fatalf("shared object counted more than once: %+v", size)
		}
	}
	if inv.RuntimeSize.Bytes == nil || *inv.RuntimeSize.Bytes != "7" || inv.RuntimeSize.CountedFiles != 1 {
		t.Fatalf("runtime merged with weights: %+v", inv.RuntimeSize)
	}
	for _, model := range inv.Models {
		if model.ID != "seed-vc" && model.ID != "seed-whisper" && model.ID != "campplus" && model.ID != "bigvgan" {
			continue
		}
		if model.FileCount != 1 || model.Size.Bytes == nil || *model.Size.Bytes != "3" || model.CompositionStatus != "complete" || model.Inference != "not_tested" {
			t.Fatalf("shared model evidence missing: %+v", model)
		}
	}
}

func TestKnownStandaloneModelComposition(t *testing.T) {
	for _, mode := range []string{"complete", "missing-shard", "empty-shard", "split-directory", "two-asr-models"} {
		t.Run(mode, func(t *testing.T) {
			m := fixtureManager(t)
			put(t, filepath.Join(m.root, "models", "asr", "ggml-small.bin"), "abc")
			put(t, filepath.Join(m.root, "models", "vad", "ggml-silero-v5.1.2.bin"), "abc")
			if mode == "two-asr-models" {
				put(t, filepath.Join(m.root, "models", "asr", "ggml-large-v3-turbo.bin"), "abc")
			}
			put(t, filepath.Join(m.root, "models", "text", "qwen2.5-7b-instruct-q4_k_m-00001-of-00002.gguf"), "abc")
			if mode != "missing-shard" {
				dir, body := "text", "abc"
				if mode == "empty-shard" {
					body = ""
				}
				if mode == "split-directory" {
					dir = "other"
				}
				put(t, filepath.Join(m.root, "models", dir, "qwen2.5-7b-instruct-q4_k_m-00002-of-00002.gguf"), body)
			}
			inv := scanFixture(t, m)
			for _, state := range inv.Models {
				if state.ID != "whisper-asr" && state.ID != "whisper-vad" && state.ID != "local-text" {
					continue
				}
				want := "complete"
				if state.ID == "local-text" && mode != "complete" && mode != "two-asr-models" {
					want = "incomplete"
				}
				if state.CompositionStatus != want || state.IdentityStatus != "recognized_by_rule" || state.Inference != "not_tested" || state.VerificationStatus != "not_checked" {
					t.Fatalf("known model evidence differs: %+v", state)
				}
			}
		})
	}
}

var knownVisionPairs = []struct{ repo, model, projector string }{
	{"models--ggml-org--SmolVLM-500M-Instruct-GGUF", "SmolVLM-500M-Instruct-Q8_0.gguf", "mmproj-SmolVLM-500M-Instruct-f16.gguf"},
	{"models--ggml-org--Qwen3-VL-2B-Instruct-GGUF", "Qwen3-VL-2B-Instruct-Q8_0.gguf", "mmproj-Qwen3-VL-2B-Instruct-Q8_0.gguf"},
}

func TestVisionCatalogExactRepositoryPairs(t *testing.T) {
	model, _ := findModel("local-vision")
	if len(model.ComponentAlternatives) != 2 {
		t.Errorf("want exactly two vision alternatives, got %v", model.ComponentAlternatives)
	}
	for i, pair := range knownVisionPairs {
		for _, name := range []string{pair.model, pair.projector} {
			rule := pair.repo + "/snapshots/{revision}/" + name
			if !contains(model.RecognitionRules, rule) {
				t.Errorf("missing recognition rule %s", rule)
			}
			for _, repo := range []string{pair.repo, knownVisionPairs[1-i].repo, pair.repo + "-other", "plain"} {
				path := filepath.Join(t.TempDir(), repo, "snapshots", "r1", name)
				if got := recognizes(model, path); got != (repo == pair.repo) {
					t.Errorf("recognizes(%s) = %v", path, got)
				}
			}
			for _, rel := range []string{name, "snapshots/" + name, "snapshots/r1/nested/" + name, "snapshots/r1/" + name + ".other"} {
				if recognizes(model, filepath.Join(t.TempDir(), pair.repo, filepath.FromSlash(rel))) {
					t.Errorf("recognized invalid layout %s", rel)
				}
			}
		}
	}
}

func TestVisionPairScannerAndBinding(t *testing.T) {
	for i, pair := range knownVisionPairs {
		for _, mode := range []string{"complete", "missing-model", "missing-projector", "empty-model", "empty-projector", "split-revision", "mixed-projector", "mixed-model", "swapped-repository", "duplicate-revisions", "both-families"} {
			t.Run(pair.model+"/"+mode, func(t *testing.T) {
				m := fixtureManager(t)
				repo, model, projector := pair.repo, pair.model, pair.projector
				if mode == "swapped-repository" {
					repo = knownVisionPairs[1-i].repo
				}
				if mode == "mixed-projector" {
					projector = knownVisionPairs[1-i].projector
				}
				if mode == "mixed-model" {
					model = knownVisionPairs[1-i].model
				}
				base := filepath.Join(m.root, "models", repo, "snapshots")
				for index, name := range []string{model, projector} {
					if mode == "missing-model" && index == 0 || mode == "missing-projector" && index == 1 {
						continue
					}
					body, rev := "fixture", "r1"
					if mode == "empty-model" && index == 0 || mode == "empty-projector" && index == 1 {
						body = ""
					}
					if mode == "split-revision" && index == 1 {
						rev = "r2"
					}
					put(t, filepath.Join(base, rev, name), body)
					if mode == "duplicate-revisions" {
						put(t, filepath.Join(base, "r2", name), body)
					}
				}
				if mode == "both-families" {
					other := knownVisionPairs[1-i]
					for _, name := range []string{other.model, other.projector} {
						put(t, filepath.Join(m.root, "models", other.repo, "snapshots", "r1", name), "fixture")
					}
				}
				inv := scanFixture(t, m)
				for _, state := range inv.Models {
					if state.ID != "local-vision" {
						continue
					}
					complete := mode == "complete" || mode == "duplicate-revisions" || mode == "both-families"
					if (state.CompositionStatus == "complete") != complete || state.Inference != "not_tested" || state.VerificationStatus != "not_checked" {
						t.Fatalf("unexpected scanner evidence: %+v", state)
					}
					if mode == "swapped-repository" && (state.FileCount != 0 || state.IdentityStatus != "unconfirmed") {
						t.Fatal("cross-family repository recognized", state)
					}
				}
				request := bindingSelection(t, m, "highlight.vision", "llama-runtime", "local-vision")
				m.mu.Lock()
				binding, err := m.resolveBindingLocked(context.Background(), "highlight.vision", request)
				m.mu.Unlock()
				if (err == nil) != (mode == "complete") {
					t.Fatalf("binding accepted=%v for %s: %v", err == nil, mode, err)
				}
				if err == nil && (len(binding.Files) != 2 || binding.Files[0].Group != binding.Files[1].Group) {
					t.Fatal("pair lost same revision group")
				}
			})
		}
	}
}

func TestKnownVisionComposition(t *testing.T) {
	for _, mode := range []string{"complete", "missing-projector", "empty-projector", "split-revision", "wrong-projector", "wrong-repository"} {
		t.Run(mode, func(t *testing.T) {
			m := fixtureManager(t)
			repo := "models--ggml-org--SmolVLM-500M-Instruct-GGUF"
			if mode == "wrong-repository" {
				repo += "-other"
			}
			base := filepath.Join(t.TempDir(), repo)
			put(t, filepath.Join(base, "snapshots", "r1", "SmolVLM-500M-Instruct-Q8_0.gguf"), "abc")
			if mode != "missing-projector" {
				rev, name, body := "r1", "mmproj-SmolVLM-500M-Instruct-f16.gguf", "abc"
				if mode == "empty-projector" {
					body = ""
				}
				if mode == "split-revision" {
					rev = "r2"
				}
				if mode == "wrong-projector" {
					name = "mmproj-other-f16.gguf"
				}
				put(t, filepath.Join(base, "snapshots", rev, name), body)
			}
			if _, err := m.Registration(context.Background(), "local-vision", base, 0, false); err != nil {
				t.Fatal(err)
			}
			inv := scanFixture(t, m)
			for _, state := range inv.Models {
				if state.ID != "local-vision" {
					continue
				}
				want := "incomplete"
				if mode == "complete" {
					want = "complete"
				}
				if state.CompositionStatus != want || state.Inference != "not_tested" || state.VerificationStatus != "not_checked" {
					t.Fatal(state)
				}
				if mode == "wrong-repository" && (state.IdentityStatus != "unconfirmed" || state.Size.Bytes != nil) {
					t.Fatal("repository boundary lost", state)
				}
				if mode != "wrong-repository" && state.IdentityStatus != "recognized_by_rule" {
					t.Fatal("vision rule missing", state)
				}
			}
		})
	}
}

func TestCoreDemucsHuggingFaceComposition(t *testing.T) {
	for _, mode := range []string{"complete", "missing", "empty", "split-revision", "mixed-format", "both-formats"} {
		t.Run(mode, func(t *testing.T) {
			m := fixtureManager(t)
			main := filepath.Join(m.root, "models", "models--adefossez--HTDemucs", "snapshots", "r1", "955717e8.safetensors")
			put(t, main, "abc")
			base := filepath.Join(m.root, "models", "models--adefossez--HTDemucs-ft", "snapshots")
			for i, sig := range []string{"f7e0c4bc", "d12395a8", "92cfc3b6", "04573f0d"} {
				rev, name, body := "r1", sig+".safetensors", "abc"
				if i == 3 {
					switch mode {
					case "missing":
						continue
					case "empty":
						body = ""
					case "split-revision":
						rev = "r2"
					case "mixed-format":
						name = sig + "-legacy.th"
					}
				}
				put(t, filepath.Join(base, rev, name), body)
				if mode == "both-formats" {
					put(t, filepath.Join(base, rev, sig+"-legacy.th"), "abc")
				}
			}
			inv := scanFixture(t, m)
			for _, state := range inv.Models {
				if state.ID != "demucs" && state.ID != "demucs-ft" {
					continue
				}
				want := "incomplete"
				if state.ID == "demucs" || mode == "complete" || mode == "both-formats" {
					want = "complete"
				}
				if state.CompositionStatus != want || state.IdentityStatus != "recognized_by_rule" || state.Inference != "not_tested" || state.VerificationStatus != "not_checked" {
					t.Fatalf("format composition or evidence changed: %+v", state)
				}
			}
		})
	}
}

func TestCoreRegisteredDemucsSnapshotLinkBoundary(t *testing.T) {
	for _, narrow := range []bool{false, true} {
		t.Run(fmt.Sprint(narrow), func(t *testing.T) {
			m := fixtureManager(t)
			base := filepath.Join(t.TempDir(), "models--adefossez--HTDemucs")
			put(t, filepath.Join(base, "blobs", "opaque"), "abc")
			snapshot := filepath.Join(base, "snapshots", "rev")
			if err := os.MkdirAll(snapshot, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("..", "..", "blobs", "opaque"), filepath.Join(snapshot, "955717e8.safetensors")); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			registered := base
			if narrow {
				registered = snapshot
			}
			if _, err := m.Registration(context.Background(), "demucs", registered, 0, false); err != nil {
				t.Fatal(err)
			}
			var inv Inventory
			if narrow {
				inv = partialInventory(t, m)
				if len(mustFiles(t, m, inv.ID, "demucs")) != 0 {
					t.Fatal("snapshot escaped registration boundary")
				}
				return
			}
			inv = scanFixture(t, m)
			fs := mustFiles(t, m, inv.ID, "demucs")
			if len(fs) != 1 || fs[0].Evidence != "recognized_by_rule" || fs[0].LinkKind != "symlink" || len(fs[0].Aliases) != 1 || inv.WeightSize.Bytes == nil || *inv.WeightSize.Bytes != "3" || *inv.AssetSize.Bytes != "3" {
				t.Fatal("repository link identity or accounting differs", fs, inv.WeightSize, inv.AssetSize)
			}
			if len(mustFiles(t, m, inv.ID, "demucs-ft")) != 0 {
				t.Fatal("registration authorized a different model")
			}
		})
	}
}

func TestCoreCapacityWithoutReliableIdentity(t *testing.T) {
	files := map[string]safefs.Entry{}
	for _, path := range []string{"first", "second"} {
		entry := safefs.Entry{Path: filepath.Join(t.TempDir(), path), Size: 3}
		files[fileKey(entry)] = entry
		files[fileKey(entry)] = entry
	}
	measured := sumEntries(files, true, true)
	if measured.Bytes == nil || *measured.Bytes != "6" || measured.CountedFiles != 2 || measured.IdentityQuality != "limited_path_only" || !measured.Complete {
		t.Fatalf("unconfirmed identity merged copies: %+v", measured)
	}
	partial := sumEntries(files, false, true)
	if partial.Bytes == nil || *partial.Bytes != "6" || partial.Complete {
		t.Fatalf("partial capacity promoted: %+v", partial)
	}
	unknown := sumEntries(nil, true, false)
	zero := sumEntries(nil, true, true)
	if unknown.Bytes != nil || zero.Bytes == nil || *zero.Bytes != "0" {
		t.Fatalf("unknown and measured zero conflated: unknown=%+v zero=%+v", unknown, zero)
	}
}

func TestCoreScanTimeoutKeepsLastComplete(t *testing.T) {
	m := fixtureManager(t)
	first := scanFixture(t, m)
	entered := make(chan struct{})
	m.scanTimeout = 50 * time.Millisecond
	m.beforeScan = func(ctx context.Context) { close(entered); <-ctx.Done() }
	op, err := m.StartScan(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	done := await(t, m, op.ID)
	if done.Status == "completed" || done.FinishedAt == nil || m.State().LastCompleteInventory != first.ID {
		t.Fatalf("timeout published full success: %+v", done)
	}
	partial, err := m.Inventory(done.ResultID)
	if err != nil || partial.Complete {
		t.Fatalf("timeout lost incomplete result: %+v, %v", partial, err)
	}
}

func TestCoreRegistrationIsolatedAndStaleEntry(t *testing.T) {
	m := fixtureManager(t)
	p := filepath.Join(t.TempDir(), "955717e8-a.th")
	put(t, p, "abc")
	_, e := m.Registration(context.Background(), "demucs-ft", filepath.Dir(p), 0, false)
	if e != nil {
		t.Fatal(e)
	}
	inv := scanFixture(t, m)
	fs, _, e := m.Files(inv.ID, "demucs", "", 200)
	if e != nil || len(fs) != 0 {
		t.Fatal("外部登记泄露到另一目录项", fs, e)
	}
	if _, e = m.StartVerify(context.Background(), inv.ID, "demucs", []string{"arbitrary-path"}); e == nil {
		t.Fatal("接受任意 ID")
	}
}
