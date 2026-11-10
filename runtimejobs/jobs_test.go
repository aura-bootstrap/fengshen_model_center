package runtimejobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/integration"
)

type fakeTimer struct {
	clock   *fakeClock
	at      time.Time
	fn      func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	old := !t.stopped
	t.stopped = true
	return old
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) AfterFunc(d time.Duration, fn func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, at: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, t)
	return t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var callbacks []func()
	for _, t := range c.timers {
		if !t.stopped && !t.at.After(c.now) {
			t.stopped = true
			callbacks = append(callbacks, t.fn)
		}
	}
	c.mu.Unlock()
	for _, fn := range callbacks {
		fn()
	}
}

type invocation struct {
	ctx     context.Context
	request Request
	emit    Emit
	reply   chan Result
}
type controlled struct {
	calls  chan invocation
	closed chan struct{}
	once   sync.Once
}

func newControlled() *controlled {
	return &controlled{calls: make(chan invocation, 16), closed: make(chan struct{})}
}
func (e *controlled) Execute(ctx context.Context, r Request, emit Emit) (Result, error) {
	ch := make(chan Result)
	e.calls <- invocation{ctx, r, emit, ch}
	return <-ch, nil
}
func (e *controlled) Close() error { e.once.Do(func() { close(e.closed) }); return nil }
func binding(scene string) integration.ExecutionBindingRef {
	return integration.ExecutionBindingRef{Scene: scene, Revision: 1, SHA256: strings.Repeat("a", 64)}
}
func body(scene string) Body {
	id := "llama-runtime"
	if scene == "image.face" {
		id = "haar-consumer"
	}
	b := Body{Component: integration.ExecutionComponentRef{ID: id, Version: "v1", ManifestSHA256: strings.Repeat("b", 64), Protocol: "test/v1"}}
	if scene == "image.face" {
		b.Face = &FaceInput{AssetID: "image"}
	} else {
		b.Score = &ScoreInput{EpisodeNumber: 1, StartSec: 0, EndSec: 1, EligibleUnits: []SpeechUnit{{ID: "c", Text: "hello", StartSec: 0, EndSec: 1}}}
	}
	return b
}
func submit(t *testing.T, m *Manager, id string) Snapshot {
	t.Helper()
	s, e := m.Submit(context.Background(), "session", "task", id, binding("highlight.score"), body("highlight.score"))
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func code(t *testing.T, e error, want int) {
	t.Helper()
	var x *Error
	if !errors.As(e, &x) || x.Status != want {
		t.Fatalf("error %v, want status %d", e, want)
	}
}
func finish(call invocation) {
	id := "c"
	call.reply <- Result{Component: call.request.Body.Component, Events: []*RawEvent{{HighlightScore: 0.9, Title: "title", Reason: "reason", Tags: []string{}, StartUnitID: &id, EndUnitID: &id}}}
}
func waitTerminal(t *testing.T, m *Manager, id string) Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		s, e := m.Snapshot("session", "task", id)
		if e != nil {
			t.Fatal(e)
		}
		if s.State.Terminal() {
			return s
		}
		_, _, e = m.WaitEvents(ctx, "session", "task", id, s.Cursor)
		if e != nil {
			t.Fatal(e)
		}
	}
}
func TestExpiredUnacknowledgedSuccessReclaimsOriginalRetention(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	e := newControlled()
	m, _ := New(e, Options{Clock: c, MaxJobs: 1, Retention: 30 * time.Minute})
	defer m.Close()
	s := submit(t, m, "r")
	finish(<-e.calls)
	original := waitTerminal(t, m, s.ID)
	c.Advance(15 * time.Minute)
	_, err := m.Submit(context.Background(), "new-session", "new-task", "r", binding("highlight.score"), body("highlight.score"))
	code(t, err, 507)
	m.ExpireSession("session")
	got, err := m.Snapshot("session", "task", s.ID)
	if err != nil || got != original || got.Acknowledged || got.State != Succeeded {
		t.Fatalf("expiry changed success: %+v %v", got, err)
	}
	_, err = m.Snapshot("session", "other-task", s.ID)
	code(t, err, 404)
	_, err = m.Submit(context.Background(), "session", "task", "new", binding("highlight.score"), body("highlight.score"))
	code(t, err, 401)
	_, err = m.Submit(context.Background(), "new-session", "new-task", "r", binding("highlight.score"), body("highlight.score"))
	code(t, err, 507)
	c.Advance(15 * time.Minute)
	_, err = m.Submit(context.Background(), "new-session", "new-task", "r", binding("highlight.score"), body("highlight.score"))
	if err != nil {
		t.Fatal(err)
	}
	finish(<-e.calls)
	m.mu.Lock()
	_, retained := m.requests[requestKey{"session", "r"}]
	m.mu.Unlock()
	if retained {
		t.Fatal("expired request reservation retained")
	}
}

