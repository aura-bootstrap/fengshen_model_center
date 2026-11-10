//go:build windows

package runtimeprocess

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"
)

const helperFlag = "--runtimeprocess-helper"

func TestHelperProcess(t *testing.T) {
	for i, a := range os.Args {
		if a != helperFlag {
			continue
		}
		mode := os.Args[i+1]
		switch mode {
		case "environment-parent":
			checkMinimalEnvironment(t)
			if t.Failed() {
				os.Exit(97)
			}
		case "environment":
			if key := os.Getenv("INJECTED_TOKEN"); key != "" && strings.Contains(strings.Join(os.Args, "\x00"), key) {
				os.Exit(98)
			}
			for _, entry := range os.Environ() {
				fmt.Fprintln(os.Stdout, entry)
			}
		case "echo":
			fmt.Fprintln(os.Stderr, "diagnostic")
			_, _ = io.Copy(os.Stdout, os.Stdin)
		case "flood":
			_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 2<<20))
		case "marker":
			if err := os.WriteFile(os.Args[i+2], []byte("resumed"), 0600); err != nil {
				os.Exit(96)
			}
			time.Sleep(time.Minute)
		case "stderr-flood":
			_, _ = io.WriteString(os.Stderr, strings.Repeat("x", 2<<20))
		case "short-output":
			_, _ = io.WriteString(os.Stdout, "too much output")
		case "no-read":
			fmt.Fprintln(os.Stdout, "ready")
			time.Sleep(time.Minute)
		case "exit":
			os.Exit(7)
		case "leaf":
			fmt.Fprintln(os.Stdout, os.Getpid())
			time.Sleep(time.Minute)
		case "tree", "root-exit":
			c := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", helperFlag, "leaf")
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			if err := c.Start(); err != nil {
				os.Exit(91)
			}
			fmt.Fprintln(os.Stdout, os.Getpid())
			if mode == "root-exit" {
				var ack [1]byte
				_, _ = os.Stdin.Read(ack[:])
				os.Exit(0)
			}
			_ = c.Wait()
		case "parent":
			s := &Supervisor{}
			p, err := s.Start(context.Background(), testBinary(t), options("tree"))
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(92)
			}
			scan := bufio.NewScanner(p.Stdout())
			for j := 0; j < 2; j++ {
				if !scan.Scan() {
					os.Exit(93)
				}
				fmt.Fprintln(os.Stdout, scan.Text())
			}
			// Wait until the test holds both descendant process handles.
			var ack [1]byte
			_, _ = os.Stdin.Read(ack[:])
			// Neither Close nor deferred cleanup: emulate a center crash.
			os.Exit(23)
		case "hold-parent":
			s := &Supervisor{}
			p, err := s.Start(context.Background(), testBinary(t), options("tree"))
			if err != nil {
				os.Exit(94)
			}
			scan := bufio.NewScanner(p.Stdout())
			for j := 0; j < 2; j++ {
				if !scan.Scan() {
					os.Exit(95)
				}
				fmt.Fprintln(os.Stdout, scan.Text())
			}
			time.Sleep(time.Minute)
		default:
			os.Exit(90)
		}
		os.Exit(0)
	}
}

func TestMinimalChildEnvironment(t *testing.T) {
	c := exec.CommandContext(testContext(t), os.Args[0], "-test.run=TestHelperProcess", "--", helperFlag, "environment-parent")
	c.Env = append(os.Environ(), "HTTP_PROXY=http://injected.invalid", "Providersecret=secret", "CENTER_TOKEN=token", "INJECTED_TOKEN=injected", "INJECTED_MODEL=injected", "PATH=C:\\untrusted", "SystemRoot=C:\\untrusted", "TEMP=C:\\untrusted", "TMP=C:\\untrusted")
	data, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("environment helper: %v\n%s", err, data)
	}
}

func checkMinimalEnvironment(t *testing.T) {
	s := &Supervisor{}
	defer s.Close()
	b := testBinary(t)
	before := strings.Join(os.Environ(), "\x00")
	p, err := s.Start(testContext(t), b, options("environment"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	if err = waitProcess(t, p); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, entry := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name, value, found := strings.Cut(strings.TrimSuffix(entry, "\r"), "=")
		name = strings.ToUpper(name)
		if !found || seen[name] != "" {
			t.Fatalf("malformed or duplicate environment name %q", name)
		}
		seen[name] = value
		switch name {
		case "SYSTEMROOT", "WINDIR", "PATH", "TEMP", "TMP":
			if strings.Contains(strings.ToLower(value), "untrusted") || !filepath.IsAbs(strings.Split(value, ";")[0]) {
				t.Errorf("untrusted system path %q", name)
			}
		default:
			t.Errorf("inherited forbidden environment name %q", name)
		}
	}
	for _, name := range []string{"SYSTEMROOT", "WINDIR", "PATH", "TEMP", "TMP"} {
		if seen[name] == "" {
			t.Errorf("missing %s", name)
		}
	}
	if strings.Join(os.Environ(), "\x00") != before {
		t.Error("launch polluted parent environment")
	}
}

func TestExplicitUnicodeEnvironmentBlock(t *testing.T) {
	block, err := minimalEnvironment([]string{"AAA_INPUT=value"})
	if err != nil {
		t.Fatal(err)
	}
	if len(block) < 2 || block[len(block)-1] != 0 || block[len(block)-2] != 0 {
		t.Fatal("environment block lacks double NUL")
	}
	entries := strings.Split(string(utf16.Decode(block[:len(block)-2])), "\x00")
	previous := ""
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" || strings.ToUpper(name) <= previous {
			t.Fatal("malformed or unsorted environment block")
		}
		previous = strings.ToUpper(name)
	}
	if len(entries) != 6 {
		t.Fatal("unexpected environment block entries")
	}
}

