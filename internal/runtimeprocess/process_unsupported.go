//go:build !windows

package runtimeprocess

import (
	"context"
	"os"
)

type nativeProcess struct{}

func openImage(string) (*os.File, fileIdentity, string, error) {
	return nil, fileIdentity{}, "", ErrUnsupported
}
func launch(context.Context, VerifiedBinary, Options, string) (*nativeProcess, *os.File, *os.File, *os.File, Identity, error) {
	return nil, nil, nil, nil, Identity{}, ErrUnsupported
}
func (*nativeProcess) check(Identity, VerifiedBinary) error { return ErrUnsupported }
func (*nativeProcess) closeJob() error                      { return nil }
func (*nativeProcess) closeProcess() error                  { return nil }
func (*nativeProcess) wait() error                          { return ErrUnsupported }
