package main

import (
	"context"
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
	for _, rel := range []string{"sidecar", "models", filepath.Join("assets", "haar")} {
		if err := os.MkdirAll(filepath.Join(root, rel), 0700); err != nil {
			t.Fatal(err)
		}
	}
	m, err := NewManager(root, t.TempDir(), testManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	allowFixtureRoots(m, filepath.Join(root, "models"), filepath.Join(root, "sidecar", "seed-vc", "checkpoints"), filepath.Join(root, "assets", "haar"))
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
	inv := scanFixture(t, m)
	for _, size := range []SizeMetric{inv.WeightSize, inv.AssetSize, inv.SeedSuiteSize} {
		if size.Bytes == nil || *size.Bytes != "3" || size.CountedFiles != 1 || !size.Complete {
			t.Fatalf("shared object counted more than once: %+v", size)
		}
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