func testBinary(t *testing.T) VerifiedBinary {
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
	b, err := VerifyBinary(path, hex.EncodeToString(sum[:]), "helper/v1", "binding/rev1")
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func options(mode string) Options {
	return Options{Args: []string{"-test.run=TestHelperProcess", "--", helperFlag, mode}, MaxStdinBytes: 1 << 20, MaxStdoutBytes: 1 << 20, MaxStderrBytes: 1 << 20, BufferBytes: 64 << 10}
}
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func waitProcess(t *testing.T, p *Process) error {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- p.Wait() }()
	select {
	case err := <-ch:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Wait blocked")
		return nil
	}
}
func readPIDs(t *testing.T, r io.Reader) []uint32 {
	t.Helper()
	out := make(chan []uint32, 1)
	go func() {
		s := bufio.NewScanner(r)
		var pids []uint32
		for len(pids) < 2 && s.Scan() {
			n, e := strconv.Atoi(s.Text())
			if e != nil {
				break
			}
			pids = append(pids, uint32(n))
		}
		out <- pids
	}()
	select {
	case pids := <-out:
		if len(pids) != 2 {
			t.Fatalf("expected child and grandchild, got %v", pids)
		}
		return pids
	case <-time.After(10 * time.Second):
		t.Fatal("no tree readiness")
		return nil
	}
}
func openWitness(t *testing.T, pid uint32) syscall.Handle {
	t.Helper()
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE|syscall.PROCESS_QUERY_INFORMATION, false, pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.CloseHandle(h) })
	return h
}
func assertDead(t *testing.T, h syscall.Handle) {
	t.Helper()
	status, err := syscall.WaitForSingleObject(h, 5000)
	if err != nil || status != syscall.WAIT_OBJECT_0 {
		t.Fatalf("process survived: status=%d err=%v", status, err)
	}
}

