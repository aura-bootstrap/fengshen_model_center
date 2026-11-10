package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"path/filepath"
	"testing"
	"time"
)

func fixtureManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	for _, p := range []string{"sidecar", "models", filepath.Join("assets", "haar")} {
		if err := os.MkdirAll(filepath.Join(root, p), 0700); err != nil {
			t.Fatal(err)
		}
	}
	m, err := NewManager(root, t.TempDir(), testManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	allowFixtureRoots(m, filepath.Join(root, "models"), filepath.Join(root, "assets", "haar"))
	t.Cleanup(func() { m.Close() })
	return m
}
func await(t *testing.T, m *Manager, id string) *Operation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		o, e := m.Operation(id)
		if e != nil {
			t.Fatal(e)
		}
		if o.Status != "running" && o.Status != "cancelling" {
			return o
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("操作未结束")
	return nil
}
func put(t *testing.T, p, s string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(s), 0600); e != nil {
		t.Fatal(e)
	}
}
func scanFixture(t *testing.T, m *Manager) Inventory {
	t.Helper()
	o, e := m.StartScan(context.Background(), m.State().Registry.Revision)
	if e != nil {
		t.Fatal(e)
	}
	done := await(t, m, o.ID)
	if done.Status != "completed" {
		t.Fatalf("scan: %+v", done)
	}
	inv, e := m.Inventory(done.ResultID)
	if e != nil {
		t.Fatal(e)
	}
	return inv
}
func TestCoreScanCopiesAndGroups(t *testing.T) {
	m := fixtureManager(t)
	for i, p := range []string{"a/f7e0c4bc-a.th", "b/d12395a8-a.th", "c/92cfc3b6-a.th", "d/04573f0d-a.th", "x/955717e8-a.th", "y/955717e8-a.th"} {
		put(t, filepath.Join(m.root, "models", p), string(rune('a'+i)))
	}
	inv := scanFixture(t, m)
	for _, s := range inv.Models {
		if s.ID == "demucs-ft" && s.CompositionStatus == "complete" {
			t.Fatal("跨目录凑套")
		}
		if s.ID == "demucs" {
			if s.FileCount != 2 || s.Size.Bytes == nil || *s.Size.Bytes != "2" {
				t.Fatalf("副本丢失: %+v", s)
			}
		}
	}
	if *inv.AssetSize.Bytes != "6" {
		t.Fatal(*inv.AssetSize.Bytes)
	}
}
func TestCoreRetentionAndImmutableQueries(t *testing.T) {
	m := fixtureManager(t)
	inv := scanFixture(t, m)
	copy, e := m.Inventory(inv.ID)
	if e != nil {
		t.Fatal(e)
	}
	copy.Models[0].Name = "mutated"
	fresh, _ := m.Inventory(inv.ID)
	if fresh.Models[0].Name == "mutated" {
		t.Fatal("查询泄露可变引用")
	}
	m.mu.Lock()
	for n := 0; n < 40; n++ {
		end := time.Now().UTC().Add(time.Duration(n) * time.Millisecond)
		id := opaqueID()
		m.operations[id] = &Operation{ID: id, Status: "completed", FinishedAt: &end}
	}
	m.collect()
	if len(m.operations) > 32 {
		t.Fatal("操作记录未限制", len(m.operations))
	}
	old := time.Now().UTC().Add(-31 * time.Minute)
	for _, op := range m.operations {
		op.FinishedAt = &old
	}
	m.collect()
	if len(m.operations) != 0 || m.inventories[inv.ID] == nil {
		t.Fatal("回收删除受保护盘点或保留过期操作")
	}
	m.mu.Unlock()
}
func TestCoreBoundedCursorsAndTombstones(t *testing.T) {
	m := fixtureManager(t)
	put(t, filepath.Join(m.root, "models", "955717e8-a.th"), "a")
	put(t, filepath.Join(m.root, "models", "955717e8-b.th"), "b")
	inv := scanFixture(t, m)
	_, first, err := m.Files(inv.ID, "demucs", "", 1)
	if err != nil || first == "" {
		t.Fatal(first, err)
	}
	for n := 0; n < maxPageCursors*2; n++ {
		_, next, err := m.Files(inv.ID, "demucs", "", 1)
		if err != nil || next != first {
			t.Fatal("重复 GET 创建新游标", next, err)
		}
	}
	m.mu.Lock()
	for n := 0; n < maxPageCursors*2; n++ {
		m.cacheCursor(pageCursor{inventory: inv.ID, model: "demucs", offset: n + 1})
	}
	if len(m.cursors) > maxPageCursors {
		t.Fatal("游标缓存无界", len(m.cursors))
	}
	for id, p := range m.cursors {
		p.createdAt = time.Now().Add(-cursorLifetime - time.Second)
		m.cursors[id] = p
	}
	m.mu.Unlock()
	if _, _, err := m.Files(inv.ID, "demucs", first, 1); err == nil {
		t.Fatal("接受过期游标")
	}
	m.mu.Lock()
	if len(m.cursors) != 0 {
		t.Fatal("GET 未回收过期游标")
	}
	oldest, newest := "", ""
	for n := 0; n < maxInventoryTombstones+1; n++ {
		id := opaqueID()
		if n == 0 {
			oldest = id
		}
		newest = id
		m.inventories[id] = &Inventory{ID: id}
		m.collect()
	}
	if len(m.gone) > maxInventoryTombstones || len(m.goneOrder) > maxInventoryTombstones {
		t.Fatal("墓碑缓存无界")
	}
	if m.gone[oldest] || !m.gone[newest] || m.inventories[inv.ID] == nil {
		t.Fatal("墓碑回收或受保护盘点不正确")
	}
	m.mu.Unlock()
	if _, err := m.Inventory(newest); err == nil || err.(*apiError).Code != "inventory_gone" {
		t.Fatal("近期回收结果未返回 410", err)
	}
	if _, err := m.Inventory(oldest); err == nil || err.(*apiError).Code != "inventory_unknown" {
		t.Fatal("超出有界历史未返回 404", err)
	}
}

