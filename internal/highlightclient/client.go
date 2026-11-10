// Package highlightclient adapts an engine task to the center.
// The caller owns task preparation and lifecycle.
// This package never opens a session, prepares media, or retries uncertain writes.
package highlightclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/scoring"
)

type Binding struct {
	Binding   integration.ExecutionBindingRef
	Component integration.ExecutionComponentRef
}

// The private receipt keeps transport injection independent of opaque runtime
// session/jobs. Production receipts are created only by RuntimeClient.Submit.
type job struct {
	native   *integration.RuntimeJob
	snapshot integration.RuntimeSnapshot
}
type runtimeTransport interface {
	Upload(context.Context, string, int64, io.Reader) (integration.RuntimeAsset, error)
	Submit(context.Context, string, Binding, integration.RuntimeBody) (*job, error)
	Events(context.Context, *job, uint64, func(integration.RuntimeEvent) error) error
	Query(context.Context, *job) (integration.RuntimeSnapshot, error)
	Result(context.Context, *job) (integration.RuntimeResult, error)
	Cancel(context.Context, *job) (integration.RuntimeSnapshot, error)
	Acknowledge(context.Context, *job) (integration.RuntimeSnapshot, error)
}
type sessionTransport struct {
	runtime *integration.RuntimeClient
	session *integration.RuntimeSessionHandle
}

func (t sessionTransport) Upload(ctx context.Context, typ string, size int64, r io.Reader) (integration.RuntimeAsset, error) {
	return t.runtime.Upload(ctx, t.session, typ, size, r)
}
func (t sessionTransport) Submit(ctx context.Context, id string, b Binding, body integration.RuntimeBody) (*job, error) {
	j, e := t.runtime.Submit(ctx, t.session, id, b.Binding, body)
	if e != nil {
		return nil, e
	}
	if j == nil {
		return nil, errResult
	}
	return &job{native: j, snapshot: j.Snapshot()}, nil
}
func (t sessionTransport) Events(ctx context.Context, j *job, cursor uint64, emit func(integration.RuntimeEvent) error) error {
	return t.runtime.Events(ctx, t.session, j.native, cursor, emit)
}
func (t sessionTransport) Query(ctx context.Context, j *job) (integration.RuntimeSnapshot, error) {
	return t.runtime.Query(ctx, t.session, j.native)
}
func (t sessionTransport) Result(ctx context.Context, j *job) (integration.RuntimeResult, error) {
	return t.runtime.Result(ctx, t.session, j.native)
}
func (t sessionTransport) Cancel(ctx context.Context, j *job) (integration.RuntimeSnapshot, error) {
	return t.runtime.Cancel(ctx, t.session, j.native)
}

func (t sessionTransport) Acknowledge(ctx context.Context, j *job) (integration.RuntimeSnapshot, error) {
	return t.runtime.Acknowledge(ctx, t.session, j.native)
}

var (
	errResult                   = errors.New("center_result_invalid")
	errBinding                  = errors.New("center_binding_missing")
	errInput                    = errors.New("center_highlight_input_invalid")
	errMedia                    = errors.New("center_media_identity_invalid")
	ErrPreparedAudioRequired    = errors.New("center_prepared_audio_required")
	ErrStreamOptionsUnsupported = errors.New("center_stream_options_unsupported")
)

type jobFailure struct {
	state integration.RuntimeState
	code  string
}

func (e *jobFailure) Error() string       { return "center_job_" + string(e.state) }
func (e *jobFailure) FailureCode() string { return e.code }

func newJobFailure(state integration.RuntimeState, code string) error {
	// Match runtimejobs' fixed executor, cancellation and timeout codes only.
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
		"center_runtime_busy",
		"center_job_cancelled",
		"center_session_expired",
		"center_closed",
		"center_job_timeout",
		"center_queue_timeout":
		return &jobFailure{state: state, code: code}
	default:
		return &jobFailure{state: state, code: "center_executor_failed"}
	}
}

type Client struct {
	transport               runtimeTransport
	grant                   func() integration.SessionScope
	bindings                map[string]Binding
	record                  func(integration.RuntimeSnapshot) error
	complete                func(integration.RuntimeCompletion) error
	task, session, instance string
}

