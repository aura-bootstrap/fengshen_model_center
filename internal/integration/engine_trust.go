package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

const engineTrustRelative = "model-center/engine-trust.json"
const engineTrustLimit = 64 * 1024

type engineTrustDocument struct {
	Schema         int             `json:"schema"`
	CenterSHA256   string          `json:"center_sha256"`
	DeliverySHA256 string          `json:"delivery_sha256"`
	Engines        []TrustedEngine `json:"engines"`
}

// Open each root component without allowing a root alias or reparse point.
func engineTrustRoot(root string) (*safefs.Scope, error) {
	if e := safefs.ValidatePath(root); e != nil {
		return nil, e
	}
	if runtime.GOOS == "windows" {
		if e := platformCheckPath(root); e != nil {
			return nil, e
		}
	}
	root = filepath.Clean(root)
	var parts []string
	p := root
	for filepath.Dir(p) != p {
		parts = append(parts, filepath.Base(p))
		p = filepath.Dir(p)
	}
	first := filepath.Join(p, parts[len(parts)-1])
	st, e := os.Lstat(first)
	if e != nil {
		return nil, e
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, safefs.ErrBoundary
	}
	s, e := safefs.Open(first)
	if e != nil {
		return nil, e
	}
	if !strings.EqualFold(s.Path(), first) {
		s.Close()
		return nil, safefs.ErrBoundary
	}
	// Derive checks all remaining components, including unknown reparse tags.
	for i := len(parts) - 2; i >= 0; i-- {
		next, e := s.Derive(parts[i])
		s.Close()
		if e != nil {
			return nil, e
		}
		s = next
	}
	return s, nil
}

func engineTrustJSON(b []byte, out any) error {
	if len(b) > engineTrustLimit || !utf8.Valid(b) {
		return errors.New("engine_trust_json_invalid")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	for {
		t, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if t == nil {
			return errors.New("engine_trust_null")
		}
	}
	// Exact field spellings: encoding/json otherwise accepts case aliases.
	var value any
	if e := json.Unmarshal(b, &value); e != nil {
		return e
	}
	var check func(any) error
	check = func(v any) error {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if k != strings.ToLower(k) {
					return errors.New("engine_trust_field_case")
				}
				if e := check(x); e != nil {
					return e
				}
			}
		case []any:
			for _, x := range v {
				if e := check(x); e != nil {
					return e
				}
			}
		}
		return nil
	}
	if e := check(value); e != nil {
		return e
	}
	object, ok := value.(map[string]any)
	if !ok || len(object) != 4 {
		return errors.New("engine_trust_fields_required")
	}
	entries, ok := object["engines"].([]any)
	if !ok {
		return errors.New("engine_trust_engines_required")
	}
	for _, v := range entries {
		entry, ok := v.(map[string]any)
		if !ok || len(entry) != 4 || entry["path"] == nil || entry["sha256"] == nil || entry["edition"] == nil || entry["management"] == nil {
			return errors.New("engine_trust_entry_fields_required")
		}
	}
	return strictSessionJSON(b, out)
}

func engineTrustEntry(v TrustedEngine) bool {
	return components.SafeRelative(v.Path) && strings.EqualFold(filepath.Ext(v.Path), ".exe") && digestValid(v.SHA256) && (v.Edition == "user" || v.Edition == "admin" || v.Edition == "developer") && !(v.Edition == "user" && v.Management)
}

// LoadEngineTrust loads only fixed post-build metadata. Expected digests must
// come from trusted launch configuration, never HTTP or process discovery.
// Engine bytes are hashed later by admitSession, not by this passive loader.
func LoadEngineTrust(ctx context.Context, installRoot, expectedCenterSHA, expectedDeliverySHA string) ([]TrustedEngine, error) {
	if !digestValid(expectedCenterSHA) || !digestValid(expectedDeliverySHA) {
		return nil, errors.New("engine_trust_expected_digest_invalid")
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	s, e := engineTrustRoot(installRoot)
	if e != nil {
		return nil, e
	}
	defer s.Close()
	leaf, e := s.Derive(engineTrustRelative)
	if e != nil {
		return nil, e
	}
	defer leaf.Close()
	f, a, e := leaf.OpenFile("engine-trust.json")
	if e != nil {
		return nil, e
	}
	if a.Size > engineTrustLimit {
		f.Close()
		return nil, errors.New("engine_trust_too_large")
	}
	b, e := io.ReadAll(io.LimitReader(f, engineTrustLimit+1))
	st, se := f.Stat()
	f.Close()
	if e != nil {
		return nil, e
	}
	if se != nil || st.Size() != a.Size || !st.ModTime().Equal(a.Modified) || int64(len(b)) != a.Size {
		return nil, errors.New("engine_trust_changed")
	}
	var doc engineTrustDocument
	if e = engineTrustJSON(b, &doc); e != nil {
		return nil, e
	}
	if doc.Schema != 1 || doc.CenterSHA256 != expectedCenterSHA || doc.DeliverySHA256 != expectedDeliverySHA || len(doc.Engines) < 1 || len(doc.Engines) > 32 {
		return nil, errors.New("engine_trust_binding_invalid")
	}
	seen := map[string]bool{}
	type observed struct {
		rel   string
		entry safefs.Entry
	}
	observedFiles := []observed{{engineTrustRelative, a}}
	out := make([]TrustedEngine, 0, len(doc.Engines))
	for _, v := range doc.Engines {
		if e = ctx.Err(); e != nil {
			return nil, e
		}
		key := strings.ToLower(v.Path)
		if !engineTrustEntry(v) || seen[key] {
			return nil, errors.New("engine_trust_entry_invalid")
		}
		seen[key] = true
		child, e := s.Derive(v.Path)
		if e != nil {
			return nil, errors.New("engine_trust_engine_invalid")
		}
		f, entry, e := child.OpenFile(filepath.Base(v.Path))
		if f != nil {
			f.Close()
		}
		child.Close()
		if e != nil {
			return nil, errors.New("engine_trust_engine_invalid")
		}
		if entry.Size <= 0 {
			return nil, errors.New("engine_trust_engine_empty")
		}
		observedFiles = append(observedFiles, observed{v.Path, entry})
		v.Path = entry.Path
		out = append(out, v)
	}
	for _, o := range observedFiles {
		child, e := s.Derive(o.rel)
		if e != nil {
			return nil, errors.New("engine_trust_changed")
		}
		f, now, e := child.OpenFile(filepath.Base(o.rel))
		if f != nil {
			f.Close()
		}
		child.Close()
		if e != nil || now.Identity != o.entry.Identity || now.Size != o.entry.Size || !now.Modified.Equal(o.entry.Modified) {
			return nil, errors.New("engine_trust_changed")
		}
	}
	return out, ctx.Err()
}
