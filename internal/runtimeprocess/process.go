// Package runtimeprocess owns only model processes created by the center.
// It is an internal execution primitive, not an HTTP command gateway or a sandbox
// for hostile executables/models. Adapters must select manifest-pinned binaries,
// binding identities and fixed argument templates themselves. Never pass request
// arguments or secrets on the command line.
package runtimeprocess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	ErrClosed      = errors.New("runtimeprocess: closed")
	ErrIdentity    = errors.New("runtimeprocess: identity mismatch")
	ErrInputLimit  = errors.New("runtimeprocess: stdin limit exceeded")
	ErrOutputLimit = errors.New("runtimeprocess: output byte or unread buffer limit exceeded")
	ErrUnsupported = errors.New("runtimeprocess: Windows 10 or newer required")
)

// VerifiedBinary is opaque and cannot be populated from JSON. Verification is
// intentionally explicit and hashes the executable, never model weights. Start
// reopens, locks and hashes again to reject changes since verification.
type VerifiedBinary struct {
	path, digest, component, binding string
	file                             fileIdentity
}
type fileIdentity struct {
	Volume, IndexHigh, IndexLow uint32
	Size                        uint64
	Modified                    uint64
}

// VerifyBinary requires an absolute .exe path and a trusted manifest SHA-256.
// Component and binding are frozen, non-secret internal identity references.
func VerifyBinary(path, expectedSHA256, component, binding string) (VerifiedBinary, error) {
	if !filepath.IsAbs(path) || !strings.EqualFold(filepath.Ext(path), ".exe") || component == "" || binding == "" || len(component) > 1024 || len(binding) > 1024 {
		return VerifiedBinary{}, fmt.Errorf("%w: invalid binary descriptor", ErrIdentity)
	}
	digest, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(digest) != sha256.Size {
		return VerifiedBinary{}, fmt.Errorf("%w: invalid SHA-256", ErrIdentity)
	}
	expectedSHA256 = strings.ToLower(expectedSHA256)
	f, id, canonical, err := openImage(path)
	if err != nil {
		return VerifiedBinary{}, err
	}
	defer f.Close()
	if err = checkDigest(context.Background(), f, expectedSHA256); err != nil {
		return VerifiedBinary{}, err
	}
	return VerifiedBinary{path: canonical, digest: expectedSHA256, component: component, binding: binding, file: id}, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}
func checkDigest(ctx context.Context, f *os.File, expected string) error {
	h := sha256.New()
	if _, err := io.Copy(h, contextReader{ctx, f}); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != expected {
		return ErrIdentity
	}
	return ctx.Err()
}

// Options is for trusted adapter code only: Args must come from a fixed template,
// not HTTP input. There is no shell, environment override, PID/port attach, or
// working-directory override. Each stream has a total byte budget; BufferBytes
// caps EACH unread stdout/stderr queue (1 byte through 4 MiB). A slow/non-reading
// adapter fails closed rather than blocking an unbounded output goroutine.
// Stdin is synchronous (OS pipe backpressure); cancel/Close unblocks writers.
type Options struct {
	Args                                          []string
	MaxStdinBytes, MaxStdoutBytes, MaxStderrBytes int64
	BufferBytes                                   int
}

func (o Options) valid() bool {
	if o.MaxStdinBytes <= 0 || o.MaxStdoutBytes <= 0 || o.MaxStderrBytes <= 0 || o.BufferBytes <= 0 || o.BufferBytes > 4<<20 || len(o.Args) > 256 {
		return false
	}
	n := 0
	for _, a := range o.Args {
		if strings.ContainsRune(a, 0) {
			return false
		}
		n += len(a) + 1
	}
	return n <= 24000
}

// ExitError contains only the exit code, never child output or command arguments.
type ExitError struct{ Code uint32 }

func (e *ExitError) Error() string { return fmt.Sprintf("runtimeprocess: exit code %d", e.Code) }

// Identity describes an actual created process, not a caller-supplied PID.
// CreationTime is the Windows creation FILETIME (100 ns ticks since 1601).
type Identity struct {
	PID                                   uint32
	CreationTime                          uint64
	ImagePath, SHA256, Component, Binding string
}

