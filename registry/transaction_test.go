package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestFaultStages(t *testing.T) {
	for _, stage := range []string{"write", "sync", "close", "replace", "uncertain"} {
		t.Run(stage, func(t *testing.T) {
			d := t.TempDir()
			s, e := Open(d, validator)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			if _, e = s.Update(context.Background(), 0, "whisper-asr", "old"); e != nil {
				t.Fatal(e)
			}
			old, _ := os.ReadFile(s.path)
			boom := errors.New("injected")
			switch stage {
			case "write":
				s.ops.write = func(*os.File, []byte) error { return boom }
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
			}
			if _, e = s.Update(context.Background(), 1, "whisper-asr", "new"); e == nil {
				t.Fatal("fault accepted")
			}
			if s.Snapshot().Revision != 1 {
				t.Fatal("published failed transaction")
			}
			b, _ := os.ReadFile(s.path)
			if stage == "uncertain" {
				backup, _ := os.ReadFile(s.path + ".bak")
				if string(backup) != string(old) {
					t.Fatal("backup lost")
				}
			} else if string(b) != string(old) {
				t.Fatal("old data lost")
			}
			if stage == "replace" || stage == "uncertain" {
				if s.Snapshot().Error == "" {
					t.Fatal("not frozen")
				}
			}
		})
	}
}
func TestConcurrentRevision(t *testing.T) {
	s, e := Open(t.TempDir(), validator)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.Update(context.Background(), 0, "whisper-asr", "value")
			if e == nil {
				mu.Lock()
				success++
				mu.Unlock()
			} else if !errors.Is(e, ErrConflict) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if success != 1 || s.Snapshot().Revision != 1 {
		t.Fatal(success)
	}
}
func TestConcurrentIndependentModelsCASRetry(t *testing.T) {
	ids := []string{"whisper-asr", "whisper-vad", "local-text", "local-vision", "demucs", "demucs-ft", "seed-vc", "seed-whisper", "campplus", "bigvgan", "haar-frontal", "haar-profile"}
	// Reuse the fixture's registration fields for each supported model ID.
	validate := func(id, path string) (Registration, error) {
		if !knownID(id) {
			return Registration{}, fmt.Errorf("unknown model ID: %s", id)
		}
		return validator("whisper-asr", path)
	}
	d := t.TempDir()
	s, e := Open(d, validate)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		id        string
		conflicts int
		err       error
	}
	results := make(chan result, len(ids))
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(len(ids))
	for _, id := range ids {
		go func(id string) {
			r := result{id: id}
			expected := s.Snapshot().Revision
			ready.Done()
			<-start
			// Each other worker commits at most once, so len(ids) attempts suffice.
			for attempt := 0; attempt < len(ids); attempt++ {
				_, r.err = s.Update(ctx, expected, id, id+".bin")
				if !errors.Is(r.err, ErrConflict) {
					results <- r
					return
				}
				r.conflicts++
				expected = s.Snapshot().Revision
			}
			r.err = fmt.Errorf("CAS retries exhausted: %w", r.err)
			results <- r
		}(id)
	}
	// All workers first observe revision zero, guaranteeing actual conflicts.
	ready.Wait()
	close(start)
	conflicts := 0
	for range ids {
		r := <-results
		if r.err != nil {
			t.Errorf("register %s: %v", r.id, r.err)
		}
		conflicts += r.conflicts
	}
	if t.Failed() {
		return
	}
	if conflicts < len(ids)-1 {
		t.Fatalf("got %d conflicts, want at least %d", conflicts, len(ids)-1)
	}
	want := s.Snapshot()
	if want.SchemaVersion != 1 || want.Revision != uint64(len(ids)) || want.Error != "" || len(want.Registrations) != len(ids) {
		t.Fatalf("incomplete snapshot: %+v", want)
	}
	for _, id := range ids {
		got, ok := want.Registrations[id]
		fixture, err := validate(id, id+".bin")
		if err != nil {
			t.Fatal(err)
		}
		fixture.UpdatedAt = got.UpdatedAt
		if !ok || got.UpdatedAt.IsZero() || !reflect.DeepEqual(got, fixture) {
			t.Errorf("registration %s: got %+v, want %+v with a timestamp", id, got, fixture)
		}
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(d, "registrations.json"))
	if e != nil {
		t.Fatal(e)
	}
	var disk Snapshot
	if e := json.Unmarshal(b, &disk); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(disk, want) {
		t.Fatalf("persisted snapshot mismatch: got %+v, want %+v", disk, want)
	}
	reopened, e := Open(d, validate)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if got := reopened.Snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened snapshot mismatch: got %+v, want %+v", got, want)
	}
}

