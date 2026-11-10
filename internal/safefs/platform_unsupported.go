//go:build !windows && !linux

package safefs

import (
	"errors"
	"io"
	"os"
)

var unsupported = errors.New("当前平台不支持所需文件安全能力")

func localPath(string) error                                      { return unsupported }
func finalPath(*os.File) (string, error)                          { return "", unsupported }
func Identity(*os.File) (string, error)                           { return "", unsupported }
func reparse(*os.Root, string, os.FileInfo) (string, error)       { return "", unsupported }
func childIdentity(*os.Root, string, os.FileInfo) (string, error) { return "", unsupported }
func SecureDir(string) error                                      { return unsupported }
func Lock(string) (io.Closer, error)                              { return nil, unsupported }
func Replace(string, string) error                                { return unsupported }
func PersistParent(string) error                                  { return unsupported }