// Supervisor's zero value is ready to use. It never adopts an external process
// and never assigns the center itself to a Job. Close rejects starts, terminates
// only its own Jobs, and waits for every owned root process and stream drain.
type Supervisor struct {
	mu        sync.Mutex
	closed    bool
	processes map[*Process]struct{}
}

// Start verifies before creating, then atomically creates the suspended process
// INSIDE its own non-inheritable kill-on-close Job. It checks the actual process
// identity before resuming the original thread. Every post-creation failure kills
// and waits. Platforms without atomic job-list creation fail closed, no fallback.
func (s *Supervisor) Start(ctx context.Context, b VerifiedBinary, o Options) (*Process, error) {
	return s.start(ctx, b, o)
}

func (s *Supervisor) start(ctx context.Context, b VerifiedBinary, o Options) (*Process, error) {
	if ctx == nil {
		return nil, errors.New("runtimeprocess: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !o.valid() {
		return nil, errors.New("runtimeprocess: invalid stream limits or arguments")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if b.path == "" || b.digest == "" {
		return nil, ErrIdentity
	}
	f, id, _, err := openImage(b.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if id != b.file {
		return nil, ErrIdentity
	}
	if err = checkDigest(ctx, f, b.digest); err != nil {
		return nil, err
	}
	p := &Process{supervisor: s, binary: b, done: make(chan struct{})}
	p.stdout = newOutput(o.BufferBytes, p.stop)
	p.stderr = newOutput(o.BufferBytes, p.stop)
	native, in, out, diag, identity, err := launch(ctx, b, o)
	if err != nil {
		return nil, err
	}
	p.native = native
	p.identity = identity
	p.stdin = &input{file: in, max: o.MaxStdinBytes, stop: p.stop}
	if s.processes == nil {
		s.processes = make(map[*Process]struct{})
	}
	s.processes[p] = struct{}{}
	p.drains.Add(2)
	go p.drain(out, p.stdout, o.MaxStdoutBytes)
	go p.drain(diag, p.stderr, o.MaxStderrBytes)
	go p.reap()
	go func() {
		select {
		case <-ctx.Done():
			p.stop(ctx.Err())
		case <-p.done:
		}
	}()
	return p, nil
}
func (s *Supervisor) Close() error {
	s.mu.Lock()
	s.closed = true
	ps := make([]*Process, 0, len(s.processes))
	for p := range s.processes {
		ps = append(ps, p)
	}
	s.mu.Unlock()
	for _, p := range ps {
		p.stop(ErrClosed)
	}
	var err error
	for _, p := range ps {
		<-p.done
		err = errors.Join(err, p.cleanupErr)
	}
	return err
}

// Process exposes only owned pipes and read-only identity. Wait is repeatable
// and concurrency safe. Close is idempotent, kills the whole owned tree and
// waits; it reports cleanup errors, not the expected cancellation exit status.
// Wait returns context/limit/Close cause or a nonzero exit error. It also closes
// the Job on normal root exit, so descendants cannot outlive the root.
type Process struct {
	supervisor                     *Supervisor
	binary                         VerifiedBinary
	identity                       Identity
	native                         *nativeProcess
	stdin                          *input
	stdout, stderr                 *output
	mu                             sync.Mutex
	stopped                        bool
	cause, errorResult, cleanupErr error
	done                           chan struct{}
	drains                         sync.WaitGroup
}

func (p *Process) Stdin() io.WriteCloser { return p.stdin }
func (p *Process) Stdout() io.ReadCloser { return p.stdout }
func (p *Process) Stderr() io.ReadCloser { return p.stderr }
func (p *Process) Identity() Identity    { return p.identity }

// Path exposes the verified canonical path to trusted adapters.
func (b VerifiedBinary) Path() string { return b.path }

// CheckIdentity is the reuse gate: checks frozen component/binding/file digest,
// live process handle, PID, creation time, image file identity and Job membership.
// It does not discover/attach by PID or port; protocol readiness is adapter-owned.
func (p *Process) CheckIdentity(b VerifiedBinary) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped || b != p.binary {
		return ErrIdentity
	}
	return p.native.check(p.identity, p.binary)
}
func (p *Process) Wait() error  { <-p.done; return p.errorResult }
func (p *Process) Close() error { p.stop(ErrClosed); <-p.done; return p.cleanupErr }
func (p *Process) stop(cause error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked(cause)
}
func (p *Process) stopLocked(cause error) {
	if p.stopped {
		return
	}
	p.stopped = true
	p.cause = cause
	// The Job handle is NEVER inherited. Closing it kills all descendants even
	// if the root has already exited or spawned children with inherited stdio.
	p.cleanupErr = p.native.closeJob()
	_ = p.stdin.Close()
}
func (p *Process) reap() {
	err := p.native.wait()
	p.mu.Lock()
	p.stopLocked(nil)
	p.cleanupErr = errors.Join(p.cleanupErr, p.native.closeProcess())
	p.mu.Unlock()
	p.drains.Wait()
	p.mu.Lock()
	p.errorResult = errors.Join(p.cause, err, p.cleanupErr)
	p.mu.Unlock()
	p.supervisor.mu.Lock()
	delete(p.supervisor.processes, p)
	close(p.done)
	p.supervisor.mu.Unlock()
}

// A root may exit before its output is drained. A late stream violation must
// still fail Wait rather than being hidden by normal root-exit cleanup.
func (p *Process) failStream(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cause == nil {
		p.cause = err
	}
	p.stopLocked(err)
}
func (p *Process) drain(f *os.File, out *output, max int64) {
	defer p.drains.Done()
	defer f.Close()
	var terminal error
	defer func() { out.finish(terminal) }()
	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, err := f.Read(buf)
		if n > 0 {
			keep := n
			if int64(keep) > max-total {
				keep = int(max - total)
			}
			if e := out.push(buf[:keep]); e != nil {
				terminal = e
				p.failStream(e)
				return
			}
			total += int64(keep)
			if keep < n {
				terminal = ErrOutputLimit
				p.failStream(terminal)
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				terminal = err
				p.failStream(err)
			}
			return
		}
	}
}

type input struct {
	mu         sync.Mutex
	file       *os.File
	max, total int64
	stop       func(error)
}

func (w *input) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if int64(len(b)) > w.max-w.total {
		w.stop(ErrInputLimit)
		return 0, ErrInputLimit
	}
	n, err := w.file.Write(b)
	w.total += int64(n)
	return n, err
}

