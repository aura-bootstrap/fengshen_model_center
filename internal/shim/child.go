package shim

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeprocess"
)

type Limits struct {
	In, Out, Err int64
	Buffer       int
}

type Child interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() error
	Close() error
}

// StartChild spawns one already validated legacy component. The shim never
// accepts a caller supplied argument vector; callers build it from fixed
// templates in this package.
type StartChild func(ctx context.Context, target string, args []string, lim Limits) (Child, error)

func processLauncher() StartChild {
	return func(ctx context.Context, target string, args []string, lim Limits) (Child, error) {
		if !ordinaryFile(target) || !strings.EqualFold(filepath.Ext(target), ".exe") {
			return nil, errors.New("shim_target_invalid")
		}
		digest, err := fileDigest(target)
		if err != nil {
			return nil, err
		}
		b, err := runtimeprocess.VerifyBinary(target, digest, "shim/"+strings.ToLower(filepath.Base(target)), "shim/local")
		if err != nil {
			return nil, err
		}
		sup := &runtimeprocess.Supervisor{}
		p, err := sup.Start(ctx, b, runtimeprocess.Options{
			Args:           append([]string(nil), args...),
			MaxStdinBytes:  lim.In,
			MaxStdoutBytes: lim.Out,
			MaxStderrBytes: lim.Err,
			BufferBytes:    lim.Buffer,
		})
		if err != nil {
			_ = sup.Close()
			return nil, err
		}
		return &processChild{p: p, sup: sup}, nil
	}
}

type processChild struct {
	p   *runtimeprocess.Process
	sup *runtimeprocess.Supervisor
}

func (c *processChild) Stdin() io.WriteCloser { return c.p.Stdin() }
func (c *processChild) Stdout() io.Reader     { return c.p.Stdout() }
func (c *processChild) Stderr() io.Reader     { return c.p.Stderr() }
func (c *processChild) Wait() error           { return c.p.Wait() }
func (c *processChild) Close() error          { return errors.Join(c.p.Close(), c.sup.Close()) }

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
