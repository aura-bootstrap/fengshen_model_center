package runtimejobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type failingExecutor struct {
	err        error
	panicValue any
}

func (e failingExecutor) Execute(context.Context, Request, Emit) (Result, error) {
	if e.panicValue != nil {
		panic(e.panicValue)
	}
	return Result{}, e.err
}
func (failingExecutor) Close() error { return nil }

func checkExecutorFailure(t *testing.T, executor failingExecutor, want string) {
	t.Helper()
	m, err := New(executor, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	s := submit(t, m, "r")
	got := waitTerminal(t, m, s.ID)
	if got.Code != want || got.State != Failed {
		t.Fatal(got)
	}
	events, cursor, err := m.Events("session", "task", s.ID, 0)
	if err != nil || len(events) == 0 || cursor != got.Cursor {
		t.Fatalf("events=%v cursor=%d error=%v", events, cursor, err)
	}
	terminalCount := 0
	for _, e := range events {
		if e.State.Terminal() {
			terminalCount++
			if e.State != Failed || e.Code != want {
				t.Fatal(e)
			}
		} else if e.Code != "" {
			t.Fatal(e)
		}
	}
	if terminalCount != 1 || events[len(events)-1].State != Failed {
		t.Fatal(events)
	}
}

func TestExecutorFailureIsSanitized(t *testing.T) {
	for _, text := range []string{
		"secret full path and token",
		"C:/private/models/model.gguf token=secret-token",
		"center_unknown",
		"center_whisper_result_invalid",
		"",
	} {
		t.Run(fmt.Sprintf("unknown/%q", text), func(t *testing.T) {
			checkExecutorFailure(t, failingExecutor{err: errors.New(text)}, "center_executor_failed")
			checkExecutorFailure(t, failingExecutor{err: &Error{Status: 500, Code: text}}, "center_executor_failed")
			checkExecutorFailure(t, failingExecutor{err: fmt.Errorf("secret path: %w", &Error{Status: 500, Code: text})}, "center_executor_failed")
		})
	}
}

func TestExecutorFixedFailureCodes(t *testing.T) {
	for _, code := range []string{
		"center_model_incompatible",
		"center_binding_stale",
		"center_component_missing",
		"center_protocol_unsupported",
		"center_whisper_failed",
		"center_whisper_window_unstable",
		"center_llama_load_failed",
		"center_llama_inference_failed",
		"center_result_invalid",
		"center_request_invalid",
		"center_runtime_busy",
	} {
		t.Run(code, func(t *testing.T) {
			checkExecutorFailure(t, failingExecutor{err: errors.New(code)}, code)
			checkExecutorFailure(t, failingExecutor{err: &Error{Status: 500, Code: code}}, code)
			checkExecutorFailure(t, failingExecutor{err: fmt.Errorf("C:/private token=secret: %w", &Error{Status: 500, Code: code})}, code)
			for _, forged := range []string{"secret prefix " + code, code + " token=secret", " " + code, code + "\n"} {
				checkExecutorFailure(t, failingExecutor{err: errors.New(forged)}, "center_executor_failed")
				checkExecutorFailure(t, failingExecutor{err: &Error{Status: 500, Code: forged}}, "center_executor_failed")
			}
			checkExecutorFailure(t, failingExecutor{err: fmt.Errorf("secret: %w", errors.New(code))}, "center_executor_failed")
			checkExecutorFailure(t, failingExecutor{panicValue: code}, "center_executor_failed")
			checkExecutorFailure(t, failingExecutor{panicValue: &Error{Status: 500, Code: code}}, "center_executor_failed")
		})
	}
	t.Run("secret panic", func(t *testing.T) {
		checkExecutorFailure(t, failingExecutor{panicValue: "C:/private token=secret"}, "center_executor_failed")
	})
}

func TestCancelWinsAgainstBlockedSuccessAndProgress(t *testing.T) {
	for i := 0; i < 25; i++ {
		e := newControlled()
		m, _ := New(e, Options{})
		s := submit(t, m, "r")
		call := <-e.calls
		cancelled := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); m.Cancel("session", "task", s.ID); close(cancelled) }()
		go func() {
			defer wg.Done()
			<-cancelled
			call.emit(Progress{Stage: StageRunning, Completed: 1, Total: 1})
			finish(call)
		}()
		wg.Wait()
		m.Close()
		got, _ := m.Snapshot("session", "task", s.ID)
		if got.State != Cancelled {
			t.Fatal(got)
		}
	}
}

func TestFrozenRequestCannotBeMutatedByExecutor(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{})
	s := submit(t, m, "r")
	call := <-e.calls
	call.request.Body.Score.EligibleUnits[0].ID = "mutated"
	call.reply <- Result{Component: call.request.Body.Component, Events: []*RawEvent{{HighlightScore: 1, Title: "t", Reason: "r", Tags: []string{}, StartUnitID: &call.request.Body.Score.EligibleUnits[0].ID, EndUnitID: &call.request.Body.Score.EligibleUnits[0].ID}}}
	got := waitTerminal(t, m, s.ID)
	if got.State != Failed || got.Code != "center_result_invalid" {
		t.Fatal(got)
	}
	m.Close()
}

func TestDelayedTimerCannotAdmitExpiredQueueOrSuccess(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	e := newControlled()
	m, _ := New(e, Options{Clock: c})
	s := submit(t, m, "r")
	call := <-e.calls
	q := submit(t, m, "q")
	c.mu.Lock()
	c.now = c.now.Add(31 * time.Minute)
	c.mu.Unlock()
	finish(call)
	got := waitTerminal(t, m, s.ID)
	if got.Code != "center_job_timeout" {
		t.Fatal(got)
	}
	m.Close()
	queued, _ := m.Snapshot("session", "task", q.ID)
	if queued.State == Succeeded {
		t.Fatal(queued)
	}
	select {
	case <-e.calls:
		t.Fatal("expired queue executed")
	default:
	}
}

func TestConcurrentCloseAndTerminalCancelIdempotent(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{})
	s := submit(t, m, "r")
	call := <-e.calls
	finish(call)
	waitTerminal(t, m, s.ID)
	got, err := m.Cancel("session", "task", s.ID)
	if err != nil || got.State != Succeeded {
		t.Fatal(got, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
