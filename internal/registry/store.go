// Package registry persists local registrations with optimistic transactions.
package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const MaxBytes = 256 * 1024

var ErrConflict = errors.New("registry_conflict: 登记版本已变化")
var ErrReadOnly = errors.New("registry_read_only: 登记存储只读")

type Registration struct {
	InputPath    string    `json:"input_path"`
	ResolvedPath string    `json:"resolved_path"`
	Kind         string    `json:"kind"`
	Identity     string    `json:"identity"`
	UpdatedAt    time.Time `json:"updated_at"`
}
type Snapshot struct {
	SchemaVersion int                     `json:"schema_version"`
	Revision      uint64                  `json:"revision"`
	Registrations map[string]Registration `json:"registrations"`
	Error         string                  `json:"-"`
}
type Store struct {
	mu             sync.Mutex
	path           string
	lock           io.Closer
	state          Snapshot
	fingerprint    [32]byte
	exists, closed bool
	readOnly       bool
	legacyPath     string
	legacyHash     [32]byte
	validate       func(string, string) (Registration, error)
	ops            fileOps
}
type fileOps struct {
	write   func(*os.File, []byte) error
	sync    func(*os.File) error
	close   func(*os.File) error
	replace func(string, string) error
}

func defaultOps() fileOps {
	return fileOps{func(f *os.File, b []byte) error {
		n, e := f.Write(b)
		if e == nil && n != len(b) {
			e = io.ErrShortWrite
		}
		return e
	}, (*os.File).Sync, (*os.File).Close, safefs.Replace}
}
func Open(dir string, validate func(id, path string) (Registration, error)) (*Store, error) {
	if validate == nil {
		return nil, errors.New("登记验证函数不能为空")
	}
	if e := safefs.SecureDir(dir); e != nil {
		return nil, e
	}
	l, e := safefs.Lock(filepath.Join(dir, "registry.lock"))
	if e != nil {
		return nil, e
	}
	s := &Store{path: filepath.Join(dir, "registrations.json"), lock: l, validate: validate, ops: defaultOps(), state: Snapshot{SchemaVersion: 1, Registrations: map[string]Registration{}}}
	b, exists, e := read(s.path)
	s.exists = exists
	s.fingerprint = sha256.Sum256(b)
	if e == nil && exists {
		s.state, e = decode(b, validate)
	} else if e == nil {
		legacy := filepath.Join(dir, "paths.json")
		var old []byte
		var found bool
		old, found, e = read(legacy)
		if e == nil && found {
			s.legacyPath = legacy
			s.legacyHash = sha256.Sum256(old)
			s.state, e = decodeLegacy(old, validate)
		}
	}
	if e != nil {
		if errors.Is(e, os.ErrPermission) {
			l.Close()
			return nil, e
		}
		s.readOnly = true
		s.state.Error = "registry_corrupt: " + e.Error()
	}
	return s, nil
}
func read(p string) ([]byte, bool, error) {
	st, e := os.Lstat(p)
	if os.IsNotExist(e) {
		return nil, false, nil
	}
	if e != nil {
		return nil, true, e
	}
	if !st.Mode().IsRegular() {
		return nil, true, errors.New("登记文件不是普通文件")
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, true, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if e == nil && len(b) > MaxBytes {
		e = errors.New("登记文件超过256 KiB")
	}
	return b, true, e
}

// Tokens reject duplicate object keys, including nested registration fields.
func unique(d *json.Decoder) error {
	t, e := d.Token()
	if e != nil {
		return e
	}
	if v, ok := t.(json.Delim); ok {
		switch v {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return errors.New("重复或非法JSON键")
				}
				seen[s] = true
				if e = unique(d); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e = unique(d); e != nil {
					return e
				}
			}
		default:
			return errors.New("非法JSON")
		}
		_, e = d.Token()
		return e
	}
	return nil
}
func decode(b []byte, validate func(string, string) (Registration, error)) (Snapshot, error) {
	s := Snapshot{SchemaVersion: 1, Registrations: map[string]Registration{}}
	d := json.NewDecoder(bytes.NewReader(b))
	if e := unique(d); e != nil {
		return s, e
	}
	if _, e := d.Token(); e != io.EOF {
		return s, errors.New("多余JSON内容")
	}
	var raw map[string]json.RawMessage
	if e := json.Unmarshal(b, &raw); e != nil || raw == nil {
		return s, errors.New("登记必须为JSON对象")
	}
	if _, ok := raw["schema_version"]; ok {
		for k := range raw {
			if k != "schema_version" && k != "revision" && k != "registrations" {
				return s, fmt.Errorf("未知登记字段: %s", k)
			}
		}
		var records map[string]map[string]json.RawMessage
		if e := json.Unmarshal(raw["registrations"], &records); e != nil {
			return s, e
		}
		for _, fields := range records {
			for k := range fields {
				switch k {
				case "input_path", "resolved_path", "kind", "identity", "updated_at":
				default:
					return s, fmt.Errorf("未知登记字段: %s", k)
				}
			}
		}
		d = json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if e := d.Decode(&s); e != nil {
			return s, e
		}
		if s.SchemaVersion != 1 || s.Registrations == nil || raw["revision"] == nil || bytes.Equal(bytes.TrimSpace(raw["revision"]), []byte("null")) || raw["registrations"] == nil {
			return s, errors.New("非法schema/version/revision")
		}
		for id, r := range s.Registrations {
			if !knownID(id) {
				return s, fmt.Errorf("未知模型ID: %s", id)
			}
			// Input-only records preserve unavailable legacy entries until explicit repair.
			inputOnly := r.ResolvedPath == "" && r.Kind == "" && r.Identity == "" && r.UpdatedAt.IsZero()
			if r.InputPath == "" || (!inputOnly && (r.ResolvedPath == "" || (r.Kind != "file" && r.Kind != "directory") || r.Identity == "" || r.UpdatedAt.IsZero())) {
				return s, fmt.Errorf("登记字段损坏: %s", id)
			}
		}
		s.Error = diagnostics(s.Registrations, validate)
		return s, nil
	}
	return decodeLegacy(b, validate)
}

