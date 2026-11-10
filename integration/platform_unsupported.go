//go:build !windows

package integration

import (
	"context"
	"io"
	"os/exec"
)

func currentSID() (string, error)                                     { return "", ErrUnsupported }
func pipePeer(io.ReadWriter) (processIdentity, error)                 { return processIdentity{}, ErrUnsupported }
func platformSupported() error                                        { return ErrUnsupported }
func platformCheckPath(string) error                                  { return ErrUnsupported }
func pipeName(string) (string, error)                                 { return "", ErrUnsupported }
func queryProcess(uint32) (processIdentity, error)                    { return processIdentity{}, ErrUnsupported }
func detach(*exec.Cmd)                                                {}
func startupLock(context.Context, string) (io.Closer, error)          { return nil, ErrUnsupported }
func listenPipe(string) (io.Closer, error)                            { return nil, ErrUnsupported }
func servePipe(context.Context, io.Closer, func(io.ReadWriter)) error { return ErrUnsupported }
func dialPipe(context.Context, string) (io.ReadWriteCloser, uint32, error) {
	return nil, 0, ErrUnsupported
}
