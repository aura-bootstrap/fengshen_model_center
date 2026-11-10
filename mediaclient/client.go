// Package mediaclient consumes media capabilities in an existing trusted task
// session. The engine owns authorization, media preparation and session lifetime.
package mediaclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/integration"
)

type Binding struct {
	Binding   integration.ExecutionBindingRef
	Component integration.ExecutionComponentRef
}

// Receipt contains no credentials or paths. Before admission, Snapshot is nil:
// retain RequestID and Body to investigate an uncertain POST. Completion is the
// full validated result and must be durable before record returns successfully.
// Persisting a receipt does not authorize replay or a new runtime session.
type Receipt struct {
	TaskID     string
	SessionID  string
	RequestID  string
	Binding    Binding
	Body       integration.RuntimeBody
	Snapshot   *integration.RuntimeSnapshot
	Completion *integration.RuntimeCompletion `json:"Completion,omitempty"`
}

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
	DownloadWAV(context.Context, *job, string, io.Writer) error
}
type sessionTransport struct {
	runtime *integration.RuntimeClient
	session *integration.SessionCredentials
}

func (t sessionTransport) Upload(ctx context.Context, typ string, n int64, r io.Reader) (integration.RuntimeAsset, error) {
	return t.runtime.Upload(ctx, t.session, typ, n, r)
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
func (t sessionTransport) Events(ctx context.Context, j *job, c uint64, emit func(integration.RuntimeEvent) error) error {
	return t.runtime.Events(ctx, t.session, j.native, c, emit)
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
func (t sessionTransport) DownloadWAV(ctx context.Context, j *job, id string, w io.Writer) error {
	return t.runtime.DownloadWAV(ctx, t.session, j.native, id, w)
}

var (
	errResult = errors.New("center_result_invalid")
	errMedia  = errors.New("center_media_identity_invalid")
	errInput  = errors.New("center_media_input_invalid")
	errScope  = errors.New("center_session_denied")
)

type Client struct {
	transport               runtimeTransport
	grant                   func() integration.SessionGrant
	task, session, instance string
	bindings                map[string]Binding
	record                  func(Receipt) error
	syncOutput              func(*os.File) error
	persistOutput           func(string) error
}

// New borrows credentials; it never opens, heartbeats or closes a session.
// record is required and must durably store Completion before returning. It must
// be safe for concurrent operations. Its argument is an owned copy, so persistence
// cannot mutate the admitted request or result. persistOutput must flush every
// directory entry through the engine-owned task root after the WAV is synced.
func New(runtime *integration.RuntimeClient, session *integration.SessionCredentials, bindings []Binding, record func(Receipt) error, persistOutput func(string) error) (*Client, error) {
	if runtime == nil || session == nil {
		return nil, errScope
	}
	return newClient(sessionTransport{runtime, session}, session.Grant, bindings, record, persistOutput)
}
func allowed(scene string) bool {
	return scene == "timbre.separate" || scene == "timbre.convert" || scene == "image.face"
}
func newClient(t runtimeTransport, grant func() integration.SessionGrant, bindings []Binding, record func(Receipt) error, persistOutput func(string) error) (*Client, error) {
	if t == nil || grant == nil || record == nil || persistOutput == nil || len(bindings) == 0 {
		return nil, errInput
	}
	g := grant()
	if g.Permission != integration.PermissionRuntime || g.TaskID == "" || g.SessionID == "" {
		return nil, errScope
	}
	c := &Client{transport: t, grant: grant, task: g.TaskID, session: g.SessionID, instance: g.InstanceID, bindings: map[string]Binding{}, record: record, syncOutput: (*os.File).Sync, persistOutput: persistOutput}
	for _, b := range bindings {
		if !allowed(b.Binding.Scene) || b.Binding.Validate() != nil || b.Component.Validate(b.Binding.Scene) != nil || !g.Allows(c.task, b.Binding) {
			return nil, errScope
		}
		if _, ok := c.bindings[b.Binding.Scene]; ok {
			return nil, errInput
		}
		c.bindings[b.Binding.Scene] = b
	}
	return c, nil
}
func (c *Client) binding(scene string) (Binding, error) {
	if c == nil {
		return Binding{}, errScope
	}
	b, ok := c.bindings[scene]
	if !ok {
		return Binding{}, errors.New("center_binding_missing")
	}
	g := c.grant()
	if g.TaskID != c.task || g.SessionID != c.session || g.InstanceID != c.instance || !g.Allows(c.task, b.Binding) {
		return Binding{}, errScope
	}
	return b, nil
}
func bodyHash(b Binding, body integration.RuntimeBody) string {
	raw, _ := json.Marshal(struct {
		Binding integration.ExecutionBindingRef
		Body    integration.RuntimeBody
	}{b.Binding, body})
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func (c *Client) save(id string, b Binding, body integration.RuntimeBody, s *integration.RuntimeSnapshot, completion ...*integration.RuntimeCompletion) error {
	r := Receipt{TaskID: c.task, SessionID: c.session, RequestID: id, Binding: b, Body: body, Snapshot: s}
	if len(completion) > 0 {
		r.Completion = completion[0]
	}
	raw, e := json.Marshal(r)
	if e != nil {
		return errResult
	}
	var copy Receipt
	if json.Unmarshal(raw, &copy) != nil {
		return errResult
	}
	return c.record(copy)
}
func (c *Client) snapshotValid(s integration.RuntimeSnapshot, b Binding, id, hash string, old *integration.RuntimeSnapshot) bool {
	if s.ID == "" || s.RequestID != id || s.SessionID != c.session || s.TaskID != c.task || s.Binding != b.Binding || s.BodySHA256 != hash || s.CreatedAt.IsZero() || s.Cursor == 0 {
		return false
	}
	switch s.State {
	case "queued", "loading", "running", "succeeded", "failed", "cancelled":
	default:
		return false
	}
	if s.State.Terminal() && (s.FinishedAt.IsZero() || s.FinishedAt.Before(s.CreatedAt)) {
		return false
	}
	return old == nil || s.ID == old.ID && s.CreatedAt.Equal(old.CreatedAt) && s.Cursor >= old.Cursor && (!old.State.Terminal() || s.State == old.State)
}
func (c *Client) run(ctx context.Context, b Binding, body integration.RuntimeBody, consume func(*job, integration.RuntimeResult) error) (err error) {
	if e := ctx.Err(); e != nil {
		return e
	}
	if _, e := c.binding(b.Binding.Scene); e != nil {
		return e
	}
	var random [32]byte
	if _, e := rand.Read(random[:]); e != nil {
		return errors.New("center_request_id_failed")
	}
	id := hex.EncodeToString(random[:])
	hash := bodyHash(b, body)
	if e := c.save(id, b, body, nil); e != nil {
		return e
	}
	j, e := c.transport.Submit(ctx, id, b, body)
	if e != nil {
		return e
	}
	if j == nil || !c.snapshotValid(j.snapshot, b, id, hash, nil) {
		return errResult
	}
	terminalSuccess := false
	defer func() {
		if ctx.Err() != nil && !terminalSuccess {
			err = errors.Join(err, ctx.Err())
			cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			s, e := c.transport.Cancel(cancelCtx, j)
			if e == nil {
				if !c.snapshotValid(s, b, id, hash, &j.snapshot) {
					e = errResult
				} else {
					e = c.save(id, b, body, &s)
				}
			}
			err = errors.Join(err, e)
		}
	}()
	if e = c.save(id, b, body, &j.snapshot); e != nil {
		return e
	}
	e = c.transport.Events(ctx, j, 0, func(v integration.RuntimeEvent) error {
		if v.Commit != nil {
			return errResult
		}
		return ctx.Err()
	})
	if e != nil && !errors.Is(e, io.EOF) {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	s, e := c.transport.Query(ctx, j)
	if e != nil {
		return e
	}
	if !c.snapshotValid(s, b, id, hash, &j.snapshot) {
		return errResult
	}
	if e = c.save(id, b, body, &s); e != nil {
		return e
	}
	if s.State != "succeeded" {
		if !s.State.Terminal() {
			return errors.New("center_job_not_terminal")
		}
		return errors.New("center_job_" + string(s.State))
	}
	terminalSuccess = true
	r, e := c.transport.Result(ctx, j)
	if e != nil {
		return e
	}
	if r.Component != b.Component {
		return errResult
	}
	if e = consume(j, r); e != nil {
		return e
	}
	completion := &integration.RuntimeCompletion{Snapshot: s, Body: body, Result: r}
	if e = completion.Validate(); e != nil {
		return e
	}
	if e = c.save(id, b, body, &s, completion); e != nil {
		return e
	}
	ack, e := c.transport.Acknowledge(ctx, j)
	if e != nil {
		return e
	}
	if !c.snapshotValid(ack, b, id, hash, &s) || ack.State != "succeeded" || !ack.Acknowledged {
		return errResult
	}
	return c.save(id, b, body, &ack, completion)
}

// Separate maps the ordered runtimeadapter outputs to vocals then accompaniment.
// The protocol does not attest device selection, so device is always empty.
func (c *Client) Separate(ctx context.Context, sourceWAV, vocalsWAV, accompanimentWAV string) (device string, err error) {
	return "", c.audio(ctx, "timbre.separate", sourceWAV, "", []string{vocalsWAV, accompanimentWAV})
}

// Convert sends Pitch=0; the engine retains its existing non-F0 compensation.
func (c *Client) Convert(ctx context.Context, sourceWAV, referenceWAV, outputWAV string) error {
	return c.audio(ctx, "timbre.convert", sourceWAV, referenceWAV, []string{outputWAV})
}

// Face is a local precheck only, never a replacement for server authorization.
func (c *Client) Face(ctx context.Context, imagePath, expectedSHA string) (integration.RuntimeFaceResult, error) {
	b, e := c.binding("image.face")
	if e != nil {
		return "", e
	}
	a, e := c.uploadImage(ctx, imagePath, expectedSHA)
	if e != nil {
		return "", e
	}
	var face integration.RuntimeFaceResult
	e = c.run(ctx, b, integration.RuntimeBody{Component: b.Component, Face: &integration.RuntimeAssetInput{AssetID: a.ID}}, func(_ *job, r integration.RuntimeResult) error {
		if r.Items != nil || r.Stream != nil || r.Events != nil || r.Media != nil {
			return errResult
		}
		switch r.Face {
		case integration.RuntimeFrontal, integration.RuntimeProfile, integration.RuntimeNoFace:
			face = r.Face
			return nil
		}
		return errResult
	})
	if e != nil {
		return "", e
	}
	return face, nil
}