func TestCoreComponentOperationEvidenceAndSnapshot(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			m := fixtureManager(t)
			dir := filepath.Join(m.root, "model-center", "components", "whisper-batch", "1")
			payload := "not an executable"
			sum := sha256.Sum256([]byte(payload))
			manifest := components.Manifest{Schema: 1, ComponentID: "whisper-batch", Version: "1", OS: runtime.GOOS, Arch: runtime.GOARCH, EntryType: "executable", Entry: "runner.exe", Packaging: "native", Files: []components.File{{Path: "runner.exe", Bytes: fmt.Sprint(len(payload)), SHA256: hex.EncodeToString(sum[:])}}}
			if mismatch {
				payload = "different payload"
			}
			put(t, filepath.Join(dir, "runner.exe"), payload)
			body, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			put(t, filepath.Join(dir, "manifest.json"), string(body))
			o, err := m.StartVerifyComponent(context.Background(), "whisper-batch", "1")
			if err != nil {
				t.Fatal(err)
			}
			done := await(t, m, o.ID)
			want, status := "matched", "completed"
			if mismatch {
				want, status = "mismatch", "failed"
			}
			if done.Status != status || done.Evidence == nil || done.Evidence.Integrity != want || done.Evidence.ComponentID != "whisper-batch" || done.Evidence.Version != "1" || done.Evidence.Inference != "not_tested" {
				t.Fatalf("组件结果丢失: %+v", done)
			}
			// No Components GET: terminal publication must populate the manager cache,
			// including failure evidence. Integration projection is maintained separately.
			m.mu.Lock()
			cached := cloneJSON(m.componentSnapshot)
			m.mu.Unlock()
			found := false
			for _, evidence := range cached {
				found = found || evidence.ComponentID == "whisper-batch" && evidence.Integrity == want
			}
			if !found {
				t.Fatal("完成后组件快照未刷新", cached)
			}
			if !mismatch {
				found = false
				for _, evidence := range m.dependencySnapshot().Components {
					found = found || evidence.ID == "whisper-batch" && evidence.Integrity == "matched"
				}
				if !found {
					t.Fatal("无需 GET 即应可见完成的组件校验")
				}
			}
			if err := os.Remove(filepath.Join(dir, "manifest.json")); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			for _, evidence := range m.componentSnapshot {
				if evidence.ComponentID == "whisper-batch" && evidence.Integrity != want {
					t.Error("内存组件缓存自动读取磁盘")
				}
			}
			m.mu.Unlock()
			done.Evidence.Integrity = "mutated"
			fresh, _ := m.Operation(o.ID)
			if fresh.Evidence.Integrity != want {
				t.Fatal("Evidence 查询泄露可变引用")
			}
		})
	}
}

func TestCoreComponentCancellationPublishesNoSuccess(t *testing.T) {
	m := fixtureManager(t)
	// Hold observation publication so cancellation deterministically precedes Verify.
	m.componentMu.Lock()
	o, err := m.StartVerifyComponent(context.Background(), "whisper-batch", "1")
	if err != nil {
		m.componentMu.Unlock()
		t.Fatal(err)
	}
	if _, err := m.StartScan(context.Background(), 0); err == nil {
		t.Error("组件校验未复用全局单操作")
	}
	m.Cancel(o.ID)
	m.componentMu.Unlock()
	done := await(t, m, o.ID)
	if done.Status != "cancelled" || done.Evidence == nil || done.Evidence.Integrity != "cancelled" || done.Evidence.Inference != "not_tested" {
		t.Fatal("取消组件校验发布成功或丢失证据", done)
	}
	for _, e := range m.dependencySnapshot().Components {
		if e.ID == "whisper-batch" && e.Integrity == "matched" {
			t.Fatal("取消后缓存显示校验通过")
		}
	}
}
