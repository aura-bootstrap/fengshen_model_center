// Package runtimeassets owns bounded, task-scoped runtime media. It performs no
// ownership: callers must pass their task/session before invoking it.
package runtimeassets

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

type Kind string

const (
	WAV   Kind = "wav"
	Image Kind = "image"
	JSON  Kind = "json"
)

var (
	ErrInvalid   = errors.New("center_result_invalid")
	ErrForbidden = errors.New("center_asset_forbidden")
	ErrFull      = errors.New("center_spool_full")
)

type Metadata struct {
	Kind            Kind    `json:"kind"`
	Bytes           string  `json:"bytes"`
	SHA256          string  `json:"sha256"`
	SampleRate      uint32  `json:"sample_rate,omitempty"`
	Channels        uint16  `json:"channels,omitempty"`
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	Width           int     `json:"width,omitempty"`
	Height          int     `json:"height,omitempty"`
}

// Asset has no path, filename, or writable fields. Metadata returns a copy.
type Asset struct {
	id   string
	meta Metadata
}

func (a Asset) ID() string         { return a.id }
func (a Asset) Metadata() Metadata { return a.meta }

type limits struct {
	wav, image, json, total int64
	items                   int
}

var hard = limits{1 << 30, 8 << 20, 4 << 20, 4 << 30, 32}

type Store struct {
	mu          sync.Mutex
	parent, dir *os.Root
	scope       *safefs.Scope
	name        string
	lim         limits
	used        int64
	counts      map[string]int
	tasks       map[string]string
	released    map[string]bool
	files       map[string]*ResultFile
	closed      bool
}

// ResultFile is a store-owned, sequential, bounded writer. It intentionally does
// not expose a filesystem path, descriptor, seek, or arbitrary file adoption.
// Close aborts an unpublished result. PublishResult consumes it even on failure.
type ResultFile struct {
	store                       *Store
	file                        *os.File
	id, task, session, identity string
	kind                        Kind
	expected, written           int64
	published, output, dead     bool
	writeErr                    error
	meta                        Metadata
	readers                     []*Reader
}

