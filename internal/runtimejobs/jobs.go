package runtimejobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sync"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
)

type job struct {
	s              Snapshot
	request        Request
	ctx            context.Context
	cancel         context.CancelFunc
	timer          Timer
	events         []Event
	changed        chan struct{}
	result         []byte
	reserved       bool
	owned          bool
	lastCommit     *StreamCommit
	committedItems []TranscriptItem
	streamBytes    int
	invalidStream  bool
}
type requestKey struct{ session, request string }
type Manager struct {
	mu            sync.Mutex
	executor      Executor
	options       Options
	jobs          map[string]*job
	requests      map[requestKey]string
	expired       map[string]bool
	queue         []*job
	heavy, haar   int
	reservedBytes int64
	closed        bool
	wg            sync.WaitGroup
	closeOnce     sync.Once
	closeDone     chan struct{}
	closeErr      error
}

func New(executor Executor, options Options) (*Manager, error) {
	if executor == nil {
		return nil, failure(422, "center_executor_missing")
	}
	if err := options.defaults(); err != nil {
		return nil, err
	}
	return &Manager{executor: executor, options: options, jobs: map[string]*job{}, requests: map[requestKey]string{}, expired: map[string]bool{}, closeDone: make(chan struct{})}, nil
}

// Submit copies and hashes the full typed body plus binding before admission.
// ctx is consulted only for admission; accepted work owns an independent context.
// Request IDs are session-scoped, and cannot be rebound to another task.
func (m *Manager) Submit(ctx context.Context, sessionID, taskID, requestID string, binding integration.ExecutionBindingRef, body Body) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if !opaque(sessionID) || !opaque(taskID) || !opaque(requestID) || binding.Validate() != nil || !body.validate(binding.Scene) {
		return Snapshot{}, failure(422, "center_request_invalid")
	}
	raw, err := json.Marshal(body)
	if err != nil || len(raw) > MaxBodyBytes {
		return Snapshot{}, failure(413, "center_request_too_large")
	}
	var owned Body
	_ = json.Unmarshal(raw, &owned)
	frozen, _ := json.Marshal(struct {
		Binding integration.ExecutionBindingRef
		Body    Body
	}{binding, owned})
	sum := sha256.Sum256(frozen)
	hash := hex.EncodeToString(sum[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Snapshot{}, failure(503, "center_closed")
	}
	if m.expired[sessionID] {
		return Snapshot{}, failure(401, "center_session_expired")
	}
	m.pruneLocked()
	key := requestKey{sessionID, requestID}
	if id, ok := m.requests[key]; ok {
		j := m.jobs[id]
		if j.s.TaskID != taskID || j.s.BodySHA256 != hash {
			return Snapshot{}, failure(409, "center_request_conflict")
		}
		return j.s, nil
	}
	face := binding.Scene == "image.face"
	if face && m.haar >= 2 || !face && m.heavy >= 1 && len(m.queue) >= 8 {
		return Snapshot{}, failure(429, "center_runtime_busy")
	}
	if len(m.jobs) >= m.options.MaxJobs || m.reservedBytes+MaxResultBytes > m.options.MaxRetainedBytes {
		return Snapshot{}, failure(507, "center_spool_full")
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return Snapshot{}, failure(503, "center_id_unavailable")
	}
	id := hex.EncodeToString(entropy[:])
	jobCtx, cancel := context.WithCancel(context.Background())
	j := &job{s: Snapshot{ID: id, SessionID: sessionID, TaskID: taskID, RequestID: requestID, Binding: binding, BodySHA256: hash, State: Queued, CreatedAt: m.options.Clock.Now()}, ctx: jobCtx, cancel: cancel, changed: make(chan struct{}), reserved: true}
	j.request = Request{id, sessionID, taskID, requestID, binding, owned, hash}
	m.jobs[id] = j
	m.requests[key] = id
	m.reservedBytes += MaxResultBytes
	m.eventLocked(j, nil)
	if face || m.heavy == 0 {
		m.startLocked(j)
	} else {
		m.queue = append(m.queue, j)
		j.timer = m.options.Clock.AfterFunc(m.options.QueueTimeout, func() { m.timeout(j, true) })
	}
	return j.s, nil
}
func (m *Manager) eventLocked(j *job, p *Progress) {
	j.s.Cursor++
	e := Event{Sequence: j.s.Cursor, At: m.options.Clock.Now(), State: j.s.State, Code: j.s.Code, Progress: p}
	if p != nil && p.Commit != nil {
		c := cloneCommit(*p.Commit)
		e.Commit = &c
		copy := *p
		copy.Commit = nil
		e.Progress = &copy
	}
	if len(j.events) == 2048 {
		copy(j.events, j.events[1:])
		j.events[2047] = e
	} else {
		j.events = append(j.events, e)
	}
	close(j.changed)
	j.changed = make(chan struct{})
}
func (m *Manager) startLocked(j *job) {
	if j.timer != nil {
		j.timer.Stop()
	}
	j.owned = true
	j.s.State = Loading
	j.s.StartedAt = m.options.Clock.Now()
	if j.s.Binding.Scene == "image.face" {
		m.haar++
	} else {
		m.heavy++
	}
	m.eventLocked(j, nil)
	duration := m.options.RunTimeout
	if j.s.Binding.Scene == "image.face" {
		duration = m.options.HaarTimeout
	}
	j.timer = m.options.Clock.AfterFunc(duration, func() { m.timeout(j, false) })
	m.wg.Add(1)
	go m.execute(j)
}
func (m *Manager) timeout(j *job, queued bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j.s.State.Terminal() {
		return
	}
	if queued && j.s.State != Queued {
		return
	}
	code := "center_job_timeout"
	if queued {
		code = "center_queue_timeout"
	}
	m.terminalLocked(j, Failed, code)
	m.dispatchLocked()
}
func (m *Manager) terminalLocked(j *job, state State, code string) {
	if j.s.State.Terminal() {
		return
	}
	if j.timer != nil {
		j.timer.Stop()
	}
	j.cancel()
	j.s.State = state
	j.s.Code = code
	j.s.FinishedAt = m.options.Clock.Now()
	if state != Succeeded {
		j.result = nil
		if !j.owned {
			m.releaseLocked(j)
		}
	}
	m.eventLocked(j, nil)
}
func (m *Manager) releaseLocked(j *job) {
	if j.reserved {
		m.reservedBytes -= MaxResultBytes
		j.reserved = false
	}
}
func (m *Manager) dispatchLocked() {
	live := m.queue[:0]
	for _, j := range m.queue {
		if !j.s.State.Terminal() && !m.options.Clock.Now().Before(j.s.CreatedAt.Add(m.options.QueueTimeout)) {
			m.terminalLocked(j, Failed, "center_queue_timeout")
		}
		if !j.s.State.Terminal() {
			live = append(live, j)
		}
	}
	m.queue = live
	if !m.closed && m.heavy == 0 && len(m.queue) > 0 {
		j := m.queue[0]
		m.queue = m.queue[1:]
		m.startLocked(j)
	}
}

