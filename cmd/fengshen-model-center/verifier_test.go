package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// coreVerifyGate pauses at a particular Read call, not merely before opening
// the file. The second call follows one complete 1 MiB read for these fixtures.
func coreVerifyGate(t *testing.T, m *Manager, read int) (<-chan struct{}, func()) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	calls := 0
	m.beforeRead = func(ctx context.Context) {
		calls++
		if calls != read {
			return
		}
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	return entered, unblock
}

func coreVerifySignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal("worker 未到达同步关口")
	}
}

func TestCoreVerifyFileIDLimitAndOwnership(t *testing.T) {
	m := fixtureManager(t)
	for n := 0; n < 1001; n++ {
		put(t, filepath.Join(m.root, "models", "955717e8-"+strconv.Itoa(n)+".th"), "abc")
	}
	inv := scanFixture(t, m)
	files, next, err := m.Files(inv.ID, "demucs", "", 1000)
	if err != nil || len(files) != 1000 || next == "" {
		t.Fatalf("first page: count=%d next=%q err=%v", len(files), next, err)
	}
	tail, end, err := m.Files(inv.ID, "demucs", next, 1000)
	if err != nil || len(tail) != 1 || end != "" {
		t.Fatalf("last page: count=%d next=%q err=%v", len(tail), end, err)
	}
	ids := make([]string, len(files))
	for n, file := range files {
		ids[n] = file.ID
	}
	if _, err := m.StartVerify(context.Background(), inv.ID, "demucs", append(append([]string{}, ids...), tail[0].ID)); err == nil || err.(*apiError).Code != "request_invalid" {
		t.Fatalf("1001 unique IDs accepted: %v", err)
	}
	if _, err := m.StartVerify(context.Background(), inv.ID, "demucs-ft", ids[:1]); err == nil || err.(*apiError).Code != "file_not_allowed" {
		t.Fatalf("another model acquired file: %v", err)
	}
	if m.State().ActiveOperation != nil {
		t.Fatal("rejected request created operation")
	}
	op, err := m.StartVerify(context.Background(), inv.ID, "demucs", ids)
	if err != nil {
		t.Fatal(err)
	}
	done := await(t, m, op.ID)
	if done.Status != "completed" || done.CheckedEntries != 1000 || done.ProcessedBytes != "3000" || len(done.Hashes) != 1000 {
		t.Fatalf("1000 IDs did not complete: status=%s count=%d bytes=%s hashes=%d", done.Status, done.CheckedEntries, done.ProcessedBytes, len(done.Hashes))
	}
	for n, hash := range done.Hashes {
		if hash.FileID != ids[n] || hash.Status != "fingerprint_computed" || hash.Bytes != "3" || hash.SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
			t.Fatalf("invalid hash at %d: %+v", n, hash)
		}
	}
}