func TestStreamsAndIdentity(t *testing.T) {
	b := testBinary(t)
	s := &Supervisor{}
	t.Cleanup(func() { s.Close() })
	p, err := s.Start(testContext(t), b, options("echo"))
	if err != nil {
		t.Fatal(err)
	}
	id := p.Identity()
	if id.PID == 0 || id.CreationTime == 0 || id.ImagePath == "" || id.SHA256 == "" || id.Component != "helper/v1" || id.Binding != "binding/rev1" {
		t.Fatalf("incomplete identity: %+v", id)
	}
	if err = p.CheckIdentity(b); err != nil {
		t.Fatal(err)
	}
	wrong := b
	wrong.binding = "other"
	if p.CheckIdentity(wrong) == nil {
		t.Fatal("accepted different binding")
	}
	payload := "hello \"世界\"\n"
	if _, err = io.WriteString(p.Stdin(), payload); err != nil {
		t.Fatal(err)
	}
	p.Stdin().Close()
	out, err := io.ReadAll(p.Stdout())
	if err != nil || string(out) != payload {
		t.Fatalf("stdout %q: %v", out, err)
	}
	diag, err := io.ReadAll(p.Stderr())
	diag = bytes.ReplaceAll(diag, []byte("warning: GOCOVERDIR not set, no coverage data emitted\n"), nil)
	if err != nil || string(diag) != "diagnostic\n" {
		t.Fatalf("stderr %q: %v", diag, err)
	}
	if err = waitProcess(t, p); err != nil {
		t.Fatal(err)
	}
	if err = p.Wait(); err != nil {
		t.Fatal(err)
	}
	if p.CheckIdentity(b) == nil {
		t.Fatal("reused exited instance")
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelKillsOnlyOwnedTree(t *testing.T) {
	external := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", helperFlag, "leaf")
	if err := external.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { external.Process.Kill(); external.Wait() })
	ext := openWitness(t, uint32(external.Process.Pid))
	s := &Supervisor{}
	t.Cleanup(func() { s.Close() })
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	p, err := s.Start(ctx, testBinary(t), options("tree"))
	if err != nil {
		t.Fatal(err)
	}
	pids := readPIDs(t, p.Stdout())
	handles := []syscall.Handle{openWitness(t, pids[0]), openWitness(t, pids[1])}
	cancel()
	if err = waitProcess(t, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	for _, h := range handles {
		assertDead(t, h)
	}
	if status, err := syscall.WaitForSingleObject(ext, 0); err != nil || status != syscall.WAIT_TIMEOUT {
		t.Fatalf("unrelated external terminated: %d %v", status, err)
	}
}

func TestParentCrashKillsTree(t *testing.T) {
	external := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", helperFlag, "leaf")
	if err := external.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { external.Process.Kill(); external.Wait() })
	ext := openWitness(t, uint32(external.Process.Pid))
	defer func() {
		if status, err := syscall.WaitForSingleObject(ext, 0); err != nil || status != syscall.WAIT_TIMEOUT {
			t.Errorf("parent crash killed unrelated process: %d %v", status, err)
		}
	}()
	for _, mode := range []string{"parent", "hold-parent"} {
		t.Run(mode, func(t *testing.T) {
			c := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--", helperFlag, mode)
			out, err := c.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			c.Stderr = os.Stderr
			ack, err := c.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer ack.Close()
			if err = c.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Process.Kill() })
			pids := readPIDs(t, out)
			var hs []syscall.Handle
			for _, pid := range pids {
				hs = append(hs, openWitness(t, pid))
			}
			if mode == "parent" {
				if _, err = ack.Write([]byte{1}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "hold-parent" {
				if err = c.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			}
			if err = c.Wait(); err == nil {
				t.Fatal("expected abrupt parent exit")
			}
			for _, h := range hs {
				assertDead(t, h)
			}
		})
	}
}

func TestSuspendedStartupFailureKillsAndWaits(t *testing.T) {
	b := testBinary(t)
	marker := filepath.Join(t.TempDir(), "must-not-run")
	o := options("marker")
	o.Args = append(o.Args, marker)
	// Directly exercise the post-CreateProcess verification failure: the actual
	// suspended process image cannot match this identity. No production test hook.
	bad := b
	bad.file.IndexLow ^= 1
	children := func() map[uint32]bool {
		snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer syscall.CloseHandle(snapshot)
		entry := syscall.ProcessEntry32{Size: uint32(unsafe.Sizeof(syscall.ProcessEntry32{}))}
		result := map[uint32]bool{}
		for err = syscall.Process32First(snapshot, &entry); err == nil; err = syscall.Process32Next(snapshot, &entry) {
			if entry.ParentProcessID == uint32(os.Getpid()) {
				result[entry.ProcessID] = true
			}
		}
		if err != syscall.ERROR_NO_MORE_FILES {
			t.Fatal(err)
		}
		return result
	}
	before := children()
	for i := 0; i < 5; i++ {
		n, _, _, _, _, err := launch(testContext(t), bad, o)
		if err == nil {
			n.closeJob()
			n.wait()
			n.closeProcess()
			t.Fatal("accepted mismatched actual image")
		}
		if !errors.Is(err, ErrIdentity) {
			t.Fatalf("unexpected startup failure: %v", err)
		}
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed startup resumed code: %v", err)
	}
	for pid := range children() {
		if !before[pid] {
			t.Fatalf("startup failure left suspended/running child %d", pid)
		}
	}
}

func TestJobFlagsAndRootExit(t *testing.T) {
	s := &Supervisor{}
	defer s.Close()
	p, err := s.Start(testContext(t), testBinary(t), options("root-exit"))
	if err != nil {
		t.Fatal(err)
	}
	pids := readPIDs(t, p.Stdout())
	hs := []syscall.Handle{openWitness(t, pids[0]), openWitness(t, pids[1])}
	var flags uint32
	getInfo := kernel32.NewProc("GetHandleInformation")
	ok, _, e := getInfo.Call(uintptr(p.native.job), uintptr(unsafe.Pointer(&flags)))
	if ok == 0 {
		t.Fatal(e)
	}
	if flags&1 != 0 {
		t.Fatal("Job handle is inheritable")
	}
	var centerMember int32
	ok, _, e = inJob.Call(^uintptr(0), uintptr(p.native.job), uintptr(unsafe.Pointer(&centerMember)))
	if ok == 0 {
		t.Fatal(e)
	}
	if centerMember != 0 {
		t.Fatal("center assigned to model Job")
	}
	if _, err = p.Stdin().Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err = waitProcess(t, p); err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		assertDead(t, h)
	}
}