// Only fixed capability codes may cross the executor boundary.
func executorFailureCode(err error) string {
	code := err.Error()
	var runtimeError *Error
	if errors.As(err, &runtimeError) && runtimeError != nil {
		code = runtimeError.Code
	}
	switch code {
	case "center_model_incompatible",
		"center_binding_stale",
		"center_component_missing",
		"center_protocol_unsupported",
		"center_whisper_failed",
		"center_llama_load_failed",
		"center_llama_inference_failed",
		"center_result_invalid",
		"center_request_invalid",
		"center_runtime_busy":
		return code
	default:
		return "center_executor_failed"
	}
}

func (m *Manager) execute(j *job) {
	defer m.wg.Done()
	result, err := m.invoke(j)
	var data []byte
	valid := false
	if err == nil && result.validate(j.request) {
		data, err = json.Marshal(result)
		valid = err == nil && len(data) <= MaxResultBytes
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !j.s.State.Terminal() { // Clock check also protects a delayed timer callback at the deadline.
		duration := m.options.RunTimeout
		if j.s.Binding.Scene == "image.face" {
			duration = m.options.HaarTimeout
		}
		if !m.options.Clock.Now().Before(j.s.StartedAt.Add(duration)) {
			m.terminalLocked(j, Failed, "center_job_timeout")
		} else if err != nil {
			m.terminalLocked(j, Failed, executorFailureCode(err))
		} else if !valid || j.invalidStream || !streamResultMatches(j, result) {
			m.terminalLocked(j, Failed, "center_result_invalid")
		} else {
			j.result = data
			m.terminalLocked(j, Succeeded, "")
		}
	}
	j.owned = false
	if j.s.State != Succeeded {
		m.releaseLocked(j)
	}
	if j.s.Binding.Scene == "image.face" {
		m.haar--
	} else {
		m.heavy--
	}
	m.dispatchLocked()
}

// The final result is cumulative; event commits contain only newly committed items.
func streamResultMatches(j *job, r Result) bool {
	if j.request.Body.Transcribe == nil || !j.request.Body.Transcribe.Stream {
		return true
	}
	if r.Stream == nil || j.lastCommit == nil || !j.lastCommit.Final {
		return false
	}
	c := *j.lastCommit
	c.Items = j.committedItems
	// JSON equality treats an empty and nil item list as the same no-speech prefix.
	if len(c.Items) == 0 && len(r.Stream.Items) == 0 {
		c.Items = r.Stream.Items
	}
	return reflect.DeepEqual(c, *r.Stream)
}
func (m *Manager) invoke(j *job) (r Result, err error) {
	defer func() {
		if recover() != nil {
			err = failure(500, "center_executor_failed")
		}
	}()
	// Keep executor mutations from changing the frozen request used by validation.
	request := j.request
	raw, _ := json.Marshal(j.request.Body)
	request.Body = Body{}
	_ = json.Unmarshal(raw, &request.Body)
	return m.executor.Execute(j.ctx, request, func(p Progress) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		if j.s.State.Terminal() {
			return failure(409, "center_job_terminal")
		}
		duration := m.options.RunTimeout
		if j.s.Binding.Scene == "image.face" {
			duration = m.options.HaarTimeout
		}
		if !m.options.Clock.Now().Before(j.s.StartedAt.Add(duration)) {
			m.terminalLocked(j, Failed, "center_job_timeout")
			return failure(409, "center_job_terminal")
		}
		if p.Total < 0 || p.Total > 1000000000 || p.Completed < 0 || p.Completed > p.Total || p.Stage != StageLoading && p.Stage != StageRunning || j.s.State == Running && p.Stage == StageLoading {
			return failure(422, "center_progress_invalid")
		}
		if p.Stage == StageRunning {
			j.s.State = Running
		}
		if p.Commit != nil {
			input := j.request.Body.Transcribe
			if input == nil || p.Stage != StageRunning || !commitValid(*p.Commit, input, j.lastCommit) {
				return failure(422, "center_stream_invalid")
			}
			c := cloneCommit(*p.Commit)
			raw, err := json.Marshal(c)
			if err != nil || j.streamBytes+len(raw) > MaxResultBytes {
				j.invalidStream = true
				return failure(413, "center_result_too_large")
			}
			j.streamBytes += len(raw)
			j.lastCommit = &c
			j.committedItems = append(j.committedItems, c.Items...)
			p.Commit = &c
		}
		copy := p
		m.eventLocked(j, &copy)
		return nil
	})
}
func (m *Manager) lookupLocked(session, task, id string) (*job, error) {
	m.pruneLocked()
	j, ok := m.jobs[id]
	if !ok || j.s.SessionID != session || j.s.TaskID != task {
		return nil, failure(404, "center_job_not_found")
	}
	return j, nil
}
func (m *Manager) Snapshot(session, task, id string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.lookupLocked(session, task, id)
	if err != nil {
		return Snapshot{}, err
	}
	return j.s, nil
}
func (m *Manager) Result(session, task, id string) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.lookupLocked(session, task, id)
	if err != nil {
		return Result{}, err
	}
	if j.s.State != Succeeded {
		return Result{}, failure(409, "center_result_unavailable")
	}
	var r Result
	err = json.Unmarshal(j.result, &r)
	return r, err
}
func (m *Manager) Cancel(session, task, id string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.lookupLocked(session, task, id)
	if err != nil {
		return Snapshot{}, err
	}
	m.terminalLocked(j, Cancelled, "center_job_cancelled")
	m.dispatchLocked()
	return j.s, nil
}

