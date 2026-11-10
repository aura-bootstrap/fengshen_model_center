package bindings

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

type fileOps struct {
	write   func(*os.File, []byte) error
	sync    func(*os.File) error
	close   func(*os.File) error
	replace func(string, string) error
}

func defaultOps() fileOps {
	return fileOps{
		write: func(f *os.File, b []byte) error {
			n, err := f.Write(b)
			if err == nil && n != len(b) {
				err = io.ErrShortWrite
			}
			return err
		},
		sync: (*os.File).Sync, close: (*os.File).Close, replace: safefs.Replace,
	}
}

type diskStamp struct {
	exists   bool
	identity string
	size     int64
	modified time.Time
	hash     [32]byte
}

type Store struct {
	mu       sync.Mutex
	path     string
	lock     io.Closer
	data     *safefs.Scope
	dirID    string
	state    Snapshot
	stamp    diskStamp
	closed   bool
	readOnly bool
	ops      fileOps
}

// Open holds bindings.lock until Close. Only an absent bindings.json is empty;
// malformed, oversized, unsafe, or unsupported documents return ErrCorrupt and
// release all handles. No legacy registration migration or automatic repair runs.
func Open(dir string) (*Store, error) {
	if err := safefs.SecureDir(dir); err != nil {
		return nil, err
	}
	lock, err := safefs.Lock(filepath.Join(dir, "bindings.lock"))
	if err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "bindings.json"), lock: lock, ops: defaultOps(), state: Snapshot{SchemaVersion: 1, Bindings: map[string]Binding{}}}
	fail := func(err error) (*Store, error) {
		if s.data != nil {
			s.data.Close()
		}
		lock.Close()
		return nil, err
	}
	s.data, err = safefs.Open(dir)
	if err != nil {
		return fail(err)
	}
	f, err := os.Open(dir)
	if err != nil {
		return fail(err)
	}
	s.dirID, err = safefs.Identity(f)
	f.Close()
	if err != nil {
		return fail(err)
	}
	raw, stamp, err := s.read()
	if err == nil && stamp.exists {
		s.state, err = decode(raw)
	}
	if err != nil {
		return fail(fmt.Errorf("%w: %v", ErrCorrupt, err))
	}
	s.stamp = stamp
	return s, nil
}

// read uses the pinned data-directory scope, never any FrozenFile path.
func (s *Store) read() ([]byte, diskStamp, error) {
	var stamp diskStamp
	// Detect replacement/redirection of the directory used for temporary writes.
	st, err := os.Lstat(filepath.Dir(s.path))
	if err != nil {
		return nil, stamp, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, stamp, safefs.ErrBoundary
	}
	anchor, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return nil, stamp, err
	}
	id, err := safefs.Identity(anchor)
	anchor.Close()
	if err != nil {
		return nil, stamp, err
	}
	if id != s.dirID {
		return nil, stamp, safefs.ErrBoundary
	}
	st, err = os.Lstat(s.path)
	if os.IsNotExist(err) {
		return nil, stamp, nil
	}
	if err != nil {
		return nil, stamp, err
	}
	if !st.Mode().IsRegular() {
		return nil, stamp, safefs.ErrBoundary
	}
	f, entry, err := s.data.OpenFile("bindings.json")
	if err != nil {
		return nil, stamp, err
	}
	defer f.Close()
	if entry.LinkKind != "none" {
		return nil, stamp, safefs.ErrBoundary
	}
	if entry.Size > MaxBytes {
		return nil, stamp, errors.New("binding document exceeds capacity")
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, stamp, err
	}
	if len(raw) > MaxBytes || int64(len(raw)) != entry.Size {
		return nil, stamp, errors.New("binding document size changed or exceeds capacity")
	}
	after, err := f.Stat()
	if err != nil {
		return nil, stamp, err
	}
	if after.Size() != entry.Size || !after.ModTime().Equal(entry.Modified) {
		return nil, stamp, errors.New("binding document changed while reading")
	}
	stamp = diskStamp{exists: true, identity: entry.Identity, size: entry.Size, modified: entry.Modified, hash: sha256.Sum256(raw)}
	return raw, stamp, nil
}

func (s *Store) freeze(code string) error {
	s.readOnly = true
	s.state.Error = code
	return ErrReadOnly
}

func (s *Store) check() error {
	_, stamp, err := s.read()
	if err != nil || stamp != s.stamp {
		return s.freeze("bindings_external_change: disk bindings changed; refusing overwrite")
	}
	return nil
}

// Snapshot returns an isolated copy. It checks only the small binding document,
// not the inventory or model files. On external change it preserves the last
// trusted data with Error set; it never imports the external contents.
func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed && !s.readOnly {
		s.check()
	}
	return clone(s.state)
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.data.Close(), s.lock.Close())
}

func (s *Store) Put(ctx context.Context, expectedRevision uint64, binding Binding) (Snapshot, error) {
	return s.change(ctx, expectedRevision, binding.Scene, binding, false)
}

func (s *Store) Delete(ctx context.Context, expectedRevision uint64, scene string) (Snapshot, error) {
	return s.change(ctx, expectedRevision, scene, Binding{}, true)
}

