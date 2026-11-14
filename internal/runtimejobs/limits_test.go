package runtimejobs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestHaarIndependentSlotsAndDeadline(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	e := newControlled()
	m, _ := New(e, Options{Clock: c})
	submit(t, m, "heavy")
	heavy := <-e.calls
	var calls []invocation
	var ids []string
	for i := 0; i < 2; i++ {
		s, err := m.Submit(context.Background(), "session", "task", fmt.Sprint("face", i), binding("image.face"), body("image.face"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, s.ID)
		calls = append(calls, <-e.calls)
	}
	_, err := m.Submit(context.Background(), "session", "task", "third", binding("image.face"), body("image.face"))
	code(t, err, 429)
	c.Advance(10 * time.Second)
	for _, id := range ids {
		s, _ := m.Snapshot("session", "task", id)
		if s.State != Failed || s.Code != "center_job_timeout" {
			t.Fatal(s)
		}
	}
	if heavy.ctx.Err() != nil {
		t.Fatal("Haar timeout cancelled heavy job")
	}
	// Even cancelled jobs retain their physical slot until their executor returns.
	_, err = m.Submit(context.Background(), "session", "task", "fourth", binding("image.face"), body("image.face"))
	code(t, err, 429)
	for _, call := range calls {
		call.reply <- Result{Component: call.request.Body.Component, Face: NoFace}
	}
	m.ExpireSession("session")
	finish(heavy)
	m.Close()
}

func TestRetentionProtectsUnacknowledgedResultsAndIdempotency(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	e := newControlled()
	m, _ := New(e, Options{Clock: c, MaxJobs: 1})
	s := submit(t, m, "r")
	call := <-e.calls
	finish(call)
	if waitTerminal(t, m, s.ID).State != Succeeded {
		t.Fatal("not successful")
	}
	c.Advance(time.Hour)
	if submit(t, m, "r").ID != s.ID {
		t.Fatal("lost idempotency")
	}
	_, err := m.Submit(context.Background(), "session", "task", "full", binding("highlight.score"), body("highlight.score"))
	code(t, err, 507)
	if _, err = m.Result("session", "task", s.ID); err != nil {
		t.Fatal("unacknowledged result deleted", err)
	}
	if err = m.Acknowledge("other", "task", s.ID); err == nil {
		t.Fatal("foreign ack")
	}
	if err = m.Acknowledge("session", "task", s.ID); err != nil {
		t.Fatal(err)
	}
	_, err = m.Snapshot("session", "task", s.ID)
	code(t, err, 404)
	next := submit(t, m, "next")
	nextCall := <-e.calls
	m.Cancel("session", "task", next.ID)
	finish(nextCall)
	m.Close()
}

func TestResultCapacityIsReservedAtAdmission(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{MaxRetainedBytes: MaxResultBytes})
	s := submit(t, m, "r")
	call := <-e.calls
	_, err := m.Submit(context.Background(), "session", "task", "full", binding("highlight.score"), body("highlight.score"))
	code(t, err, 507)
	m.Cancel("session", "task", s.ID)
	// Cancellation cannot release capacity while the executor still owns work.
	_, err = m.Submit(context.Background(), "session", "task", "q", binding("highlight.score"), body("highlight.score"))
	code(t, err, 507)
	finish(call)
	m.wg.Wait()
	q := submit(t, m, "q")
	if q.State != Loading {
		t.Fatal(q)
	}
	nextCall := <-e.calls
	m.ExpireSession("session")
	finish(nextCall)
	m.Close()
}

func TestStrictDecodeAndBoundedTypedBody(t *testing.T) {
	for _, raw := range []string{`null`, `{"face":null}`, `{"face":{"asset_id":null}}`, `{"face":{"asset_id":"a","asset_id":"b"}}`, `{"Face":{"asset_id":"a"}}`, "{\"face\":{\"asset_id\":\"\xff\"}}", `{"command":"cmd.exe"}`, `{"score":{"unknown":1}}`, `{} {}`, `{"face":{"asset_id":123}}`, strings.Repeat(" ", MaxBodyBytes+1)} {
		if _, err := DecodeBody([]byte(raw)); err == nil {
			t.Fatalf("accepted %q", raw[:min(60, len(raw))])
		}
	}
	e := newControlled()
	m, _ := New(e, Options{})
	defer m.Close()
	tests := []Body{body("highlight.score"), body("highlight.score"), body("highlight.score"), body("highlight.score")}
	tests[0].Face = &FaceInput{AssetID: "image"}
	tests[1].Score.EligibleUnits[0].Text = strings.Repeat("x", 8193)
	tests[2].Score.EligibleUnits[0].EndSec = -1
	tests[3].Score.EligibleUnits = append(tests[3].Score.EligibleUnits, tests[3].Score.EligibleUnits[0])
	for i, b := range tests {
		_, err := m.Submit(context.Background(), "session", "task", fmt.Sprint(i), binding("highlight.score"), b)
		code(t, err, 422)
	}
}

func TestInvalidResultAndSecretErrorsNeverPublished(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{})
	s := submit(t, m, "r")
	call := <-e.calls
	call.reply <- Result{Component: call.request.Body.Component, Events: []*RawEvent{{HighlightScore: 1.9}}}
	got := waitTerminal(t, m, s.ID)
	if got.State != Failed || got.Code != "center_result_invalid" {
		t.Fatal(got)
	}
	m.Close()
}

func TestQueuedCancelNeverExecutesAndEventsWait(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{ScoreSlots: 1}) // cap scores so "q" queues behind "r"
	s := submit(t, m, "r")
	call := <-e.calls
	q := submit(t, m, "q")
	m.Cancel("session", "task", q.ID)
	m.Cancel("session", "task", q.ID)
	current, _ := m.Snapshot("session", "task", s.ID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := m.WaitEvents(ctx, "session", "task", s.ID, current.Cursor)
	if err != context.Canceled {
		t.Fatal(err)
	}
	_, _, err = m.Events("session", "task", s.ID, current.Cursor+1)
	code(t, err, 422)
	call.emit(Progress{Stage: StageRunning, Completed: 1, Total: 2})
	events, _, err := m.WaitEvents(context.Background(), "session", "task", s.ID, current.Cursor)
	if err != nil || len(events) != 1 || events[0].State != Running {
		t.Fatal(events, err)
	}
	events[0].Progress.Completed = 99
	again, _, _ := m.Events("session", "task", s.ID, current.Cursor)
	if again[0].Progress.Completed != 1 {
		t.Fatal("event alias")
	}
	finish(call)
	waitTerminal(t, m, s.ID)
	m.Close()
	select {
	case <-e.calls:
		t.Fatal("cancelled queue executed")
	default:
	}
}