func TestCoreVerifyLargeFileAcrossBuffers(t *testing.T) {
	m := fixtureManager(t)
	body := strings.Repeat("first-buffer-", 100000) + strings.Repeat("different-tail\x00", 90000) + "end"
	put(t, filepath.Join(m.root, "models", "955717e8-large.th"), body)
	inv := scanFixture(t, m)
	files := mustFiles(t, m, inv.ID, "demucs")
	if len(files) != 1 {
		t.Fatal(files)
	}
	reads := 0
	m.beforeRead = func(context.Context) { reads++ }
	o, err := m.StartVerify(context.Background(), inv.ID, "demucs", []string{files[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	done := await(t, m, o.ID)
	bytes := strconv.Itoa(len(body))
	sum := sha256.Sum256([]byte(body))
	digest := hex.EncodeToString(sum[:])
	if done.Status != "completed" || done.CheckedEntries != 1 || done.ProcessedBytes != bytes || len(done.Hashes) != 1 {
		t.Fatalf("大文件操作记录不完整: %+v", done)
	}
	h := done.Hashes[0]
	if h.FileID != files[0].ID || h.Status != "fingerprint_computed" || h.SHA256 != digest || h.Bytes != bytes || h.CheckedAt.IsZero() {
		t.Fatalf("跨 buffer 摘要或完整字节数错误: %+v", h)
	}
	if reads < 3 {
		t.Fatalf("未跨多个 1 MiB buffer: reads=%d", reads)
	}
	stored := mustFiles(t, m, inv.ID, "demucs")[0]
	if stored.Hash == nil || *stored.Hash != h || stored.Bytes != bytes {
		t.Fatalf("盘点未保存完整摘要记录: %+v", stored)
	}
}

func TestCoreVerifyBadFileDoesNotStopGoodFile(t *testing.T) {
	for _, mutation := range []string{"deleted", "changed"} {
		t.Run(mutation, func(t *testing.T) {
			m := fixtureManager(t)
			badPath := filepath.Join(m.root, "models", "955717e8-bad.th")
			goodPath := filepath.Join(m.root, "models", "955717e8-good.th")
			put(t, badPath, "bad")
			put(t, goodPath, "abc")
			inv := scanFixture(t, m)
			files := mustFiles(t, m, inv.ID, "demucs")
			var badID, goodID string
			for _, f := range files {
				switch filepath.Base(f.Path) {
				case filepath.Base(badPath):
					badID = f.ID
				case filepath.Base(goodPath):
					goodID = f.ID
				}
			}
			if len(files) != 2 || badID == "" || goodID == "" {
				t.Fatal(files)
			}
			if mutation == "deleted" {
				if err := os.Remove(badPath); err != nil {
					t.Fatal(err)
				}
			} else {
				put(t, badPath, "changed before StartVerify")
			}
			// The failing file is intentionally first: verification must continue.
			o, err := m.StartVerify(context.Background(), inv.ID, "demucs", []string{badID, goodID})
			if err != nil {
				t.Fatal(err)
			}
			done := await(t, m, o.ID)
			if done.Status != "failed" || done.CheckedEntries != 2 || done.ProcessedBytes != "3" || len(done.Hashes) != 2 {
				t.Fatalf("坏文件阻止后续文件或记账错误: %+v", done)
			}
			bad, good := done.Hashes[0], done.Hashes[1]
			if bad.FileID != badID || bad.Status != "failed" || bad.SHA256 != "" || bad.Bytes != "0" {
				t.Fatalf("启动前变化的文件发布摘要: %+v", bad)
			}
			if good.FileID != goodID || good.Status != "fingerprint_computed" || good.Bytes != "3" || good.SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
				t.Fatalf("好文件未继续计算: %+v", good)
			}
			for _, f := range mustFiles(t, m, inv.ID, "demucs") {
				if f.ID == badID && f.Hash != nil || f.ID == goodID && (f.Hash == nil || *f.Hash != good) {
					t.Fatalf("盘点发布结果错误: %+v", f)
				}
			}
		})
	}
}

func TestCoreVerifyMutationAfterFirstBuffer(t *testing.T) {
	const bufferSize = 1024 * 1024
	for _, mutation := range []string{"truncate", "same_size_rewrite", "replace", "metadata_before_eof"} {
		t.Run(mutation, func(t *testing.T) {
			m := fixtureManager(t)
			p := filepath.Join(m.root, "models", "955717e8-active.th")
			size := 2*bufferSize + 37
			if mutation == "metadata_before_eof" {
				// Exactly one buffer: the second Read will return EOF. This
				// deliberately tests the pre-EOF gate, not afterRead.
				size = bufferSize
			}
			put(t, p, strings.Repeat("a", size))
			inv := scanFixture(t, m)
			files := mustFiles(t, m, inv.ID, "demucs")
			if len(files) != 1 {
				t.Fatal(files)
			}
			before, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			entered, release := coreVerifyGate(t, m, 2)
			o, err := m.StartVerify(context.Background(), inv.ID, "demucs", []string{files[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			coreVerifySignal(t, entered)
			wantBytes := strconv.Itoa(size)
			wantMessage := "file_changed: 读取期间文件发生变化"
			switch mutation {
			case "truncate":
				if err := os.Truncate(p, bufferSize); err != nil {
					t.Fatal(err)
				}
				wantBytes = strconv.Itoa(bufferSize)
			case "same_size_rewrite":
				f, err := os.OpenFile(p, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				// Rewrite already-read bytes without changing the length.
				n, writeErr := f.WriteAt([]byte("changed"), 17)
				closeErr := f.Close()
				if writeErr != nil || n != len("changed") || closeErr != nil {
					t.Fatalf("修改已读区失败: n=%d write=%v close=%v", n, writeErr, closeErr)
				}
				fallthrough
			case "metadata_before_eof":
				// Force a distinct timestamp; do not rely on filesystem clock resolution.
				modified := before.ModTime().Add(2 * time.Hour)
				if err := os.Chtimes(p, modified, modified); err != nil {
					t.Fatal(err)
				}
			case "replace":
				replacement := filepath.Join(m.root, "models", "replacement.tmp")
				put(t, replacement, strings.Repeat("b", size))
				// Preserve size and mtime, isolating the final path identity check.
				if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, p); err != nil {
					// Windows may deny replacing an open handle without delete sharing.
					if runtime.GOOS == "windows" && (errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.Errno(32))) {
						t.Skipf("平台句柄策略拒绝替换，未覆盖读取中替换分支: %v", err)
					}
					t.Fatal(err)
				}
				wantMessage = "file_changed: 读取后路径身份或元数据已变化"
			}
			release()
			done := await(t, m, o.ID)
			if done.Status != "failed" || done.CheckedEntries != 1 || done.ProcessedBytes != wantBytes || len(done.Hashes) != 1 {
				t.Fatalf("读取中变化未失败或已读字节丢失: %+v", done)
			}
			h := done.Hashes[0]
			if h.FileID != files[0].ID || h.Status != "failed" || h.SHA256 != "" || h.Bytes != wantBytes || !strings.HasPrefix(h.Message, wantMessage) {
				t.Fatalf("未执行读取后复核或发布有效摘要: %+v", h)
			}
			if f := mustFiles(t, m, inv.ID, "demucs")[0]; f.Hash != nil || f.ContentEqual != "" {
				t.Fatalf("盘点保留无效指纹: %+v", f)
			}
		})
	}
}

func TestCoreVerifyMutationAndCancelAfterEOF(t *testing.T) {
	const bufferSize = 1024 * 1024
	body := strings.Repeat("a", 2*bufferSize+37)
	wantBytes := strconv.Itoa(len(body))
	for _, mutation := range []string{"same_size_rewrite", "truncate", "cancel", "replace"} {
		t.Run(mutation, func(t *testing.T) {
			m := fixtureManager(t)
			p := filepath.Join(m.root, "models", "955717e8-active.th")
			put(t, p, body)
			put(t, filepath.Join(m.root, "models", "955717e8-peer.th"), body)
			inv := scanFixture(t, m)
			files := mustFiles(t, m, inv.ID, "demucs")
			if len(files) != 2 || files[0].Path != p {
				t.Fatal(files)
			}
			// Establish a real content relation first, so clearing it is not vacuous.
			seed, err := m.StartVerify(context.Background(), inv.ID, "demucs", []string{files[0].ID, files[1].ID})
			if err != nil {
				t.Fatal(err)
			}
			seedDone := await(t, m, seed.ID)
			seeded := mustFiles(t, m, inv.ID, "demucs")
			if seedDone.Status != "completed" || seeded[0].ContentEqual == "" || seeded[0].ContentEqual != seeded[1].ContentEqual {
				t.Fatalf("未建立初始内容关系: %+v %+v", seedDone, seeded)
			}
			before, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			reads := 0
			m.beforeRead = func(context.Context) { reads++ }
			m.afterRead = func(context.Context) {
				close(entered)
				// Cancellation must not release this gate: only the main test may
				// allow post-EOF stat/identity checks and terminal publication.
				<-release
			}
			o, err := m.StartVerify(context.Background(), inv.ID, "demucs", []string{files[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			coreVerifySignal(t, entered)
			if reads < 4 {
				t.Fatalf("未完成全部数据及真实 EOF 读取: reads=%d", reads)
			}
			wantStatus := "failed"
			wantMessage := "file_changed: 读取期间文件发生变化"
			switch mutation {
			case "same_size_rewrite":
				f, err := os.OpenFile(p, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				n, writeErr := f.WriteAt([]byte("changed"), 17)
				closeErr := f.Close()
				if writeErr != nil || n != len("changed") || closeErr != nil {
					t.Fatalf("EOF 后同大小改写失败: n=%d write=%v close=%v", n, writeErr, closeErr)
				}
				modified := before.ModTime().Add(2 * time.Hour)
				if err := os.Chtimes(p, modified, modified); err != nil {
					t.Fatal(err)
				}
				after, err := os.Stat(p)
				if err != nil {
					t.Fatal(err)
				}
				if after.Size() != before.Size() || !after.ModTime().Equal(modified) || after.ModTime().Equal(before.ModTime()) {
					t.Fatalf("未落实同大小及明确 mtime 变化: before=%v/%d after=%v/%d", before.ModTime(), before.Size(), after.ModTime(), after.Size())
				}
			case "truncate":
				if err := os.Truncate(p, 37); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancelling, err := m.Cancel(o.ID)
				if err != nil || cancelling.Status != "cancelling" || cancelling.FinishedAt != nil {
					t.Fatalf("EOF 后取消未保持阻塞: %+v %v", cancelling, err)
				}
				wantStatus = "cancelled"
				wantMessage = context.Canceled.Error()
			case "replace":
				replacement := filepath.Join(m.root, "models", "replacement.tmp")
				put(t, replacement, strings.Repeat("b", len(body)))
				if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, p); err != nil {
					if runtime.GOOS == "windows" && (errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.Errno(32))) {
						t.Skipf("Windows 共享策略拒绝 EOF 后替换；仅跳过替换分支: %v", err)
					}
					t.Fatal(err)
				}
				wantMessage = "file_changed: 读取后路径身份或元数据已变化"
			}
			running, err := m.Operation(o.ID)
			if err != nil || running.Status != map[bool]string{false: "running", true: "cancelling"}[mutation == "cancel"] || running.FinishedAt != nil || len(running.Hashes) != 0 {
				t.Fatalf("EOF 关口释放前已完成或发布摘要: %+v %v", running, err)
			}
			unblock()
			done := await(t, m, o.ID)
			if done.Status != wantStatus || done.FinishedAt == nil || done.CheckedEntries != 1 || done.ProcessedBytes != wantBytes || len(done.Hashes) != 1 {
				t.Fatalf("EOF 后变化/取消状态或全部已读字节错误: %+v", done)
			}
			h := done.Hashes[0]
			if h.FileID != files[0].ID || h.Status != wantStatus || h.SHA256 != "" || h.Bytes != wantBytes || h.CheckedAt.IsZero() || !strings.HasPrefix(h.Message, wantMessage) {
				t.Fatalf("EOF 后发布有效指纹或丢失全部已读字节: %+v", h)
			}
			for _, f := range mustFiles(t, m, inv.ID, "demucs") {
				if f.ContentEqual != "" || f.ID == files[0].ID && f.Hash != nil {
					t.Fatalf("EOF 后保留无效摘要/内容关系: %+v", f)
				}
				if f.ID == files[1].ID && (f.Hash == nil || f.Hash.Status != "fingerprint_computed" || f.Hash.Bytes != wantBytes) {
					t.Fatalf("未参与的有效副本摘要被破坏: %+v", f)
				}
			}
		})
	}
}

func TestCoreVerifyTimeoutAfterFirstBuffer(t *testing.T) {
	m := fixtureManager(t)
	put(t, filepath.Join(m.root, "models", "955717e8-timeout.th"), strings.Repeat("x", 2*1024*1024))
	inv := scanFixture(t, m)
	files := mustFiles(t, m, inv.ID, "demucs")
	if len(files) != 1 {
		t.Fatal(files)
	}
	m.verifyTimeout = 2 * time.Second
	entered := make(chan struct{})
	cause := make(chan error, 1)
	calls := 0
	m.beforeRead = func(ctx context.Context) {
		calls++
		if calls == 2 {
			close(entered)
			<-ctx.Done()
			cause <- ctx.Err()
		}
	}
	o, err := m.StartVerify(context.Background(), inv.ID, "demucs", []string{files[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	coreVerifySignal(t, entered)
	done := await(t, m, o.ID)
	if err := <-cause; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("并非 deadline 导致结束: %v", err)
	}
	// finish treats deadline as failed, while fingerprint labels its interrupted
	// file cancelled. Neither should be weakened to an arbitrary non-success.
	if done.Status != "failed" || done.CheckedEntries != 1 || done.ProcessedBytes != "1048576" || len(done.Hashes) != 1 {
		t.Fatalf("超时操作状态或字节数错误: %+v", done)
	}
	h := done.Hashes[0]
	if h.Status != "cancelled" || h.SHA256 != "" || h.Bytes != "1048576" || h.FileID != files[0].ID || !strings.Contains(h.Message, context.DeadlineExceeded.Error()) {
		t.Fatalf("超时文件记录错误: %+v", h)
	}
	found := false
	for _, d := range done.Diagnostics {
		found = found || d.Code == "verify_timeout"
	}
	if !found {
		t.Fatal("缺少 verify_timeout 诊断", done.Diagnostics)
	}
	if f := mustFiles(t, m, inv.ID, "demucs")[0]; f.Hash != nil {
		t.Fatalf("超时后发布指纹: %+v", f)
	}
}

func TestCoreCloseWaitsForActiveWorker(t *testing.T) {
	for _, kind := range []string{"scan", "verify"} {
		t.Run(kind, func(t *testing.T) {
			m := fixtureManager(t)
			put(t, filepath.Join(m.root, "models", "955717e8-close.th"), strings.Repeat("c", 2*1024*1024))
			var inv Inventory
			var files []ModelFile
			if kind == "verify" {
				inv = scanFixture(t, m)
				files = mustFiles(t, m, inv.ID, "demucs")
				if len(files) != 1 {
					t.Fatal(files)
				}
			}
			entered := make(chan struct{})
			cancelled := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			hook := func(ctx context.Context) {
				close(entered)
				<-ctx.Done()
				close(cancelled)
				// Keep the worker alive after cancellation, proving Close waits
				// for termination rather than just delivering cancellation.
				<-release
			}
			var o Operation
			var err error
			if kind == "scan" {
				m.beforeScan = hook
				o, err = m.StartScan(context.Background(), 0)
			} else {
				calls := 0
				m.beforeRead = func(ctx context.Context) {
					calls++
					if calls == 2 {
						hook(ctx)
					}
				}
				o, err = m.StartVerify(context.Background(), inv.ID, "demucs", []string{files[0].ID})
			}
			if err != nil {
				t.Fatal(err)
			}
			coreVerifySignal(t, entered)
			closed := make(chan error, 1)
			go func() { closed <- m.Close() }()
			coreVerifySignal(t, cancelled)
			select {
			case err := <-closed:
				t.Fatalf("worker 尚未退出，Close 已返回: %v", err)
			default:
			}
			running, err := m.Operation(o.ID)
			if err != nil || running.FinishedAt != nil {
				t.Fatalf("阻塞中的 worker 不应已结束: %+v %v", running, err)
			}
			unblock()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("worker 释放后 Close 未结束")
			}
			done, err := m.Operation(o.ID)
			if err != nil || done.Status != "cancelled" || done.FinishedAt == nil {
				t.Fatalf("Close 未等待 cancelled 终态: %+v %v", done, err)
			}
			if kind == "verify" {
				if done.CheckedEntries != 1 || done.ProcessedBytes != "1048576" || len(done.Hashes) != 1 || done.Hashes[0].Status != "cancelled" || done.Hashes[0].SHA256 != "" || done.Hashes[0].Bytes != "1048576" {
					t.Fatalf("Close 后校验结果错误: %+v", done)
				}
				if f := mustFiles(t, m, inv.ID, "demucs")[0]; f.Hash != nil {
					t.Fatalf("Close 后保留摘要: %+v", f)
				}
			} else {
				result, err := m.Inventory(done.ResultID)
				if err != nil || result.Complete {
					t.Fatalf("Close 后扫描被标为完整: %+v %v", result, err)
				}
			}
			if state := m.State(); state.ActiveOperation != nil || state.LastAttempt == nil || state.LastAttempt.Status != "cancelled" {
				t.Fatalf("关闭后活动状态未清理: %+v", state)
			}
			for n := 0; n < 2; n++ {
				if err := m.Close(); err != nil {
					t.Fatalf("重复 Close 失败: %v", err)
				}
			}
			if _, err := m.StartScan(context.Background(), 0); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("Close 后仍可启动 worker: %v", err)
			}
		})
	}
}

func TestCoreOperationCancellationAndRequestLifetime(t *testing.T) {
	m := fixtureManager(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	m.beforeScan = func(ctx context.Context) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	request, cancel := context.WithCancel(context.Background())
	o, e := m.StartScan(request, 0)
	if e != nil {
		t.Fatal(e)
	}
	<-entered
	cancel()
	if _, e = m.StartScan(context.Background(), 0); e == nil {
		t.Fatal("未限制单操作")
	}
	running, _ := m.Operation(o.ID)
	if running.Status != "running" {
		t.Fatal("请求结束取消了后台任务")
	}
	m.Cancel(o.ID)
	done := await(t, m, o.ID)
	if done.Status != "cancelled" {
		t.Fatal(done.Status)
	}
	again, _ := m.Cancel(o.ID)
	if again.Status != "cancelled" {
		t.Fatal("终态复活")
	}
}
func TestCoreVerifyMutationAndCancel(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "mutation", true: "cancel"}[cancelled], func(t *testing.T) {
			m := fixtureManager(t)
			p := filepath.Join(m.root, "models", "955717e8-a.th")
			put(t, p, "abc")
			inv := scanFixture(t, m)
			files, _, _ := m.Files(inv.ID, "demucs", "", 200)
			entered := make(chan struct{})
			release := make(chan struct{})
			once := false
			m.beforeRead = func(ctx context.Context) {
				if once {
					return
				}
				once = true
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			o, e := m.StartVerify(context.Background(), inv.ID, "demucs", []string{files[0].ID})
			if e != nil {
				t.Fatal(e)
			}
			<-entered
			if cancelled {
				m.Cancel(o.ID)
			} else {
				put(t, p, "changed")
				close(release)
			}
			done := await(t, m, o.ID)
			if len(done.Hashes) != 1 || done.Hashes[0].SHA256 != "" || done.Hashes[0].Status == "fingerprint_computed" {
				t.Fatalf("发布无效指纹 %+v", done)
			}
		})
	}
}
func TestCoreContentEqualOnlyCurrentOrdinaryCopies(t *testing.T) {
	m := fixtureManager(t)
	for name, body := range map[string]string{"a": "abc", "b": "abc", "c": "xyz"} {
		put(t, filepath.Join(m.root, "models", "955717e8-"+name+".th"), body)
	}
	inv := scanFixture(t, m)
	files, _, _ := m.Files(inv.ID, "demucs", "", 200)
	for _, f := range files {
		if f.ContentEqual != "" || !f.DuplicateCandidate {
			t.Fatal("未校验文件不应内容相同", f)
		}
	}
	verify := func(ids []string) {
		t.Helper()
		o, err := m.StartVerify(context.Background(), inv.ID, "demucs", ids)
		if err != nil {
			t.Fatal(err)
		}
		if done := await(t, m, o.ID); done.Status != "completed" {
			t.Fatal(done)
		}
	}
	verify([]string{files[0].ID})
	first, _, _ := m.Files(inv.ID, "demucs", "", 200)
	if first[0].ContentEqual != "" {
		t.Fatal("单一有效摘要不能形成关系")
	}
	verify([]string{files[1].ID, files[2].ID})
	files, _, _ = m.Files(inv.ID, "demucs", "", 200)
	if files[0].ContentEqual == "" || files[0].ContentEqual != files[1].ContentEqual || files[2].ContentEqual != "" {
		t.Fatal("相同摘要副本关系不正确", files)
	}
	fresh, _ := m.Inventory(inv.ID)
	if *fresh.WeightSize.Bytes != "9" || *fresh.AssetSize.Bytes != "9" {
		t.Fatal("相同内容普通副本被扣容量")
	}
	inv = scanFixture(t, m)
	files, _, _ = m.Files(inv.ID, "demucs", "", 200)
	if files[0].ContentEqual == "" || files[0].ContentEqual != files[1].ContentEqual {
		t.Fatal("未变化摘要未继承关系")
	}
	oldID := inv.ID
	put(t, files[1].Path, "changed")
	inv = scanFixture(t, m)
	for _, id := range []string{oldID, inv.ID} {
		fs, _, err := m.Files(id, "demucs", "", 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fs {
			if f.ContentEqual != "" {
				t.Fatal("历史失效摘要保留内容关系", f)
			}
		}
	}
}

func TestCoreContentEqualCancellationClearsRelation(t *testing.T) {
	m := fixtureManager(t)
	for _, name := range []string{"a", "b"} {
		put(t, filepath.Join(m.root, "models", "955717e8-"+name+".th"), "abc")
	}
	inv := scanFixture(t, m)
	files, _, _ := m.Files(inv.ID, "demucs", "", 200)
	o, _ := m.StartVerify(context.Background(), inv.ID, "demucs", []string{files[0].ID, files[1].ID})
	await(t, m, o.ID)
	entered := make(chan struct{})
	m.beforeRead = func(ctx context.Context) { close(entered); <-ctx.Done() }
	o, err := m.StartVerify(context.Background(), inv.ID, "demucs", []string{files[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	for _, f := range mustFiles(t, m, inv.ID, "demucs") {
		if f.ContentEqual != "" {
			t.Error("重新校验期间保留旧内容关系")
		}
	}
	m.Cancel(o.ID)
	await(t, m, o.ID)
	for _, f := range mustFiles(t, m, inv.ID, "demucs") {
		if f.ContentEqual != "" {
			t.Fatal("取消后保留内容相同关系")
		}
	}
}

func mustFiles(t *testing.T, m *Manager, inventory, model string) []ModelFile {
	t.Helper()
	files, _, err := m.Files(inventory, model, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestCoreContentEqualHardlinksAreAliases(t *testing.T) {
	m := fixtureManager(t)
	p := filepath.Join(m.root, "models", "955717e8-a.th")
	put(t, p, "abc")
	if err := os.Link(p, filepath.Join(m.root, "models", "955717e8-b.th")); err != nil {
		t.Skipf("平台不允许硬链接: %v", err)
	}
	inv := scanFixture(t, m)
	files := mustFiles(t, m, inv.ID, "demucs")
	if len(files) != 1 || len(files[0].Aliases) != 1 {
		t.Fatal(files)
	}
	o, err := m.StartVerify(context.Background(), inv.ID, "demucs", []string{files[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	await(t, m, o.ID)
	if mustFiles(t, m, inv.ID, "demucs")[0].ContentEqual != "" {
		t.Fatal("别名不应成为普通副本关系")
	}
}