func (s *Store) change(ctx context.Context, expected uint64, scene string, binding Binding, del bool) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := func(err error) (Snapshot, error) { return clone(s.state), err }
	if s.closed {
		return out(os.ErrClosed)
	}
	if s.readOnly {
		return out(ErrReadOnly)
	}
	if ctx == nil {
		return out(errors.New("bindings_context_required"))
	}
	if err := ctx.Err(); err != nil {
		return out(err)
	}
	if err := s.check(); err != nil {
		return out(err)
	}
	if expected != s.state.Revision {
		return out(ErrConflict)
	}
	if _, ok := integration.RuntimeScene(scene); !ok {
		return out(ErrInvalid)
	}
	if s.state.Revision == ^uint64(0) {
		return out(errors.New("bindings_revision_overflow"))
	}
	next := clone(s.state)
	next.Revision++
	if del {
		if _, exists := next.Bindings[scene]; !exists {
			return out(errors.New("center_binding_missing"))
		}
		delete(next.Bindings, scene)
	} else {
		binding = normalized(binding)
		if err := validateBinding(binding); err != nil {
			return out(err)
		}
		if binding.Ref != (integration.ExecutionBindingRef{}) {
			current, exists := s.state.Bindings[scene]
			if !exists || binding.Ref != current.Ref {
				return out(ErrConflict)
			}
		}
		digest, err := contentHash(binding)
		if err != nil {
			return out(err)
		}
		binding.Ref = integration.ExecutionBindingRef{Scene: scene, Revision: next.Revision, SHA256: digest}
		next.Bindings[scene] = binding
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return out(fmt.Errorf("%w: %v", ErrInvalid, err))
	}
	if len(raw) > MaxBytes {
		return out(fmt.Errorf("%w: binding document exceeds capacity", ErrInvalid))
	}
	if err = ctx.Err(); err != nil {
		return out(err)
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".bindings-*")
	if err != nil {
		return out(err)
	}
	temp := f.Name()
	defer os.Remove(temp)
	// Also close after an injected/failed close so no temporary handle leaks.
	defer f.Close()
	if err = s.ops.write(f, raw); err == nil {
		err = s.ops.sync(f)
	}
	closeErr := s.ops.close(f)
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return out(fmt.Errorf("bindings_write_failed: %w", err))
	}
	// Confirm the temporary content too: a short write must not reach Replace.
	written, err := os.ReadFile(temp)
	if err != nil || !bytes.Equal(written, raw) {
		return out(errors.New("bindings_write_failed: incomplete temporary document"))
	}
	if err = s.check(); err != nil {
		return out(err)
	}
	if err = ctx.Err(); err != nil {
		return out(err)
	}
	// Replace is the commit boundary. Cancellation after it must not unpublish
	// a confirmed durable commit. A Replace error can mean it already committed.
	if err = s.ops.replace(temp, s.path); err != nil {
		s.freeze("bindings_commit_uncertain: replacement could not be confirmed")
		return out(fmt.Errorf("bindings_commit_uncertain: %w", err))
	}
	committed, stamp, err := s.read()
	if err != nil || !stamp.exists || !bytes.Equal(committed, raw) {
		return out(s.freeze("bindings_commit_uncertain: persisted content could not be confirmed"))
	}
	s.state, s.stamp = next, stamp
	return out(nil)
}

// unique rejects duplicate keys and null values at every depth, before decoding
// into structs can silently discard either. Depth and total bytes are bounded.
func unique(d *json.Decoder, depth int) error {
	if depth > 24 {
		return errors.New("binding JSON nesting limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return errors.New("null binding field")
	}
	if delim, ok := token.(json.Delim); ok {
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate binding JSON key")
				}
				seen[name] = true
				if err := unique(d, depth+1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := unique(d, depth+1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid binding JSON")
		}
		_, err = d.Token()
	}
	return err
}

// required checks exact, case-sensitive JSON keys and presence of every persisted
// field, including legitimately zero registry revisions and empty registrations.
// json.Decoder alone accepts case-insensitive names and absent scalar fields.
func required(raw json.RawMessage, typ reflect.Type) error {
	if typ == reflect.TypeFor[time.Time]() {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return errors.New("binding JSON object required")
		}
		expected := 0
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := field.Tag.Get("json")
			if name == "-" {
				continue
			}
			expected++
			value, exists := fields[name]
			if !exists {
				return fmt.Errorf("binding field required: %s", name)
			}
			if err := required(value, field.Type); err != nil {
				return err
			}
		}
		if len(fields) != expected {
			return errors.New("unknown binding field")
		}
	case reflect.Map:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return errors.New("binding JSON map required")
		}
		for _, value := range fields {
			if err := required(value, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil || values == nil {
			return errors.New("binding JSON array required")
		}
		for _, value := range values {
			if err := required(value, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func decode(raw []byte) (Snapshot, error) {
	var state Snapshot
	if len(raw) > MaxBytes || !utf8.Valid(raw) || !json.Valid(raw) {
		return state, errors.New("invalid binding JSON or capacity exceeded")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := unique(d, 0); err != nil {
		return state, err
	}
	if _, err := d.Token(); err != io.EOF {
		return state, errors.New("trailing binding JSON")
	}
	if err := required(raw, reflect.TypeFor[Snapshot]()); err != nil {
		return state, err
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&state); err != nil {
		return state, err
	}
	if state.SchemaVersion != 1 || state.Bindings == nil {
		return state, errors.New("unsupported binding schema")
	}
	seenRevisions := map[uint64]bool{}
	for scene, b := range state.Bindings {
		if b.Scene != scene || validateBinding(b) != nil || b.Ref.Validate() != nil || b.Ref.Scene != scene || b.Ref.Revision > state.Revision || seenRevisions[b.Ref.Revision] {
			return state, ErrInvalid
		}
		seenRevisions[b.Ref.Revision] = true
		digest, err := contentHash(b)
		if err != nil || digest != b.Ref.SHA256 {
			return state, errors.New("binding digest mismatch")
		}
		state.Bindings[scene] = normalized(b)
	}
	return state, nil
}
