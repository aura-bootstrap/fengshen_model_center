//go:build !windows

package runtimeprocess

import "os"

func PinFile(string) (*os.File, error)      { return nil, ErrUnsupported }
func PinDirectory(string) (*os.File, error) { return nil, ErrUnsupported }