// ExpireSession revokes future admission and cancels only this session's work.
// The authenticated session owner, not HTTP body data, calls this method.
func (m *Manager) ExpireSession(session string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expired[session] = true
	for _, j := range m.jobs {
		if j.s.SessionID == session {
			m.terminalLocked(j, Cancelled, "center_session_expired")
		}
	}
	m.dispatchLocked()
}

// CapabilityEvidence observes exact execution identities without admitting work.
// Expired sessions and evidence outside retention are never projected.
func (m *Manager) CapabilityEvidence(binding integration.ExecutionBindingRef, component integration.ExecutionComponentRef) (bool, *integration.InferenceEvidence) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var latest *integration.InferenceEvidence
	busy := false
	now := m.options.Clock.Now()
	for _, j := range m.jobs {
		if m.expired[j.s.SessionID] || j.s.Binding != binding || j.request.Body.Component != component {
			continue
		}
		busy = busy || j.owned || !j.s.State.Terminal()
		if j.s.StartedAt.IsZero() || (j.s.State != Succeeded && j.s.State != Failed) || !now.Before(j.s.FinishedAt.Add(m.options.Retention)) {
			continue
		}
		e := integration.InferenceEvidence{State: "failed", JobID: j.s.ID, InputSHA256: j.s.BodySHA256, Binding: binding, Component: component, StartedAt: j.s.StartedAt, FinishedAt: j.s.FinishedAt}
		if j.s.State == Succeeded {
			if !json.Valid(j.result) {
				continue
			}
			e.State = "passed"
			sum := sha256.Sum256(j.result)
			e.ResultSHA256 = hex.EncodeToString(sum[:])
		}
		if e.Validate(binding.Scene) != nil {
			continue
		}
		if latest == nil || e.FinishedAt.After(latest.FinishedAt) || e.FinishedAt.Equal(latest.FinishedAt) && e.JobID > latest.JobID {
			latest = &e
		}
	}
	return busy, latest
}

