package runtimeadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentclient"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeprocess"
)

type shimSession struct {
	client  *componentclient.Client
	proc    *runtimeprocess.Process
	dir     string
	closeFn func() error
}

func (s *shimSession) Close() error {
	var err error
	if s.closeFn != nil {
		err = s.closeFn()
	}
	if s.proc != nil {
		err = errors.Join(err, s.proc.Close())
	}
	if s.dir != "" {
		err = errors.Join(err, os.RemoveAll(s.dir))
	}
	return err
}

// shimLauncher is replaceable only by package tests; production always starts the
// delivered shim binary next to the center.
type shimLauncher func(ctx context.Context, mode, target, frontal, profile string) (*shimSession, error)

func (e *Executor) newShimSession(ctx context.Context, mode, target, frontal, profile string) (*shimSession, error) {
	if e.launchShim != nil {
		return e.launchShim(ctx, mode, target, frontal, profile)
	}
	return e.launchShimProcess(ctx, mode, target, frontal, profile)
}

func (e *Executor) launchShimProcess(ctx context.Context, mode, target, frontal, profile string) (*shimSession, error) {
	if e.shimPath == "" {
		return nil, errors.New("center_shim_missing")
	}
	var address string
	for attempt := 0; attempt < 3; attempt++ {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		address = listener.Addr().String()
		_ = listener.Close()
		break
	}
	dir, err := os.MkdirTemp(e.scratch, "shim-")
	if err != nil {
		return nil, err
	}
	args := []string{"--mode", mode, "--listen", address, "--work-dir", dir}
	if target != "" {
		args = append(args, "--target", target)
	}
	if frontal != "" {
		args = append(args, "--haar-frontal", frontal)
	}
	if profile != "" {
		args = append(args, "--haar-profile", profile)
	}
	digest, err := digestFile(e.shimPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, errors.New("center_shim_missing")
	}
	b, err := runtimeprocess.VerifyBinary(e.shimPath, digest, "shim/"+mode, "center/local")
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, errors.New("center_shim_invalid")
	}
	proc, err := e.supervisor.Start(ctx, b, runtimeprocess.Options{
		Args:           args,
		MaxStdinBytes:  1,
		MaxStdoutBytes: 1 << 20,
		MaxStderrBytes: 1 << 20,
		BufferBytes:    64 << 10,
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	client, err := componentclient.New("http://" + address)
	if err != nil {
		_ = proc.Close()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	healthCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.WaitHealthy(healthCtx); err != nil {
		_ = proc.Close()
		_ = os.RemoveAll(dir)
		return nil, errors.New("center_shim_unavailable")
	}
	return &shimSession{client: client, proc: proc, dir: dir}, nil
}

func digestFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("center_shim_missing")
	}
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

func componentEntryPath(r Resolved) (string, error) {
	if !filepath.IsAbs(r.ComponentRoot) || r.Manifest.Entry == "" {
		return "", errors.New("center_component_invalid")
	}
	p := filepath.Join(r.ComponentRoot, filepath.FromSlash(r.Manifest.Entry))
	rel, err := filepath.Rel(r.ComponentRoot, p)
	if err != nil || rel == ".." || filepath.IsAbs(rel) {
		return "", errors.New("center_component_invalid")
	}
	return p, nil
}
