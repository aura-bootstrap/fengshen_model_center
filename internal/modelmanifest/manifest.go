// Package modelmanifest owns the single user-level model manifest. It replaces
// the previous per-model registration file and installed resource layout.
package modelmanifest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	MaxBytes   = 4 << 20
	MaxEntries = 100000
)

type Entry struct {
	ModelID string `json:"model_id"`
	Role    string `json:"role,omitempty"`
	Path    string `json:"path,omitempty"`
	NewPath string `json:"new_path,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

func (e Entry) FilePath() string {
	if e.Path != "" {
		return e.Path
	}
	return e.NewPath
}

type Manifest struct {
	Schema      int     `json:"schema"`
	GeneratedAt string  `json:"generated_at,omitempty"`
	Root        string  `json:"root,omitempty"`
	Entries     []Entry `json:"entries"`
}

func DefaultPath(home string) string {
	return filepath.Join(home, ".fengshen", "model_manifest.json")
}

func Load(path string) (Manifest, []byte, error) {
	var m Manifest
	info, err := os.Lstat(path)
	if err != nil {
		return m, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > MaxBytes {
		return m, nil, errors.New("model_manifest_invalid: 文件不是普通文件或超出大小限制")
	}
	f, err := os.Open(path)
	if err != nil {
		return m, nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return m, nil, err
	}
	if len(raw) > MaxBytes {
		return m, nil, errors.New("model_manifest_invalid: 超出大小限制")
	}
	if err = json.Unmarshal(raw, &m); err != nil {
		return m, raw, fmt.Errorf("model_manifest_invalid: %w", err)
	}
	if m.Schema != 1 || len(m.Entries) == 0 || len(m.Entries) > MaxEntries {
		return m, raw, errors.New("model_manifest_invalid: schema 或条目数量无效")
	}
	for i, e := range m.Entries {
		p := e.FilePath()
		if !validID(e.ModelID) || p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p || e.Bytes < 0 {
			return m, raw, fmt.Errorf("model_manifest_invalid: 第 %d 条无效", i+1)
		}
		if e.SHA256 != "" {
			if b, err := hex.DecodeString(e.SHA256); err != nil || len(b) != sha256.Size {
				return m, raw, fmt.Errorf("model_manifest_invalid: 第 %d 条摘要无效", i+1)
			}
		}
	}
	return m, raw, nil
}

func Revision(raw []byte) uint64 {
	sum := sha256.Sum256(raw)
	v := binary.BigEndian.Uint64(sum[:8])
	if v == 0 {
		v = 1
	}
	return v
}

func Write(path string, m Manifest) error {
	if m.Schema != 1 {
		return errors.New("model_manifest_invalid: schema")
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > MaxBytes {
		return errors.New("model_manifest_invalid: 超出大小限制")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".model-manifest-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func validID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// SHA256ByPath indexes declared hashes from a hub manifest for reuse.
func SHA256ByPath(m Manifest) map[string]string {
	out := map[string]string{}
	for _, e := range m.Entries {
		p := e.FilePath()
		if p == "" || e.SHA256 == "" {
			continue
		}
		out[strings.ToLower(filepath.Clean(p))] = strings.ToLower(e.SHA256)
	}
	return out
}