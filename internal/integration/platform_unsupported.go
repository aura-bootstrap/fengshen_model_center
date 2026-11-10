//go:build !windows

package integration

import (
	"context"
	"io"
	"os/exec"
)

func platformSupported() error                               { return ErrUnsupported }
func platformCheckPath(string) error                         { return ErrUnsupported }
func queryProcess(uint32) (processIdentity, error)           { return processIdentity{}, ErrUnsupported }
func detach(*exec.Cmd)                                       {}
func startupLock(context.Context, string) (io.Closer, error) { return nil, ErrUnsupported }
