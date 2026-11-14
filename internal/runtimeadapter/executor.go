package runtimeadapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/bindings"
	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeprocess"
)

type ModelFile struct {
	ModelID string
	Path    string
}
type Resolved struct {
	Binding       bindings.Binding
	Manifest      components.Manifest
	ComponentRoot string
	Entry         runtimeprocess.VerifiedBinary
	Models        []ModelFile
	Release       func()
}
type Resolver func(context.Context, integration.ExecutionBindingRef, integration.ExecutionComponentRef) (Resolved, error)
type BatchFallbackResolver func(context.Context, []ModelFile) (Resolved, error)

type Executor struct {
	resolve       Resolver
	batchFallback BatchFallbackResolver
	assets        *runtimeassets.Store
	scratch       string
	supervisor    runtimeprocess.Supervisor
	shimPath      string
	launchShim    shimLauncher
	mu            sync.Mutex
	closed        bool
	llama         *llamaRuntime
	heavy         chan struct{}
	lifetime      context.Context
	cancel        context.CancelFunc
	active        sync.WaitGroup
	timers        sync.WaitGroup
	closeDone     chan struct{}
	closeErr      error
	// Test clock only; production uses a stoppable timer.
	llamaAfter func(time.Duration) <-chan time.Time
}

func New(resolve Resolver, assets *runtimeassets.Store, scratch string) (*Executor, error) {
	if resolve == nil || assets == nil {
		return nil, errors.New("center_adapter_invalid")
	}
	dir, err := os.MkdirTemp(scratch, "runtime-adapter-")
	if err != nil {
		return nil, err
	}
	e := &Executor{resolve: resolve, assets: assets, scratch: dir}
	if exe, err := os.Executable(); err == nil {
		e.shimPath = filepath.Join(filepath.Dir(exe), "fengshen-shim.exe")
	}
	return e, nil
}

// SetWhisperBatchFallback installs the episode-scoped recovery resolver. It
// must be set before the HTTP server starts; it is not changed while serving.
func (e *Executor) SetWhisperBatchFallback(resolver BatchFallbackResolver) {
	e.batchFallback = resolver
}

func (e *Executor) initLocked() {
	if e.lifetime == nil {
		e.lifetime, e.cancel = context.WithCancel(context.Background())
		e.heavy = make(chan struct{}, 1)
		e.closeDone = make(chan struct{})
		e.llama = &llamaRuntime{}
	}
}

func (e *Executor) enter(ctx context.Context) (context.Context, func(), error) {
	e.mu.Lock()
	e.initLocked()
	if e.closed {
		e.mu.Unlock()
		return nil, nil, errors.New("center_closed")
	}
	e.active.Add(1)
	life := e.lifetime
	e.mu.Unlock()
	run, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(life, cancel)
	return run, func() { stop(); cancel(); e.active.Done() }, nil
}

func (e *Executor) lockHeavy(ctx context.Context) error {
	select {
	case e.heavy <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-e.heavy
		return err
	}
	return nil
}

func (e *Executor) Execute(ctx context.Context, req runtimejobs.Request, emit runtimejobs.Emit) (runtimejobs.Result, error) {
	ctx, done, err := e.enter(ctx)
	if err != nil {
		return runtimejobs.Result{}, err
	}
	defer done()
	score := req.Binding.Scene == "highlight.score" || req.Binding.Scene == "highlight.vision"
	if req.Binding.Scene != "image.face" {
		if score {
			// Scores share one persistent llama-server via reference counting and
			// are admitted concurrently (<= ScoreSlots) by the job manager, so they
			// do not take the exclusive heavy gate. Writes still exclude scores:
			// the manager only dispatches a write once every score has drained.
			e.llama.stopIdle()
		} else {
			if err = e.lockHeavy(ctx); err != nil {
				return runtimejobs.Result{}, err
			}
			defer func() { <-e.heavy }()
			e.llama.stopIdle()
			e.llama.unload()
		}
	}
	resolved, err := e.resolve(ctx, req.Binding, req.Body.Component)
	if err != nil {
		if score {
			e.llama.unload()
		}
		return runtimejobs.Result{}, err
	}
	// scoreLocked owns the resolution on every path, including validation errors.
	release := resolved.Release
	if release != nil {
		defer func() {
			if release != nil {
				release()
			}
		}()
	}
	if resolved.Binding.Ref != req.Binding || resolved.Binding.Component != req.Body.Component {
		if score {
			e.llama.unload()
		}
		return runtimejobs.Result{}, errors.New("center_binding_stale")
	}
	switch req.Binding.Scene {
	case "highlight.transcribe":
		return e.transcribe(ctx, req, resolved, emit)
	case "highlight.score", "highlight.vision":
		release = nil
		return e.scoreLocked(ctx, req, resolved, emit)
	case "timbre.separate", "timbre.convert":
		return e.timbre(ctx, req, resolved, emit)
	case "image.face":
		return e.face(ctx, req, resolved, emit)
	default:
		return runtimejobs.Result{}, errors.New("center_protocol_unsupported")
	}
}

func (e *Executor) Close() error {
	e.mu.Lock()
	e.initLocked()
	if e.closed {
		done := e.closeDone
		e.mu.Unlock()
		<-done
		return e.closeErr
	}
	e.closed = true
	e.cancel()
	e.mu.Unlock()
	e.active.Wait()
	e.heavy <- struct{}{}
	e.llama.unload()
	<-e.heavy
	e.timers.Wait()
	err := errors.Join(e.supervisor.Close(), os.RemoveAll(e.scratch))
	e.mu.Lock()
	e.closeErr = err
	close(e.closeDone)
	e.mu.Unlock()
	return err
}