func TestBlockedStdinAndConcurrentShutdown(t *testing.T) {
	s := &Supervisor{}
	defer s.Close()
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	o := options("no-read")
	o.MaxStdinBytes = 4 << 20
	p, err := s.Start(ctx, testBinary(t), o)
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := bufio.NewReader(p.Stdout()).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("helper not ready: %q %v", ready, err)
	}
	writeDone := make(chan error, 1)
	go func() { _, err := p.Stdin().Write(make([]byte, 2<<20)); writeDone <- err }()
	// A real pipe write to a ready non-reader must remain blocked before cancel.
	select {
	case err := <-writeDone:
		t.Fatalf("write did not block: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = p.Close(); _ = p.Wait() }()
	}
	cancel()
	_ = s.Close()
	wg.Wait()
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("blocked writer unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked stdin survived shutdown")
	}
}

func TestShortAndStderrOutputLimits(t *testing.T) {
	for _, mode := range []string{"short-output", "stderr-flood"} {
		t.Run(mode, func(t *testing.T) {
			s := &Supervisor{}
			defer s.Close()
			o := options(mode)
			o.MaxStdoutBytes = 1
			o.MaxStderrBytes = 1
			p, err := s.Start(testContext(t), testBinary(t), o)
			if err != nil {
				t.Fatal(err)
			}
			if err = waitProcess(t, p); !errors.Is(err, ErrOutputLimit) {
				t.Fatalf("output limit hidden by exit: %v", err)
			}
		})
	}
}

func TestLimitsAndClose(t *testing.T) {
	for _, kind := range []string{"output", "buffer", "stdin", "close", "supervisor", "deadline", "exit"} {
		t.Run(kind, func(t *testing.T) {
			s := &Supervisor{}
			t.Cleanup(func() { s.Close() })
			mode := "tree"
			o := options(mode)
			if kind == "output" || kind == "buffer" {
				o = options("flood")
				o.MaxStdoutBytes = 32
				if kind == "buffer" {
					o.MaxStdoutBytes = 4 << 20
					o.BufferBytes = 32
				}
			}
			if kind == "stdin" {
				o = options("echo")
				o.MaxStdinBytes = 2
			}
			if kind == "exit" {
				o = options("exit")
			}
			ctx := testContext(t)
			if kind == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 150*time.Millisecond)
				defer cancel()
			}
			p, err := s.Start(ctx, testBinary(t), o)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "stdin":
				if _, err = io.WriteString(p.Stdin(), "abc"); !errors.Is(err, ErrInputLimit) {
					t.Fatalf("input limit: %v", err)
				}
			case "close":
				readPIDs(t, p.Stdout())
				if err = p.Close(); err != nil {
					t.Fatal(err)
				}
			case "supervisor":
				readPIDs(t, p.Stdout())
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err = s.Start(ctx, testBinary(t), o); !errors.Is(err, ErrClosed) {
					t.Fatalf("start after close: %v", err)
				}
			}
			err = waitProcess(t, p)
			switch kind {
			case "output", "buffer":
				if !errors.Is(err, ErrOutputLimit) {
					t.Fatalf("output limit: %v", err)
				}
			case "stdin":
				if !errors.Is(err, ErrInputLimit) {
					t.Fatalf("input limit wait: %v", err)
				}
			case "deadline":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deadline: %v", err)
				}
			default:
				if err == nil {
					t.Fatal("expected unsuccessful exit")
				}
			}
			if err = p.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRejectUnverifiedChangedBinaryAndBadOptions(t *testing.T) {
	s := &Supervisor{}
	defer s.Close()
	ctx := testContext(t)
	if _, err := s.Start(ctx, VerifiedBinary{}, options("echo")); err == nil {
		t.Fatal("accepted empty descriptor")
	}
	b := testBinary(t)
	for _, o := range []Options{{}, {Args: []string{"bad\x00arg"}, MaxStdinBytes: 1, MaxStdoutBytes: 1, MaxStderrBytes: 1, BufferBytes: 1}} {
		if p, err := s.Start(ctx, b, o); err == nil {
			p.Close()
			t.Fatal("accepted invalid options")
		}
	}
	path := filepath.Join(t.TempDir(), "helper.exe")
	data, err := os.ReadFile(b.path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	copyB, err := VerifyBinary(path, b.digest, "helper/v1", "binding/rev1")
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	// Restore mtime and preserve inode/length: Start must rehash, not merely stat.
	if err = os.Chtimes(path, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Start(ctx, copyB, options("echo")); err == nil {
		p.Close()
		t.Fatal("accepted modified executable")
	}
	if _, err = VerifyBinary(b.path, strings.Repeat("0", 64), "helper/v1", "binding/rev1"); err == nil {
		t.Fatal("accepted wrong hash")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if p, err := s.Start(canceled, b, options("tree")); !errors.Is(err, context.Canceled) {
		if p != nil {
			p.Close()
		}
		t.Fatalf("cancelled start: %v", err)
	}
}