// Keep this allowlist aligned with the stable model-center catalog, independently
// of validators whose errors can also mean a missing or inaccessible path.
func knownID(id string) bool {
	switch id {
	case "whisper-asr", "whisper-vad", "local-text", "local-vision", "demucs", "demucs-ft", "seed-vc", "seed-whisper", "campplus", "bigvgan", "haar-frontal", "haar-profile":
		return true
	}
	return false
}

func decodeLegacy(b []byte, validate func(string, string) (Registration, error)) (Snapshot, error) {
	s := Snapshot{SchemaVersion: 1, Registrations: map[string]Registration{}}
	d := json.NewDecoder(bytes.NewReader(b))
	if e := unique(d); e != nil {
		return s, e
	}
	if _, e := d.Token(); e != io.EOF {
		return s, errors.New("多余JSON内容")
	}
	var paths map[string]string
	if e := json.Unmarshal(b, &paths); e != nil || paths == nil {
		return s, errors.New("非法旧登记格式")
	}
	for id, p := range paths {
		if !knownID(id) || p == "" {
			return s, fmt.Errorf("非法旧登记或未知模型ID: %s", id)
		}
	}
	for id, p := range paths {
		r, e := validate(id, p)
		if e != nil {
			r = Registration{}
		} else if r.UpdatedAt.IsZero() {
			r.UpdatedAt = time.Now().UTC()
		}
		r.InputPath = p
		s.Registrations[id] = r
	}
	s.Error = diagnostics(s.Registrations, validate)
	return s, nil
}