// Acknowledge confirms durable receipt; active unacknowledged success is protected.
func (m *Manager) Acknowledge(session, task, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.lookupLocked(session, task, id)
	if err != nil {
		return err
	}
	if !j.s.State.Terminal() {
		return failure(409, "center_job_not_terminal")
	}
	j.s.Acknowledged = true
	return nil
}
func (m *Manager) pruneLocked() {
	now := m.options.Clock.Now()
	for id, j := range m.jobs {
		if !j.owned && j.s.State.Terminal() && (j.s.State != Succeeded || j.s.Acknowledged || m.expired[j.s.SessionID]) && !now.Before(j.s.FinishedAt.Add(m.options.Retention)) {
			delete(m.jobs, id)
			delete(m.requests, requestKey{j.s.SessionID, j.s.RequestID})
			m.releaseLocked(j)
		}
	}
}
func (m *Manager) eventsLocked(j *job, cursor uint64) ([]Event, uint64, error) {
	if cursor > j.s.Cursor {
		return nil, j.s.Cursor, failure(422, "center_event_cursor_invalid")
	}
	if len(j.events) > 0 && cursor < j.events[0].Sequence-1 {
		return nil, j.s.Cursor, failure(410, "center_event_cursor_expired")
	}
	out := make([]Event, 0)
	for _, e := range j.events {
		if e.Sequence > cursor {
			if e.Progress != nil {
				p := *e.Progress
				e.Progress = &p
			}
			if e.Commit != nil {
				c := cloneCommit(*e.Commit)
				e.Commit = &c
			}
			out = append(out, e)
		}
	}
	return out, j.s.Cursor, nil
}
func (m *Manager) Events(session, task, id string, cursor uint64) ([]Event, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.lookupLocked(session, task, id)
	if err != nil {
		return nil, 0, err
	}
	return m.eventsLocked(j, cursor)
}

// WaitEvents is a lossless notification wait, not a polling/sleep loop. An empty
// response on a terminal job tells the SSE transport to finish its stream.
func (m *Manager) WaitEvents(ctx context.Context, session, task, id string, cursor uint64) ([]Event, uint64, error) {
	for {
		m.mu.Lock()
		j, err := m.lookupLocked(session, task, id)
		if err != nil {
			m.mu.Unlock()
			return nil, 0, err
		}
		events, next, err := m.eventsLocked(j, cursor)
		if err != nil || len(events) > 0 || j.s.State.Terminal() {
			m.mu.Unlock()
			return events, next, err
		}
		changed := j.changed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, cursor, ctx.Err()
		case <-changed:
		}
	}
}

// Close rejects admission, cancels all work, joins Execute calls, then closes
// the owned executor. An executor that ignores cancellation will block Close;
// the process supervisor must supply bounded kill-and-wait behavior.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		for _, j := range m.jobs {
			m.terminalLocked(j, Cancelled, "center_closed")
		}
		m.queue = nil
		m.mu.Unlock()
		m.wg.Wait()
		m.closeErr = m.executor.Close()
		close(m.closeDone)
	})
	<-m.closeDone
	return m.closeErr
}
