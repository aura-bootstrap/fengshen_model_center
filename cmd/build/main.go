// Command build is an explicit, offline manifest/delivery generator. It is not
// linked into the model center and never downloads tools or executes inference.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

type LockedInput struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Version  string `json:"version"`
	License  string `json:"license"`
	Identity string `json:"identity,omitempty"`
	Modified int64  `json:"modified,omitempty"`
}
type Lock struct {
	Schema       int           `json:"schema"`
	RootIdentity string        `json:"root_identity,omitempty"`
	Inputs       []LockedInput `json:"inputs"`
}

const maxLockBytes = 16 * 1024 * 1024

type Delivery struct {
	Schema           int               `json:"schema"`
	Files            []components.File `json:"files"`
	Inference        string            `json:"inference"`
	CleanEnvironment string            `json:"clean_environment"`
}

func main() {
	mode := flag.String("mode", "", "manifest, delivery, engine-trust, or verify-lock")
	centerSHA := flag.String("center-sha256", "", "trusted center digest for engine-trust")
	deliverySHA := flag.String("delivery-sha256", "", "trusted delivery digest for engine-trust")
	root := flag.String("root", "", "absolute controlled output/input root")
	template := flag.String("template", "", "manifest template or lock path")
	engines := flag.Bool("engines", false, "include all three engine input graphs")
	output := flag.String("output", "", "snapshot output directory")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var err error
	switch *mode {
	case "manifest":
		var m components.Manifest
		var b []byte
		b, err = os.ReadFile(*template)
		if err == nil {
			err = components.StrictJSON(b, &m)
		}
		if err == nil {
			err = generate(ctx, *root, m)
		}
	case "delivery":
		err = delivery(ctx, *root)
	case "engine-trust":
		err = engineTrust(ctx, *root, *template, *centerSHA, *deliverySHA)
	case "snapshot", "verify-snapshot":
		err = snapshot(ctx, *root, *output, *engines, *mode == "verify-snapshot")
	case "seal-lock":
		err = sealLock(ctx, *root, *template)
	case "verify-lock":
		_, err = verifyLock(ctx, *root, *template)
	default:
		err = fmt.Errorf("choose explicit -mode")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func hash(ctx context.Context, s *safefs.Scope, rel string) (components.File, error) {
	var out components.File
	f, a, err := s.OpenFile(rel)
	if err != nil {
		return out, err
	}
	defer f.Close()
	if a.LinkKind != "none" {
		return out, fmt.Errorf("links forbidden: %s", rel)
	}
	h := sha256.New()
	buf := make([]byte, 1024*1024)
	var n int64
	for {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		count, e := f.Read(buf)
		if count > 0 {
			h.Write(buf[:count])
			n += int64(count)
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return out, e
		}
	}
	st, err := f.Stat()
	if err != nil {
		return out, err
	}
	id, err := safefs.Identity(f)
	if err != nil {
		return out, err
	}
	g, after, err := s.OpenFile(rel)
	if g != nil {
		g.Close()
	}
	if err != nil || after.LinkKind != "none" || a.Identity != id || a.Identity != after.Identity || n != a.Size || st.Size() != n || after.Size != n || !st.ModTime().Equal(a.Modified) || !after.Modified.Equal(a.Modified) {
		return out, fmt.Errorf("file changed: %s", rel)
	}
	return components.File{Path: filepath.ToSlash(rel), Bytes: fmt.Sprint(n), SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
func writeNew(p string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	if len(b) > components.MaxManifestBytes {
		return fmt.Errorf("metadata exceeds 256 KiB")
	}
	f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	return closeErr
}
func generate(ctx context.Context, root string, m components.Manifest) error {
	ext := strings.ToLower(filepath.Ext(m.Entry))
	if ext != ".exe" && ext != ".dll" && ext != ".so" && ext != ".dylib" && !(m.OS != "windows" && ext == "") {
		return fmt.Errorf("entry must be a binary, not source or a development interpreter")
	}
	s, err := safefs.Open(root)
	if err != nil {
		return err
	}
	defer s.Close()
	var files []components.File
	err = s.Walk(ctx, safefs.Limits{MaxEntries: 100000, MaxDepth: 64}, func(a safefs.Entry) error {
		if a.Error != "" {
			return fmt.Errorf("unsafe/unreadable build input: %s", a.Relative)
		}
		if a.LinkKind != "none" {
			return fmt.Errorf("links forbidden")
		}
		if a.IsDir {
			return nil
		}
		rel := filepath.ToSlash(a.Relative)
		if !components.SafeRelative(rel) {
			return fmt.Errorf("unsafe relative file")
		}
		if rel == "manifest.json" {
			return fmt.Errorf("manifest already exists; use a new clean build directory")
		}
		if rel == m.StartupReportRef {
			return fmt.Errorf("startup report must be generated after manifest")
		}
		f, e := hash(ctx, s, a.Relative)
		if e == nil {
			files = append(files, f)
		}
		return e
	})
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	m.Files = files
	if err = components.Validate(m); err != nil {
		return err
	}
	return writeNew(filepath.Join(root, "manifest.json"), m)
}
func verifyLock(ctx context.Context, root, p string) (Lock, error) {
	var lock Lock
	b, err := os.ReadFile(p)
	if err != nil {
		return lock, err
	}
	if len(b) > maxLockBytes {
		return lock, fmt.Errorf("lock too large")
	}
	if err = components.StrictJSON(b, &lock); err != nil {
		return lock, err
	}
	if lock.Schema != 1 || len(lock.Inputs) == 0 {
		return lock, fmt.Errorf("empty/unsupported lock")
	}
	s, err := safefs.Open(root)
	if err != nil {
		return lock, err
	}
	defer s.Close()
	seen := map[string]bool{}
	for _, in := range lock.Inputs {
		key := strings.ToLower(in.Path)
		h, e := hex.DecodeString(in.SHA256)
		if !components.SafeRelative(in.Path) || seen[key] || len(h) != 32 || e != nil || in.Version == "" || in.Version == "latest" || in.License == "" {
			return lock, fmt.Errorf("unlocked input: %s", in.Path)
		}
		seen[key] = true
		f, e := hash(ctx, s, in.Path)
		if e != nil {
			return lock, e
		}
		if f.SHA256 != strings.ToLower(in.SHA256) {
			return lock, fmt.Errorf("input digest mismatch: %s", in.Path)
		}
		if in.Identity != "" {
			opened, a, e := s.OpenFile(in.Path)
			if opened != nil {
				opened.Close()
			}
			if e != nil || a.Identity != in.Identity || a.Modified.UnixNano() != in.Modified {
				return lock, fmt.Errorf("input moved/replaced: %s", in.Path)
			}
		}
	}
	if lock.RootIdentity != "" {
		f, e := os.Open(root)
		if e != nil {
			return lock, e
		}
		id, e := safefs.Identity(f)
		f.Close()
		if e != nil || id != lock.RootIdentity {
			return lock, fmt.Errorf("source root moved/replaced")
		}
	}
	return lock, nil
}

// sealLock adds stable file/root identities to the already hashed input set.
// Later verification rejects same-content replacement and root movement too.
func sealLock(ctx context.Context, root, p string) error {
	lock, err := verifyLock(ctx, root, p)
	if err != nil {
		return err
	}
	s, err := safefs.Open(root)
	if err != nil {
		return err
	}
	defer s.Close()
	f, err := os.Open(root)
	if err != nil {
		return err
	}
	lock.RootIdentity, err = safefs.Identity(f)
	f.Close()
	if err != nil {
		return err
	}
	for i := range lock.Inputs {
		f, a, e := s.OpenFile(lock.Inputs[i].Path)
		if f != nil {
			f.Close()
		}
		if e != nil {
			return e
		}
		lock.Inputs[i].Identity = a.Identity
		lock.Inputs[i].Modified = a.Modified.UnixNano()
	}
	b, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > maxLockBytes {
		return fmt.Errorf("lock too large")
	}
	if err = os.WriteFile(p, b, 0600); err != nil {
		return err
	}
	_, err = verifyLock(ctx, root, p)
	return err
}

func delivery(ctx context.Context, root string) error {
	s, err := safefs.Open(root)
	if err != nil {
		return err
	}
	defer s.Close()
	d := Delivery{Schema: 1, Files: []components.File{}, Inference: "not_tested", CleanEnvironment: "not_verified"}
	err = s.Walk(ctx, safefs.Limits{MaxEntries: 100000, MaxDepth: 64}, func(a safefs.Entry) error {
		if a.Error != "" || a.LinkKind != "none" {
			return fmt.Errorf("unsafe delivery file: %s", a.Relative)
		}
		if a.IsDir {
			return nil
		}
		rel := filepath.ToSlash(a.Relative)
		if rel == "model-center/delivery.json" {
			return nil
		}
		if filepath.Base(rel) != "manifest.json" && filepath.Base(rel) != "startup-report.json" && rel != "model-center/fengshen-model-center.exe" && rel != "model-center/fengshen-shim.exe" && rel != "resource-layout.json" && rel != "source-inputs.lock.json" && rel != "external-inputs.lock.json" && rel != "compiler-inputs.lock.json" && rel != "dependency-inputs.lock.json" && rel != "toolchain.json" && rel != "build-results.json" {
			return nil
		}
		f, e := hash(ctx, s, a.Relative)
		if e == nil {
			d.Files = append(d.Files, f)
		}
		return e
	})
	if err != nil {
		return err
	}
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Path < d.Files[j].Path })
	if err = validateDelivery(ctx, s, d); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > components.MaxManifestBytes {
		return fmt.Errorf("delivery too large")
	}
	target := filepath.Join(root, "model-center", "delivery.json")
	if old, e := os.ReadFile(target); e == nil {
		if string(old) == string(b) {
			return nil
		}
		return fmt.Errorf("delivery exists with different content; use a fresh output")
	}
	return writeNew(target, d)
}
