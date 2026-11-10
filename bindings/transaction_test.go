package bindings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/integration"
)

func TestCorruptionFailsOpenWithoutErasingOrHoldingLock(t *testing.T) {
	valid := put(t, openStore(t, t.TempDir()), 0, fixture(t, "highlight.score"))
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		`{`, `null`, `[]`, `{}`, `{} {}`, `{"schema_version":1,"revision":0,"bindings":null}`,
		`{"schema_version":1,"revision":null,"bindings":{}}`,
		`{"schema_version":2,"revision":0,"bindings":{}}`,
		`{"schema_version":1,"schema_version":1,"revision":0,"bindings":{}}`,
		`{"schema_version":1,"revision":0,"bindings":{},"error":"hidden"}`,
		strings.Repeat(" ", MaxBytes+1),
		strings.Replace(string(raw), `"device":"cpu"`, `"device":"cpu","device":"auto"`, 1),
		strings.Replace(string(raw), `"device":"cpu"`, `"device":null`, 1),
		strings.Replace(string(raw), `"bytes":"17"`, `"bytes":17`, 1),
		strings.Replace(string(raw), `"bytes":"17"`, `"bytes":"18"`, 1),
		strings.Replace(string(raw), `"identity":"identity-0"`, `"identity":"identity-0","extra":1`, 1),
		strings.Replace(string(raw), `"registry_revision":3`, `"registry_revision":null`, 1),
		strings.Replace(string(raw), `"registry_revision":3,`, ``, 1),
		strings.Replace(string(raw), `"revision":1`, `"revision":0`, 1),
		strings.Replace(string(raw), `"bindings":{"highlight.score":`, `"bindings":{"image.face":`, 1),
	}
	for i, text := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			d := t.TempDir()
			p := filepath.Join(d, "bindings.json")
			if err := os.WriteFile(p, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				s, err := Open(d)
				if !errors.Is(err, ErrCorrupt) || s != nil {
					if s != nil {
						s.Close()
					}
					t.Fatalf("corrupt document accepted or lock leaked: %v", err)
				}
			}
			got, err := os.ReadFile(p)
			if err != nil || string(got) != text {
				t.Fatal("corrupt data overwritten", err)
			}
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			openStore(t, d)
		})
	}
}