// New requires an existing absolute local root. Only a newly created random
// child is owned; existing files and other store instances are never cleaned.
func New(root string) (*Store, error) { return newWithLimits(root, hard) }
func newWithLimits(root string, l limits) (*Store, error) {
	if l.wav <= 0 || l.wav > hard.wav || l.image <= 0 || l.image > hard.image || l.json <= 0 || l.json > hard.json || l.total <= 0 || l.total > hard.total || l.items <= 0 || l.items > hard.items {
		return nil, ErrInvalid
	}
	if e := safefs.ValidatePath(root); e != nil {
		return nil, e
	}
	// Derive each component, unlike safefs.Open which permits resolving a root link.
	// safefs deliberately disallows volume roots. Check the first directory
	// without following links, then use Derive for every remaining component.
	parts := splitRoot(root)
	if len(parts) == 0 {
		return nil, ErrInvalid
	}
	anchor, e := safefs.Open(parts[0])
	if e != nil {
		return nil, e
	}
	st, err := os.Lstat(parts[0])
	if err != nil || !ordinaryDirectory(st) || anchor.Path() != parts[0] {
		anchor.Close()
		return nil, safefs.ErrBoundary
	}
	for _, p := range parts[1:] {
		next, err := anchor.Derive(p)
		anchor.Close()
		if err != nil {
			return nil, err
		}
		anchor = next
	}
	defer anchor.Close()
	parent, e := os.OpenRoot(anchor.Path())
	if e != nil {
		return nil, e
	}
	name, e := randomID()
	if e != nil {
		parent.Close()
		return nil, e
	}
	name = "runtime-" + name
	if e = parent.Mkdir(name, 0700); e != nil {
		parent.Close()
		return nil, e
	}
	scope, e := anchor.Derive(name)
	if e != nil {
		parent.Remove(name)
		parent.Close()
		return nil, e
	}
	dir, e := parent.OpenRoot(name)
	if e != nil {
		scope.Close()
		parent.Remove(name)
		parent.Close()
		return nil, e
	}
	return &Store{parent: parent, dir: dir, scope: scope, name: name, lim: l, counts: map[string]int{}, tasks: map[string]string{}, released: map[string]bool{}, files: map[string]*ResultFile{}}, nil
}
func splitRoot(p string) []string {
	var names []string
	for {
		d := filepath.Dir(p)
		if d == p {
			break
		}
		names = append(names, filepath.Base(p))
		p = d
	}
	if len(names) == 0 {
		return nil
	}
	out := []string{filepath.Join(p, names[len(names)-1])}
	for i := len(names) - 2; i >= 0; i-- {
		out = append(out, names[i])
	}
	return out
}
func randomID() (string, error) {
	var b [24]byte
	_, e := rand.Read(b[:])
	return hex.EncodeToString(b[:]), e
}
func (s *Store) max(k Kind) int64 {
	switch k {
	case WAV:
		return s.lim.wav
	case Image:
		return s.lim.image
	case JSON:
		return s.lim.json
	}
	return 0
}
func (s *Store) create(ctx context.Context, task, session string, k Kind, n int64, output bool) (*ResultFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if s.closed || s.released[session] || task == "" || session == "" {
		return nil, ErrForbidden
	}
	if old := s.tasks[session]; old != "" && old != task {
		return nil, ErrForbidden
	}
	if n <= 0 || n > s.max(k) || (!output && k == JSON) {
		return nil, ErrInvalid
	}
	if s.counts[session] >= s.lim.items || n > s.lim.total-s.used {
		return nil, ErrFull
	}
	id, e := randomID()
	if e != nil {
		return nil, e
	}
	f, e := s.dir.OpenFile(id, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	identity, e := safefs.Identity(f)
	if e != nil {
		f.Close()
		s.dir.Remove(id)
		return nil, e
	}
	r := &ResultFile{store: s, file: f, id: id, task: task, session: session, identity: identity, kind: k, expected: n, output: output}
	s.files[id] = r
	s.tasks[session] = task
	s.counts[session]++
	s.used += n
	return r, nil
}
func (s *Store) CreateResult(ctx context.Context, task, session string, k Kind, contentLength int64) (*ResultFile, error) {
	return s.create(ctx, task, session, k, contentLength, true)
}
func (r *ResultFile) Write(b []byte) (int, error) {
	if r == nil || r.store == nil {
		return 0, ErrForbidden
	}
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.dead || r.published || s.closed {
		return 0, ErrForbidden
	}
	if r.writeErr != nil {
		return 0, r.writeErr
	}
	if int64(len(b)) > r.expected-r.written {
		r.writeErr = ErrFull
		return 0, ErrFull
	}
	n, e := r.file.Write(b)
	r.written += int64(n)
	if e != nil {
		r.writeErr = e
	}
	return n, e
}
func (s *Store) discard(r *ResultFile) error {
	if r.dead {
		return nil
	}
	r.dead = true
	for _, reader := range r.readers {
		reader.Close()
	}
	r.readers = nil
	r.file.Close()
	delete(s.files, r.id)
	s.used -= r.expected
	s.counts[r.session]--
	return s.dir.Remove(r.id)
}
func (r *ResultFile) Close() error {
	if r == nil || r.store == nil {
		return ErrForbidden
	}
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.published {
		return nil
	}
	return s.discard(r)
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	n, e := r.r.Read(b)
	if c := r.ctx.Err(); c != nil {
		return n, c
	}
	return n, e
}
func (s *Store) Upload(ctx context.Context, task, session string, k Kind, contentLength int64, src io.Reader) (Asset, error) {
	if src == nil {
		return Asset{}, ErrInvalid
	}
	r, e := s.create(ctx, task, session, k, contentLength, false)
	if e != nil {
		return Asset{}, e
	}
	defer r.Close()
	n, e := io.CopyBuffer(r, io.LimitReader(contextReader{ctx, src}, contentLength+1), make([]byte, 64<<10))
	if e != nil {
		return Asset{}, e
	}
	if n != contentLength {
		return Asset{}, ErrInvalid
	}
	return s.publish(ctx, task, r, false)
}
func (s *Store) PublishResult(ctx context.Context, task string, r *ResultFile) (Asset, error) {
	return s.publish(ctx, task, r, true)
}
func (s *Store) checked(r *ResultFile) (*os.File, error) {
	leaf, e := s.scope.Derive(r.id)
	if e != nil {
		return nil, ErrInvalid
	}
	defer leaf.Close()
	f, entry, e := leaf.OpenFile(r.id)
	if e != nil {
		return nil, ErrInvalid
	}
	if entry.Identity != r.identity || entry.LinkKind != "none" || entry.Size != r.expected {
		f.Close()
		return nil, ErrInvalid
	}
	return f, nil
}
func (s *Store) publish(ctx context.Context, task string, r *ResultFile, output bool) (a Asset, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r == nil || r.store != s || s.files[r.id] != r || r.dead || r.published || r.output != output || r.task != task {
		return a, ErrForbidden
	}
	defer func() {
		if err != nil {
			s.discard(r)
		}
	}()
	if err = ctx.Err(); err != nil {
		return
	}
	if s.closed || s.released[r.session] {
		err = ErrForbidden
		return
	}
	if r.writeErr != nil {
		err = r.writeErr
		return
	}
	if r.written != r.expected {
		err = ErrInvalid
		return
	}
	if err = r.file.Sync(); err != nil {
		return
	}
	var f *os.File
	f, err = s.checked(r)
	if err != nil {
		return
	}
	defer f.Close()
	r.meta, err = inspect(ctx, f, r.kind, r.expected)
	if err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	r.file.Close()
	r.published = true
	return Asset{r.id, r.meta}, nil
}

// Reader exposes only bounded read/seek and close, never Name/Fd/Write.
type Reader struct {
	closed  atomic.Bool
	mu      sync.Mutex
	file    *os.File
	section *io.SectionReader
	ctx     context.Context
}

func (r *Reader) Read(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.section.Read(b)
}
func (r *Reader) Seek(n int64, w int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.section.Seek(n, w)
}
func (r *Reader) Close() error {
	if r.closed.Swap(true) {
		return nil
	}
	return r.file.Close()
}
func (s *Store) open(ctx context.Context, task, id string, output bool) (*Reader, Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return nil, Metadata{}, e
	}
	r := s.files[id]
	if s.closed || r == nil || r.dead || !r.published || r.task != task || r.output != output {
		return nil, Metadata{}, ErrForbidden
	}
	f, e := s.checked(r)
	if e != nil {
		return nil, Metadata{}, e
	}
	m, e := inspect(ctx, f, r.kind, r.expected)
	if e != nil || m != r.meta {
		f.Close()
		return nil, Metadata{}, ErrInvalid
	}
	var active []*Reader
	for _, reader := range r.readers {
		if !reader.closed.Load() {
			active = append(active, reader)
		}
	}
	r.readers = active
	if len(active) >= 32 {
		f.Close()
		return nil, Metadata{}, ErrFull
	}
	reader := &Reader{file: f, section: io.NewSectionReader(f, 0, r.expected), ctx: ctx}
	r.readers = append(r.readers, reader)
	return reader, m, nil
}
func (s *Store) Open(ctx context.Context, task, assetID string) (*Reader, Metadata, error) {
	return s.open(ctx, task, assetID, false)
}
func (s *Store) Download(ctx context.Context, task, resultID string) (*Reader, Metadata, error) {
	return s.open(ctx, task, resultID, true)
}
func (s *Store) ReleaseSession(session string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released[session] = true
	var errs []error
	for _, r := range s.files {
		if r.session == session {
			errs = append(errs, s.discard(r))
		}
	}
	return errors.Join(errs...)
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	for _, r := range s.files {
		errs = append(errs, s.discard(r))
	}
	errs = append(errs, s.scope.Close(), s.dir.Close(), s.parent.Remove(s.name), s.parent.Close())
	return errors.Join(errs...)
}
func digest(ctx context.Context, f *os.File, n int64) (string, error) {
	h := sha256.New()
	count, e := io.Copy(h, contextReader{ctx, io.NewSectionReader(f, 0, n)})
	if e != nil {
		return "", e
	}
	if count != n {
		return "", ErrInvalid
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func byteString(n int64) string { return strconv.FormatInt(n, 10) }