// No writer mutex: closing the OS pipe must unblock a concurrent blocked Write.
func (w *input) Close() error {
	err := w.file.Close()
	if errors.Is(err, os.ErrClosed) {
		return nil
	}
	return err
}

type output struct {
	mu         sync.Mutex
	ready      *sync.Cond
	buf        []byte
	head, size int
	ended      bool
	err        error
	stop       func(error)
}

func newOutput(capacity int, stop func(error)) *output {
	o := &output{buf: make([]byte, capacity), stop: stop}
	o.ready = sync.NewCond(&o.mu)
	return o
}
func (o *output) push(b []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ended {
		return ErrClosed
	}
	if len(b) > len(o.buf)-o.size {
		return ErrOutputLimit
	}
	start := (o.head + o.size) % len(o.buf)
	n := copy(o.buf[start:], b)
	copy(o.buf, b[n:])
	o.size += len(b)
	o.ready.Broadcast()
	return nil
}
func (o *output) finish(err error) {
	o.mu.Lock()
	if !o.ended {
		o.ended = true
		o.err = err
	}
	o.ready.Broadcast()
	o.mu.Unlock()
}
func (o *output) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for o.size == 0 && !o.ended {
		o.ready.Wait()
	}
	if o.size == 0 {
		if o.err != nil {
			return 0, o.err
		}
		return 0, io.EOF
	}
	n := len(b)
	if n > o.size {
		n = o.size
	}
	first := copy(b[:n], o.buf[o.head:])
	copy(b[first:n], o.buf[:n-first])
	o.head = (o.head + n) % len(o.buf)
	o.size -= n
	return n, nil
}
func (o *output) Close() error { o.finish(ErrClosed); o.stop(ErrClosed); return nil }
