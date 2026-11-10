package runtimeadapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http/httptest"
	"os/exec"
	"testing"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentclient"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/internal/shim"
)

// newShimTestExecutor wires the real shim handlers in-process while launching the
// existing fake component executables (the test binary's init contract) with
// stdio, so adapter tests still exercise HTTP/WS end to end.
func newShimTestExecutor(t *testing.T, store *runtimeassets.Store) *Executor {
	t.Helper()
	e := &Executor{assets: store, scratch: t.TempDir()}
	e.launchShim = inProcessShimLauncher(t)
	return e
}

func inProcessShimLauncher(t *testing.T) shimLauncher {
	t.Helper()
	return func(ctx context.Context, mode, target, frontal, profile string) (*shimSession, error) {
		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			t.Fatal(err)
		}
		token := hex.EncodeToString(tokenBytes)
		work := t.TempDir()
		srv, err := shim.New(shim.Config{
			Mode:       mode,
			Token:      token,
			Target:     target,
			WorkRoot:   work,
			FrontalXML: frontal,
			ProfileXML: profile,
			StartChild: execTestChild,
		})
		if err != nil {
			return nil, err
		}
		ts := httptest.NewServer(srv.Handler())
		client, err := componentclient.New(ts.URL, token)
		if err != nil {
			ts.Close()
			_ = srv.Close()
			return nil, err
		}
		return &shimSession{client: client, closeFn: func() error { ts.Close(); return srv.Close() }}, nil
	}
}

type execChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
}

func execTestChild(ctx context.Context, target string, args []string, lim shim.Limits) (shim.Child, error) {
	cmd := exec.CommandContext(ctx, target, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execChild{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

func (c *execChild) Stdin() io.WriteCloser { return c.stdin }
func (c *execChild) Stdout() io.Reader     { return c.stdout }
func (c *execChild) Stderr() io.Reader     { return c.stderr }
func (c *execChild) Wait() error           { return c.cmd.Wait() }
func (c *execChild) Close() error {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return nil
}