func TestLockSubprocessAbruptExit(t *testing.T) {
	const exitCode = 42
	if d := os.Getenv("REGISTRY_TEST_LOCK_ABRUPT_EXIT"); d != "" {
		s, e := Open(d, validator)
		if e != nil {
			os.Exit(23)
		}
		if _, e := s.Update(context.Background(), 1, "whisper-asr", "saved-by-child"); e != nil {
			os.Exit(24)
		}
		// Deliberately skip Close (and all defers) to exercise OS lock release.
		os.Exit(exitCode)
	}
	d := t.TempDir()
	s, e := Open(d, validator)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e := s.Update(context.Background(), 0, "whisper-asr", "saved-by-parent"); e != nil {
		t.Fatal(e)
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockSubprocessAbruptExit$")
	cmd.Env = append(os.Environ(), "REGISTRY_TEST_LOCK_ABRUPT_EXIT="+d)
	output, e := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if ctx.Err() != nil || !errors.As(e, &exitErr) || exitErr.ExitCode() != exitCode {
		t.Fatalf("child exit: got %v (context: %v), want code %d; output: %s", e, ctx.Err(), exitCode, output)
	}
	lockPath := filepath.Join(d, "registry.lock")
	if _, e := os.Stat(lockPath); e != nil {
		t.Fatalf("lock file missing after child exit: %v", e)
	}
	reopened, e := Open(d, validator)
	if e != nil {
		t.Fatalf("lock not released after abrupt child exit: %v", e)
	}
	defer reopened.Close()
	got := reopened.Snapshot()
	r, ok := got.Registrations["whisper-asr"]
	if got.SchemaVersion != 1 || got.Revision != 2 || got.Error != "" || len(got.Registrations) != 1 || !ok ||
		r.InputPath != "saved-by-child" || r.ResolvedPath != "saved-by-child" || r.Kind != "file" || r.Identity != "fixture" || r.UpdatedAt.IsZero() {
		t.Fatalf("registration lost after child exit: %+v", got)
	}
	if _, e := os.Stat(lockPath); e != nil {
		t.Fatalf("lock file removed on reopen: %v", e)
	}
}

func TestLockSubprocess(t *testing.T) {
	if d := os.Getenv("REGISTRY_TEST_LOCK"); d != "" {
		s, e := Open(d, validator)
		if e != nil {
			os.Exit(23)
		}
		defer s.Close()
		os.Exit(0)
	}
	d := t.TempDir()
	s, e := Open(d, validator)
	if e != nil {
		t.Fatal(e)
	}
	run := func() error {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLockSubprocess$")
		cmd.Env = append(os.Environ(), "REGISTRY_TEST_LOCK="+d)
		return cmd.Run()
	}
	if e = run(); e == nil {
		t.Fatal("child acquired live lock")
	}
	s.Close()
	if e = run(); e != nil {
		t.Fatal("lock not released", e)
	}
	if _, e = os.Stat(filepath.Join(d, "registry.lock")); e != nil {
		t.Fatal("lock file removed")
	}
}
func TestStrictFields(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{} {}`, `{"schema_version":1,"revision":null,"registrations":{}}`, `{"schema_version":1,"revision":0,"registrations":{},"extra":1}`} {
		if _, e := decode([]byte(raw), validator); e == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