func TestExternalChangeFreezesLastTrustedSnapshot(t *testing.T) {
	for _, stage := range []string{"created", "rewritten", "removed", "during-write", "same-bytes-replaced", "snapshot"} {
		t.Run(stage, func(t *testing.T) {
			d := t.TempDir()
			s := openStore(t, d)
			b := fixture(t, "highlight.score")
			if stage != "created" {
				put(t, s, 0, b)
			}
			before := s.Snapshot()
			p := filepath.Join(d, "bindings.json")
			change := func() {
				if stage == "removed" {
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
				} else if stage == "same-bytes-replaced" {
					raw, err := os.ReadFile(p)
					if err != nil {
						t.Fatal(err)
					}
					temp := filepath.Join(d, "replacement")
					if err := os.WriteFile(temp, raw, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(p); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(temp, p); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(p, []byte("external-change"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "during-write" {
				original := s.ops.sync
				s.ops.sync = func(f *os.File) error { change(); return original(f) }
			} else {
				change()
			}
			if stage == "snapshot" && !strings.HasPrefix(s.Snapshot().Error, "bindings_external_change") {
				t.Fatal("snapshot concealed external modification")
			}
			if _, err := s.Put(context.Background(), before.Revision, b); !errors.Is(err, ErrReadOnly) {
				t.Fatal(err)
			}
			after := s.Snapshot()
			if after.Error == "" || after.Revision != before.Revision || !reflect.DeepEqual(after.Bindings, before.Bindings) {
				t.Fatal("external state published or trusted state lost", after)
			}
			if _, err := s.Delete(context.Background(), before.Revision, b.Scene); !errors.Is(err, ErrReadOnly) {
				t.Fatal(err)
			}
		})
	}
}

func TestTransactionsCancellationAndFaults(t *testing.T) {
	for _, stage := range []string{"cancel-before", "cancel-during-sync", "write", "short-write", "sync", "close", "replace", "uncertain", "verify"} {
		t.Run(stage, func(t *testing.T) {
			d := t.TempDir()
			s := openStore(t, d)
			b := fixture(t, "highlight.score")
			before := put(t, s, 0, b)
			old, err := os.ReadFile(s.path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			boom := errors.New("injected")
			switch stage {
			case "cancel-before":
				cancel()
			case "cancel-during-sync":
				s.ops.sync = func(f *os.File) error { cancel(); return f.Sync() }
			case "write":
				s.ops.write = func(*os.File, []byte) error { return boom }
			case "short-write":
				s.ops.write = func(f *os.File, b []byte) error { _, e := f.Write(b[:len(b)/2]); return e }
			case "sync":
				s.ops.sync = func(*os.File) error { return boom }
			case "close":
				s.ops.close = func(f *os.File) error { f.Close(); return boom }
			case "replace":
				s.ops.replace = func(string, string) error { return boom }
			case "uncertain":
				s.ops.replace = func(a, b string) error {
					if e := defaultOps().replace(a, b); e != nil {
						return e
					}
					return boom
				}
			case "verify":
				s.ops.replace = func(a, b string) error {
					if e := defaultOps().replace(a, b); e != nil {
						return e
					}
					return os.WriteFile(b, []byte("unconfirmed"), 0600)
				}
			}
			b.Files[0].Identity = "new-identity"
			_, err = s.Put(ctx, 1, b)
			if err == nil {
				t.Fatal("failed transaction accepted")
			}
			if strings.HasPrefix(stage, "cancel") && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			after := s.Snapshot()
			if after.Revision != 1 || !reflect.DeepEqual(after.Bindings, before.Bindings) {
				t.Fatal("failed transaction published", after)
			}
			raw, err := os.ReadFile(s.path)
			if err != nil {
				t.Fatal(err)
			}
			uncertain := stage == "uncertain" || stage == "verify" || stage == "replace"
			if stage != "uncertain" && stage != "verify" && !bytes.Equal(raw, old) {
				t.Fatal("old document lost")
			}
			if uncertain && after.Error == "" {
				t.Fatal("uncertain commit not frozen")
			}
			files, err := filepath.Glob(filepath.Join(d, ".bindings-*"))
			if err != nil || len(files) != 0 {
				t.Fatal("temporary files leaked", files, err)
			}
			if !uncertain {
				s.ops = defaultOps()
				put(t, s, 1, b)
			} else if _, err := s.Put(context.Background(), 1, b); !errors.Is(err, ErrReadOnly) {
				t.Fatal(err)
			}
		})
	}
}

func TestConcurrentRevisionAndSceneRetry(t *testing.T) {
	s := openStore(t, t.TempDir())
	b := fixture(t, "highlight.score")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.Put(context.Background(), 0, b); errs <- err }()
	}
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 || s.Snapshot().Revision != 1 {
		t.Fatal(success)
	}
	for _, definition := range integration.RuntimeScenes() {
		binding := fixture(t, definition.ID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				revision := s.Snapshot().Revision
				_, err := s.Put(context.Background(), revision, binding)
				if err == nil {
					return
				}
				if !errors.Is(err, ErrConflict) {
					t.Error(err)
					return
				}
			}
			t.Error("CAS retries exhausted")
		}()
	}
	wg.Wait()
	if s.Snapshot().Revision != 7 || len(s.Snapshot().Bindings) != 6 {
		t.Fatal(s.Snapshot())
	}
}

func TestScopeIdentityIsFrozenAndValidated(t *testing.T) {
	b := fixture(t, "highlight.score")
	b.Files[0].ScopeIdentity = "scope-before"
	s := openStore(t, t.TempDir())
	a := put(t, s, 0, b).Bindings[b.Scene]
	b.Files[0].ScopeIdentity = "scope-after"
	changed := put(t, s, 1, b).Bindings[b.Scene]
	if a.Ref.SHA256 == changed.Ref.SHA256 {
		t.Fatal("scope replacement did not affect binding hash")
	}
	b.Files[0].ScopeIdentity = ""
	if _, err := s.Put(context.Background(), 2, b); err == nil {
		t.Fatal("missing scope identity accepted")
	}
}

func TestCancelAfterCommitPublishesConfirmedState(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ops.replace = func(a, b string) error {
		if err := defaultOps().replace(a, b); err != nil {
			return err
		}
		cancel()
		return nil
	}
	next, err := s.Put(ctx, 0, fixture(t, "highlight.score"))
	if err != nil || next.Revision != 1 || s.Snapshot().Revision != 1 {
		t.Fatal("cancellation after commit hid durable state", next, err)
	}
}

func TestDocumentCapacity(t *testing.T) {
	s := openStore(t, t.TempDir())
	b := fixture(t, "highlight.score")
	for i := 1; i < MaxFiles; i++ {
		f := b.Files[0]
		f.FileID, f.Identity, f.RelativePath = fmt.Sprint(i), fmt.Sprint(i), fmt.Sprint(i)
		b.Files = append(b.Files, f)
	}
	base := put(t, s, 0, b)
	// All individual fields and the file count remain valid, but the whole
	// document exceeds MaxBytes. Rejection must leave disk and memory intact.
	old, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range b.Files {
		b.Files[i].Group = strings.Repeat("x", 2048)
	}
	if _, err := s.Put(context.Background(), 1, b); err == nil {
		t.Fatal("document capacity ignored")
	}
	raw, err := os.ReadFile(s.path)
	if err != nil || !bytes.Equal(raw, old) || !reflect.DeepEqual(s.Snapshot(), base) {
		t.Fatal("capacity failure was not atomic", err)
	}
}

func TestRejectInvalidUTF8OnDisk(t *testing.T) {
	d := t.TempDir()
	s := openStore(t, d)
	b := fixture(t, "highlight.score")
	b.Files[0].Group = "replacement-\ufffd"
	put(t, s, 0, b)
	s.Close()
	p := filepath.Join(d, "bindings.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte("replacement-\ufffd"), append([]byte("replacement-"), 0xff), 1)
	if err := os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(d)
	if opened != nil {
		opened.Close()
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatal("invalid UTF-8 was silently repaired", err)
	}
}

func TestLimitsAndRevisionOverflow(t *testing.T) {
	s := openStore(t, t.TempDir())
	b := fixture(t, "highlight.score")
	b.Files[0].Group = strings.Repeat("x", MaxBytes)
	if _, err := s.Put(context.Background(), 0, b); err == nil {
		t.Fatal("oversized content accepted")
	}
	b = fixture(t, "highlight.score")
	for i := 1; i <= MaxFiles; i++ {
		f := b.Files[0]
		f.FileID, f.Identity, f.RelativePath = fmt.Sprint(i), fmt.Sprint(i), fmt.Sprint(i)
		b.Files = append(b.Files, f)
	}
	if _, err := s.Put(context.Background(), 0, b); err == nil {
		t.Fatal("file limit ignored")
	}
	b = fixture(t, "highlight.score")
	s.state.Revision = ^uint64(0)
	if _, err := s.Put(context.Background(), ^uint64(0), b); err == nil || s.state.Revision != ^uint64(0) {
		t.Fatal("revision wrapped", err)
	}
}

func TestLifecycleLock(t *testing.T) {
	const env = "BINDINGS_TEST_LOCK_DIR"
	if dir := os.Getenv(env); dir != "" {
		s, err := Open(dir)
		if err != nil {
			os.Exit(23)
		}
		if os.Getenv("BINDINGS_TEST_ABRUPT") != "" {
			// Skip Close: the OS must release the lifetime lock after process death.
			os.Exit(42)
		}
		s.Close()
		os.Exit(0)
	}
	d := t.TempDir()
	s := openStore(t, d)
	if other, err := Open(d); err == nil {
		other.Close()
		t.Fatal("duplicate lifetime owner")
	}
	run := func(abrupt bool) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLifecycleLock$")
		cmd.Env = append(os.Environ(), env+"="+d)
		if abrupt {
			cmd.Env = append(cmd.Env, "BINDINGS_TEST_ABRUPT=1")
		}
		return cmd.Run()
	}
	if err := run(false); err == nil {
		t.Fatal("child acquired live lock")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), 0, fixture(t, "highlight.score")); !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
	if _, err := s.Delete(context.Background(), 0, "highlight.score"); !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
	if err := run(false); err != nil {
		t.Fatal("lock not released", err)
	}
	var exit *exec.ExitError
	if err := run(true); !errors.As(err, &exit) || exit.ExitCode() != 42 {
		t.Fatal("abrupt child", err)
	}
	openStore(t, d)
	if _, err := os.Stat(filepath.Join(d, "bindings.lock")); err != nil {
		t.Fatal(err)
	}
}