func diagnostics(records map[string]Registration, validate func(string, string) (Registration, error)) string {
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var messages []string
	for _, id := range ids {
		r := records[id]
		current, e := validate(id, r.InputPath)
		if e != nil {
			messages = append(messages, fmt.Sprintf("registration_unavailable: %s: %v", id, e))
		} else if r.Identity == "" || current.Identity != r.Identity || current.ResolvedPath != r.ResolvedPath || current.Kind != r.Kind {
			messages = append(messages, "registration_changed: "+id+": 登记入口身份已变化，需重新登记")
		}
	}
	return strings.Join(messages, "; ")
}
func clone(s Snapshot) Snapshot {
	m := make(map[string]Registration, len(s.Registrations))
	for k, v := range s.Registrations {
		m[k] = v
	}
	s.Registrations = m
	return s
}
func (s *Store) Snapshot() Snapshot { s.mu.Lock(); defer s.mu.Unlock(); return clone(s.state) }
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lock.Close()
}
func (s *Store) Update(ctx context.Context, expected uint64, id, path string) (Snapshot, error) {
	return s.change(ctx, expected, id, path, false)
}
func (s *Store) Delete(ctx context.Context, expected uint64, id string) (Snapshot, error) {
	return s.change(ctx, expected, id, "", true)
}
func (s *Store) change(ctx context.Context, expected uint64, id, path string, del bool) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := func(e error) (Snapshot, error) { return clone(s.state), e }
	if s.closed {
		return out(os.ErrClosed)
	}
	if s.readOnly {
		return out(ErrReadOnly)
	}
	if !knownID(id) {
		return out(fmt.Errorf("model_unknown: 未知模型ID: %s", id))
	}
	if e := ctx.Err(); e != nil {
		return out(e)
	}
	if expected != s.state.Revision {
		return out(ErrConflict)
	}
	next := clone(s.state)
	if del {
		if _, ok := next.Registrations[id]; !ok {
			return out(errors.New("登记不存在"))
		}
		delete(next.Registrations, id)
	} else {
		r, e := s.validate(id, path)
		if e != nil {
			return out(e)
		}
		if r.InputPath == "" {
			r.InputPath = path
		}
		if r.UpdatedAt.IsZero() {
			r.UpdatedAt = time.Now().UTC()
		}
		next.Registrations[id] = r
	}
	if next.Revision == ^uint64(0) {
		return out(errors.New("revision溢出"))
	}
	next.Revision++
	next.SchemaVersion = 1
	b, e := json.MarshalIndent(next, "", "  ")
	if e != nil {
		return out(e)
	}
	if len(b) > MaxBytes {
		return out(errors.New("登记超过256 KiB"))
	}
	check := func() error {
		old, exists, e := read(s.path)
		changed := e != nil || exists != s.exists || sha256.Sum256(old) != s.fingerprint
		if s.legacyPath != "" {
			legacy, found, err := read(s.legacyPath)
			changed = changed || err != nil || !found || sha256.Sum256(legacy) != s.legacyHash
		}
		if changed {
			s.readOnly = true
			s.state.Error = "registry_external_change: 磁盘登记已被外部改写，禁止覆盖"
			return ErrReadOnly
		}
		return nil
	}
	if e = check(); e != nil {
		return out(e)
	}
	f, e := os.CreateTemp(filepath.Dir(s.path), ".registry-*")
	if e != nil {
		return out(e)
	}
	temp := f.Name()
	defer os.Remove(temp)
	if e = s.ops.write(f, b); e == nil {
		e = s.ops.sync(f)
	}
	ce := s.ops.close(f)
	if e == nil {
		e = ce
	}
	if e != nil {
		return out(fmt.Errorf("registry_write_failed: %w", e))
	}
	if e = ctx.Err(); e != nil {
		return out(e)
	}
	if e = check(); e != nil {
		return out(e)
	}
	if e = s.ops.replace(temp, s.path); e != nil {
		s.readOnly = true
		s.state.Error = "registry_commit_uncertain: " + e.Error()
		return out(fmt.Errorf("registry_commit_uncertain: %w", e))
	}
	committed, exists, e := read(s.path)
	if e != nil || !exists || !bytes.Equal(committed, b) {
		s.readOnly = true
		s.state.Error = "registry_commit_uncertain: 无法确认提交内容"
		return out(ErrReadOnly)
	}
	next.Error = diagnostics(next.Registrations, s.validate)
	s.state = next
	s.legacyPath = ""
	s.exists = true
	s.fingerprint = sha256.Sum256(b)
	return out(nil)
}
