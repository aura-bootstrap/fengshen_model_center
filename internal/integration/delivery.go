package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

const centerRelative = "model-center/fengshen-model-center.exe"
const deliveryLimit = 256 * 1024

type deliveryFile struct {
	Path   string `json:"path"`
	Bytes  string `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type deliveryManifest struct {
	Schema           int            `json:"schema"`
	Files            []deliveryFile `json:"files"`
	Inference        string         `json:"inference"`
	CleanEnvironment string         `json:"clean_environment"`
}
type fileStamp struct {
	ID             string
	Size, Modified int64
	Directory      bool
}

func stamp(path string) (fileStamp, error) {
	if e := platformCheckPath(path); e != nil {
		return fileStamp{}, e
	}
	f, e := os.Open(path)
	if e != nil {
		return fileStamp{}, e
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil {
		return fileStamp{}, e
	}
	if !s.IsDir() && !s.Mode().IsRegular() {
		return fileStamp{}, errors.New("center_file_invalid")
	}
	id, e := safefs.Identity(f)
	return fileStamp{id, s.Size(), s.ModTime().UnixNano(), s.IsDir()}, e
}
func stampsCurrent(files map[string]fileStamp) bool {
	if len(files) == 0 {
		return false
	}
	for p, expected := range files {
		got, e := stamp(p)
		if e != nil || got != expected {
			return false
		}
	}
	return true
}

// Reject duplicate JSON keys as well as unknown fields and trailing values.
func strictDeliveryJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	var value func() error
	value = func() error {
		t, e := d.Token()
		if e != nil {
			return e
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return errors.New("delivery_duplicate_key")
				}
				seen[key] = true
				if e = value(); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := value(); e != nil {
					return e
				}
			}
		default:
			return errors.New("delivery_json_invalid")
		}
		_, e = d.Token()
		return e
	}
	if e := value(); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("delivery_trailing_data")
	}
	return decode(bytes.NewReader(b), deliveryLimit, v)
}

// Hash and parse the very same bounded bytes; never parse an unchecked reopen.
func trustedJSON(ctx context.Context, path, digest string) ([]byte, error) {
	before, e := stamp(path)
	if e != nil {
		return nil, e
	}
	if before.Directory || before.Size > deliveryLimit {
		return nil, errors.New("delivery_file_invalid")
	}
	if e = ctx.Err(); e != nil {
		return nil, e
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	id, e := safefs.Identity(f)
	if e != nil || id != before.ID {
		f.Close()
		return nil, errors.New("center_file_changed")
	}
	b, e := io.ReadAll(io.LimitReader(f, deliveryLimit+1))
	end, se := f.Stat()
	f.Close()
	if e != nil {
		return nil, e
	}
	if se != nil || end.Size() != before.Size || end.ModTime().UnixNano() != before.Modified {
		return nil, errors.New("center_file_changed")
	}
	after, e := stamp(path)
	if e != nil || before != after || int64(len(b)) != before.Size {
		return nil, errors.New("center_file_changed")
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, errors.New("center_hash_mismatch")
	}
	return b, ctx.Err()
}

func (c *Client) verifyDelivery(ctx context.Context) (map[string]fileStamp, error) {
	files := map[string]fileStamp{}
	remember := func(path string) error {
		s, e := stamp(path)
		if e == nil {
			files[path] = s
		}
		return e
	}
	if e := remember(c.delivery()); e != nil {
		return nil, e
	}
	b, e := trustedJSON(ctx, c.delivery(), c.cfg.DeliverySHA256)
	if e != nil {
		return nil, e
	}
	var d deliveryManifest
	if e = strictDeliveryJSON(b, &d); e != nil {
		return nil, e
	}
	if d.Schema != 1 || d.Inference != "not_tested" || d.CleanEnvironment != "not_verified" || len(d.Files) == 0 {
		return nil, errors.New("delivery_schema_invalid")
	}
	seen := map[string]bool{}
	centerFound := false
	for _, item := range d.Files {
		key := strings.ToLower(item.Path)
		if !components.SafeRelative(item.Path) || seen[key] || key == "model-center/delivery.json" {
			return nil, errors.New("delivery_path_invalid")
		}
		seen[key] = true
		size, e := strconv.ParseInt(item.Bytes, 10, 64)
		if e != nil || size < 0 || strconv.FormatInt(size, 10) != item.Bytes || !digestValid(item.SHA256) {
			return nil, errors.New("delivery_file_invalid")
		}
		path := filepath.Join(c.cfg.InstallRoot, filepath.FromSlash(item.Path))
		if e = remember(path); e != nil {
			return nil, e
		}
		if files[path].Directory || files[path].Size != size {
			return nil, errors.New("delivery_size_mismatch")
		}
		if item.Path == centerRelative {
			centerFound = true
			if item.SHA256 != c.cfg.CenterSHA256 {
				return nil, errors.New("delivery_center_mismatch")
			}
		}
		e = hashFile(ctx, path, item.SHA256, size)
		if e != nil {
			return nil, e
		}
	}
	if !centerFound {
		return nil, errors.New("delivery_required_file_missing")
	}
	if !stampsCurrent(files) {
		return nil, errors.New("center_file_changed")
	}
	return files, nil
}