func TestExpiredExecutorMustExitBeforePrune(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	e := newControlled()
	m, _ := New(e, Options{Clock: c, MaxJobs: 1, Retention: time.Minute})
	s := submit(t, m, "r")
	call := <-e.calls
	m.ExpireSession("session")
	m.mu.Lock()
	reserved := m.reservedBytes
	m.mu.Unlock()
	if reserved != MaxResultBytes {
		t.Fatal("executor still owns released reservation")
	}
	c.Advance(time.Minute)
	_, err := m.Submit(context.Background(), "new-session", "task", "r", binding("highlight.score"), body("highlight.score"))
	code(t, err, 507)
	got, err := m.Snapshot("session", "task", s.ID)
	if err != nil || got.State != Cancelled {
		t.Fatalf("owned terminal pruned: %+v %v", got, err)
	}
	finish(call)
	m.wg.Wait()
	_, err = m.Submit(context.Background(), "new-session", "task", "r", binding("highlight.score"), body("highlight.score"))
	if err != nil {
		t.Fatal(err)
	}
	finish(<-e.calls)
	m.Close()
}

func TestCapabilityEvidenceExactIdentityAndRetention(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	e := newControlled()
	m, _ := New(e, Options{Clock: c, Retention: time.Minute})
	defer m.Close()
	ref, b := binding("highlight.score"), body("highlight.score")
	if busy, evidence := m.CapabilityEvidence(ref, b.Component); busy || evidence != nil {
		t.Fatal("passive read invented work")
	}
	s := submit(t, m, "r")
	call := <-e.calls
	if busy, evidence := m.CapabilityEvidence(ref, b.Component); !busy || evidence != nil {
		t.Fatal("execution not busy")
	}
	finish(call)
	waitTerminal(t, m, s.ID)
	r, err := m.Result("session", "task", s.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r)
	sum := sha256.Sum256(raw)
	busy, evidence := m.CapabilityEvidence(ref, b.Component)
	if busy || evidence == nil || evidence.State != "passed" || evidence.InputSHA256 != s.BodySHA256 || evidence.ResultSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("incorrect success evidence: %v %+v", busy, evidence)
	}
	changed := ref
	changed.Revision++
	if _, evidence := m.CapabilityEvidence(changed, b.Component); evidence != nil {
		t.Fatal("cross binding evidence")
	}
	component := b.Component
	component.Version = "v2"
	if _, evidence := m.CapabilityEvidence(ref, component); evidence != nil {
		t.Fatal("cross component evidence")
	}
	c.Advance(time.Minute)
	if _, evidence := m.CapabilityEvidence(ref, b.Component); evidence != nil {
		t.Fatal("stale retained success evidence")
	}
	c.Advance(time.Second)
	failed := submit(t, m, "failed")
	call = <-e.calls
	call.reply <- Result{}
	waitTerminal(t, m, failed.ID)
	if _, evidence := m.CapabilityEvidence(ref, b.Component); evidence == nil || evidence.State != "failed" || evidence.ResultSHA256 != "" {
		t.Fatalf("failure evidence: %+v", evidence)
	}
	m.ExpireSession("session")
	if busy, evidence := m.CapabilityEvidence(ref, b.Component); busy || evidence != nil {
		t.Fatal("expired evidence projected")
	}
}

func TestCapabilityEvidenceDoesNotPromoteQueuedOrCancelled(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	e := newControlled()
	m, _ := New(e, Options{Clock: c})
	s := submit(t, m, "r")
	call := <-e.calls
	ref := binding("highlight.score")
	ref.Revision++
	_, err := m.Submit(context.Background(), "session", "task", "queued", ref, body("highlight.score"))
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(2 * time.Minute)
	if busy, evidence := m.CapabilityEvidence(ref, body("highlight.score").Component); busy || evidence != nil {
		t.Fatal("queue timeout invented inference failure")
	}
	m.Cancel("session", "task", s.ID)
	finish(call)
	m.Close()
	if busy, evidence := m.CapabilityEvidence(binding("highlight.score"), body("highlight.score").Component); busy || evidence != nil {
		t.Fatal("cancellation invented inference failure")
	}
}