// New borrows an existing runtime session; it neither creates scope nor
// owns session shutdown. record must persist non-secret job receipts and may be
// called concurrently when the caller runs multiple operations concurrently.
// complete is required: it must durably store the entire owned result receipt in
// the task's private directory before returning. It can contain private dialogue.
func New(runtime *integration.RuntimeClient, session *integration.RuntimeSessionHandle, bindings []Binding, record func(integration.RuntimeSnapshot) error, complete func(integration.RuntimeCompletion) error) (*Client, error) {
	if runtime == nil || session == nil {
		return nil, errors.New("center_session_invalid")
	}
	return newClient(sessionTransport{runtime, session}, session.Scope, bindings, record, complete)
}
func bindingKey(scene, component string) string { return scene + "/" + component }
func newClient(transport runtimeTransport, grant func() integration.SessionScope, bindings []Binding, record func(integration.RuntimeSnapshot) error, complete func(integration.RuntimeCompletion) error) (*Client, error) {
	if transport == nil || grant == nil || record == nil || complete == nil || len(bindings) == 0 {
		return nil, errInput
	}
	g := grant()
	if g.SessionID == "" || g.TaskID == "" {
		return nil, errors.New("center_session_denied")
	}
	c := &Client{transport: transport, grant: grant, bindings: make(map[string]Binding), record: record, complete: complete, task: g.TaskID, session: g.SessionID, instance: g.InstanceID}
	for _, b := range bindings {
		if !strings.HasPrefix(b.Binding.Scene, "highlight.") || b.Binding.Validate() != nil || b.Component.Validate(b.Binding.Scene) != nil || !g.Allows(g.TaskID, b.Binding) {
			return nil, errors.New("center_session_denied")
		}
		key := bindingKey(b.Binding.Scene, b.Component.ID)
		if _, exists := c.bindings[key]; exists {
			return nil, errInput
		}
		c.bindings[key] = b
	}
	return c, nil
}
func (c *Client) binding(scene, component string) (Binding, error) {
	if c == nil {
		return Binding{}, errBinding
	}
	b, ok := c.bindings[bindingKey(scene, component)]
	if !ok {
		return Binding{}, errBinding
	}
	g := c.grant()
	if g.TaskID != c.task || g.SessionID != c.session || g.InstanceID != c.instance || !g.Allows(c.task, b.Binding) {
		return Binding{}, errors.New("center_session_denied")
	}
	return b, nil
}
func requestID() (string, error) {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return hex.EncodeToString(b[:]), nil
}
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// run submits once and consumes a single SSE connection from its beginning.
// A disconnected event stream is not cancellation and never causes a replay.
func (c *Client) run(ctx context.Context, b Binding, id string, body integration.RuntimeBody, consume func(integration.RuntimeEvent) (bool, error), validate func(integration.RuntimeResult) error) (result integration.RuntimeResult, err error) {
	if err = ctx.Err(); err != nil {
		return result, err
	}
	j, e := c.transport.Submit(ctx, id, b, body)
	if e != nil {
		return result, e
	}
	if j == nil || j.snapshot.TaskID != c.task || j.snapshot.SessionID != c.session || j.snapshot.Binding != b.Binding || j.snapshot.RequestID != id {
		return result, errResult
	}
	consumerCancelled := false
	terminalSuccess := false
	defer func() {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		if err != nil && !terminalSuccess && (ctx.Err() != nil || consumerCancelled) {
			cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			snapshot, cancelErr := c.transport.Cancel(cancelCtx, j)
			if cancelErr == nil {
				cancelErr = c.record(snapshot)
			}
			err = errors.Join(err, cancelErr)
		}
	}()
	if err = c.record(j.snapshot); err != nil {
		return result, err
	}
	var callbackErr error
	var commits []integration.RuntimeStreamCommit
	commitBytes := 0
	err = c.transport.Events(ctx, j, 0, func(v integration.RuntimeEvent) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if v.Commit != nil {
			raw, e := json.Marshal(v.Commit)
			commitBytes += len(raw)
			if e != nil || commitBytes > 4<<20 || len(commits) >= 16384 {
				callbackErr = errResult
				return errResult
			}
			var owned integration.RuntimeStreamCommit
			if json.Unmarshal(raw, &owned) != nil {
				callbackErr = errResult
				return errResult
			}
			commits = append(commits, owned)
		}
		if consume == nil {
			if v.Commit != nil {
				return errResult
			}
			return nil
		}
		cancel, e := consume(v)
		consumerCancelled = consumerCancelled || cancel
		if e != nil {
			callbackErr = e
		}
		return e
	})
	if callbackErr != nil {
		return result, callbackErr
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	snapshot, e := c.transport.Query(ctx, j)
	if e != nil {
		return result, e
	}
	old := j.snapshot
	if snapshot.ID != old.ID || snapshot.TaskID != c.task || snapshot.SessionID != c.session || snapshot.RequestID != old.RequestID || snapshot.Binding != old.Binding || snapshot.BodySHA256 != old.BodySHA256 || !snapshot.CreatedAt.Equal(old.CreatedAt) || snapshot.Cursor < old.Cursor {
		return result, errResult
	}
	if err = c.record(snapshot); err != nil {
		return result, err
	}
	if !snapshot.State.Terminal() {
		return result, errors.New("center_job_not_terminal")
	}
	if snapshot.State != "succeeded" {
		return result, newJobFailure(snapshot.State, snapshot.Code)
	}
	terminalSuccess = true
	result, err = c.transport.Result(ctx, j)
	if err == nil && result.Component != b.Component {
		err = errResult
	}
	if err != nil {
		return result, err
	}
	if validate == nil {
		return result, errResult
	}
	if err = validate(result); err != nil {
		return result, err
	}
	receipt := integration.RuntimeCompletion{Snapshot: snapshot, Body: body, Result: result, Commits: commits}
	if err = receipt.Validate(); err != nil {
		return result, err
	}
	raw, e := json.Marshal(receipt)
	if e != nil {
		return result, errResult
	}
	var owned integration.RuntimeCompletion
	if json.Unmarshal(raw, &owned) != nil {
		return result, errResult
	}
	if err = c.complete(owned); err != nil {
		return result, err
	}
	ack, e := c.transport.Acknowledge(ctx, j)
	if e != nil {
		return result, e
	}
	if ack.ID != snapshot.ID || ack.SessionID != snapshot.SessionID || ack.TaskID != snapshot.TaskID || ack.RequestID != snapshot.RequestID || ack.Binding != snapshot.Binding || ack.BodySHA256 != snapshot.BodySHA256 || !ack.CreatedAt.Equal(snapshot.CreatedAt) || ack.State != "succeeded" || !ack.Acknowledged || ack.Cursor < snapshot.Cursor {
		return result, errResult
	}
	return result, c.record(ack)
}

// Scorer selects an explicitly frozen score or vision binding. Invalid or absent
// scenes return an adapter that fails when called, rather than falling back.
func (c *Client) Scorer(scene string) scoring.Scorer { return &scorer{client: c, scene: scene} }