func TestAdmissionIdempotencyAndOwnership(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{})
	s := submit(t, m, "r")
	call := <-e.calls
	defer func() { finish(call); m.Close() }()
	same := submit(t, m, "r")
	if s.ID != same.ID {
		t.Fatal("duplicate")
	}
	b := body("highlight.score")
	b.Score.EligibleUnits[0].Text = "different"
	_, err := m.Submit(context.Background(), "session", "task", "r", binding("highlight.score"), b)
	code(t, err, 409)
	_, err = m.Submit(context.Background(), "session", "other", "r", binding("highlight.score"), b)
	code(t, err, 409)
	_, err = m.Snapshot("other", "task", s.ID)
	code(t, err, 404)
	_, err = m.Result("session", "other", s.ID)
	code(t, err, 404)
	for i := 0; i < 8; i++ {
		submit(t, m, fmt.Sprint(i))
	}
	_, err = m.Submit(context.Background(), "session", "task", "full", binding("highlight.score"), body("highlight.score"))
	code(t, err, 429)
	m.ExpireSession("session")
}
func TestCancelCannotPublishSuccess(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{})
	s := submit(t, m, "r")
	call := <-e.calls
	m.Cancel("session", "task", s.ID)
	finish(call)
	m.Close()
	got, _ := m.Snapshot("session", "task", s.ID)
	if got.State != Cancelled {
		t.Fatal(got)
	}
	_, err := m.Result("session", "task", s.ID)
	code(t, err, 409)
	if call.emit(Progress{Stage: StageRunning, Completed: 1, Total: 1}) == nil {
		t.Fatal("late progress accepted")
	}
}
func TestClockTimeoutQueueAndExpiry(t *testing.T) {
	c := &fakeClock{now: time.Unix(100, 0)}
	e := newControlled()
	m, _ := New(e, Options{Clock: c})
	s := submit(t, m, "r")
	call := <-e.calls
	q := submit(t, m, "q")
	c.Advance(2 * time.Minute)
	qs, _ := m.Snapshot("session", "task", q.ID)
	if qs.Code != "center_queue_timeout" {
		t.Fatal(qs)
	}
	c.Advance(28 * time.Minute)
	got, _ := m.Snapshot("session", "task", s.ID)
	if got.Code != "center_job_timeout" {
		t.Fatal(got)
	}
	finish(call)
	m.Close()
}
func TestEventsCursorAndSessionIsolation(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{})
	s := submit(t, m, "r")
	call := <-e.calls
	for i := 0; i < 2050; i++ {
		if err := call.emit(Progress{Stage: StageRunning, Completed: i, Total: 3000}); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := m.Events("session", "task", s.ID, 0)
	code(t, err, 410)
	events, _, err := m.Events("session", "task", s.ID, 2050)
	if err != nil || len(events) == 0 {
		t.Fatal(err)
	}
	for i := 1; i < len(events); i++ {
		if events[i].Sequence <= events[i-1].Sequence {
			t.Fatal("nonmonotonic")
		}
	}
	m.ExpireSession("someone_else")
	if call.ctx.Err() != nil {
		t.Fatal("foreign expiry")
	}
	m.ExpireSession("session")
	if call.ctx.Err() == nil {
		t.Fatal("not cancelled")
	}
	_, err = m.Submit(context.Background(), "session", "task", "new", binding("highlight.score"), body("highlight.score"))
	code(t, err, 401)
	finish(call)
	m.Close()
}
func TestCloseWaitsAndRejects(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{})
	submit(t, m, "r")
	call := <-e.calls
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	<-call.ctx.Done()
	_, err := m.Submit(context.Background(), "other", "task", "new", binding("highlight.score"), body("highlight.score"))
	code(t, err, 503)
	select {
	case <-done:
		t.Fatal("close returned before executor")
	default:
	}
	finish(call)
	<-done
	select {
	case <-e.closed:
	default:
		t.Fatal("executor not closed")
	}
}
func TestHTTPContextIndependentAndOwnedResults(t *testing.T) {
	e := newControlled()
	m, _ := New(e, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	s, err := m.Submit(ctx, "session", "task", "r", binding("highlight.score"), body("highlight.score"))
	if err != nil {
		t.Fatal(err)
	}
	call := <-e.calls
	cancel()
	if call.ctx.Err() != nil {
		t.Fatal("HTTP cancellation leaked")
	}
	finish(call)
	if waitTerminal(t, m, s.ID).State != Succeeded {
		t.Fatal("not succeeded")
	}
	r, err := m.Result("session", "task", s.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.Events[0].HighlightScore = 0
	again, _ := m.Result("session", "task", s.ID)
	if again.Events[0].HighlightScore != 0.9 {
		t.Fatal("result alias")
	}
	m.Close()
}
