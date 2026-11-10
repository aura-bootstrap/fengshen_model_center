// Package safefs provides read-only, handle-anchored local filesystem scopes.
package safefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrLimit    = errors.New("scan_limit: 扫描达到限额")
	ErrPartial  = errors.New("scope_incomplete: 部分目录项不可访问")
	ErrBoundary = errors.New("link_not_allowed: 无法确认授权范围")
)

type Limits struct{ MaxEntries, MaxDepth int }
type Entry struct {
	// SourcePath is the checked request path; Path is the final handle target.
	Relative, SourcePath, Path, Identity, LinkKind string
	Size                                           int64
	Modified                                       time.Time
	IsDir                                          bool
	Error                                          string
}
type Scope struct {
	mu             sync.RWMutex
	root           *os.Root
	path, leaf, id string
	closed         bool
}

func ValidatePath(p string) error {
	if p == "" || strings.ContainsRune(p, 0) || strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") || !filepath.IsAbs(p) {
		return fmt.Errorf("path_invalid: 必须为本机绝对路径")
	}
	// Apply Windows namespace restrictions on every platform as inputs can be persisted.
	w := strings.ReplaceAll(p, `\`, "/")
	for i, c := range w {
		if c == ':' && !(i == 1 && ((w[0] >= 'A' && w[0] <= 'Z') || (w[0] >= 'a' && w[0] <= 'z'))) {
			return fmt.Errorf("path_invalid: 禁止替代数据流")
		}
	}
	for _, c := range strings.Split(w, "/") {
		if c == "" {
			continue
		}
		if strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") {
			return fmt.Errorf("path_invalid: 路径尾部不明确")
		}
		b := strings.ToUpper(strings.SplitN(c, ".", 2)[0])
		if b == "CON" || b == "PRN" || b == "AUX" || b == "NUL" || b == "CONIN$" || b == "CONOUT$" || (len(b) == 4 && (strings.HasPrefix(b, "COM") || strings.HasPrefix(b, "LPT")) && strings.ContainsRune("123456789¹²³", rune(b[3]))) {
			return fmt.Errorf("path_invalid: 保留设备名")
		}
	}
	p = filepath.Clean(p)
	if filepath.Dir(p) == p {
		return fmt.Errorf("path_invalid: 禁止卷根")
	}
	return localPath(p)
}

// PersistParents publishes directory entries up to and including the owned root.
func PersistParents(path, root string) error {
	if err := ValidatePath(path); err != nil {
		return err
	}
	if err := ValidatePath(root); err != nil {
		return err
	}
	path, root = filepath.Clean(path), filepath.Clean(root)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return ErrBoundary
	}
	for current := path; ; current = filepath.Dir(current) {
		if err := PersistParent(current); err != nil {
			return err
		}
		if samePath(current, root) {
			return nil
		}
	}
}

func Open(p string) (*Scope, error) {
	if e := ValidatePath(p); e != nil {
		return nil, e
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.IsDir() && !st.Mode().IsRegular() {
		return nil, ErrBoundary
	}
	resolved, e := finalPath(f)
	if e != nil {
		return nil, e
	}
	if e = ValidatePath(resolved); e != nil {
		return nil, e
	}
	id, e := Identity(f)
	if e != nil {
		return nil, e
	}
	s := &Scope{path: resolved, id: id}
	base := resolved
	if !st.IsDir() {
		base = filepath.Dir(resolved)
		s.leaf = filepath.Base(resolved)
	}
	s.root, e = os.OpenRoot(base)
	if e != nil {
		return nil, e
	}
	rel := "."
	if s.leaf != "" {
		rel = s.leaf
	}
	g, e := s.root.Open(rel)
	if e == nil {
		var got string
		got, e = Identity(g)
		g.Close()
		if got != id {
			e = ErrBoundary
		}
	}
	if e != nil {
		s.root.Close()
		return nil, e
	}
	return s, nil
}

// Derive narrows an existing directory authorization without re-opening its absolute
// name. Every component must be ordinary and keep its observed identity and exact
// handle location. In particular a missing child behind a link is not "missing".
func (s *Scope) Derive(rel string) (*Scope, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, os.ErrClosed
	}
	if s.leaf != "" || !validRel(rel) {
		return nil, ErrBoundary
	}
	parts := strings.FieldsFunc(filepath.Clean(rel), func(r rune) bool { return r == '/' || r == '\\' })
	root := s.root
	owned := false
	defer func() {
		if owned {
			root.Close()
		}
	}()
	for i, part := range parts {
		anchor, err := root.Open(".")
		if err != nil {
			return nil, err
		}
		base, err := finalPath(anchor)
		anchor.Close()
		if err != nil {
			return nil, err
		}
		st, err := root.Lstat(part)
		if err != nil {
			return nil, err
		}
		kind, err := reparse(root, part, st)
		if err != nil {
			return nil, err
		}
		if kind != "none" || (!st.IsDir() && (i != len(parts)-1 || !st.Mode().IsRegular())) {
			return nil, ErrBoundary
		}
		beforeID, err := childIdentity(root, part, st)
		if err != nil {
			return nil, ErrBoundary
		}
		var next *os.Root
		var f *os.File
		if st.IsDir() {
			next, err = root.OpenRoot(part)
			if err == nil {
				f, err = next.Open(".")
			}
		} else {
			f, err = root.Open(part)
		}
		if err != nil {
			if next != nil {
				next.Close()
			}
			// The entry existed before opening; disappearance or redirection now
			// leaves its boundary uncertain, not an established missing default.
			return nil, fmt.Errorf("%v: %w", err, ErrBoundary)
		}
		actual, pathErr := finalPath(f)
		id, idErr := Identity(f)
		f.Close()
		checked, checkErr := root.Lstat(part)
		checkedKind, afterID := "", ""
		if checkErr == nil {
			checkedKind, checkErr = reparse(root, part, checked)
		}
		if checkErr == nil && checkedKind == "none" {
			afterID, checkErr = childIdentity(root, part, checked)
		}
		if pathErr != nil || idErr != nil || checkErr != nil || checkedKind != "none" || beforeID != id || afterID != id || !samePath(actual, filepath.Join(base, part)) {
			if next != nil {
				next.Close()
			}
			return nil, ErrBoundary
		}
		if err = localPath(actual); err != nil {
			if next != nil {
				next.Close()
			}
			return nil, err
		}
		if !st.IsDir() {
			// Transfer only a leaf policy, never its parent's directory permission.
			if !owned {
				root, err = root.OpenRoot(".")
				if err != nil {
					return nil, err
				}
			}
			owned = false
			return &Scope{root: root, path: actual, leaf: part, id: id}, nil
		}
		if owned {
			root.Close()
		}
		root, owned = next, true
	}
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	path, err := finalPath(f)
	id, idErr := Identity(f)
	f.Close()
	if err != nil || idErr != nil {
		return nil, ErrBoundary
	}
	if !owned {
		root, err = root.OpenRoot(".")
		if err != nil {
			return nil, err
		}
	}
	owned = false
	return &Scope{root: root, path: path, id: id}, nil
}

func samePath(a, b string) bool {
	r, err := filepath.Rel(a, b)
	return err == nil && r == "."
}

func (s *Scope) Path() string { return s.path }
func (s *Scope) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.root.Close()
}
func validRel(rel string) bool {
	return rel != "" && !filepath.IsAbs(rel) && filepath.IsLocal(rel) && !strings.ContainsAny(rel, ":\x00") && !strings.Contains(rel, `\`+`\`)
}
func (s *Scope) open(rel string, allowDir bool) (*os.File, Entry, error) {
	a := Entry{Relative: rel, Path: filepath.Join(s.path, rel), LinkKind: "none"}
	if s.leaf != "" {
		a.Path = s.path
	}
	a.SourcePath = a.Path
	if s.closed {
		return nil, a, os.ErrClosed
	}
	if !validRel(rel) || (s.leaf != "" && rel != s.leaf) {
		return nil, a, ErrBoundary
	}
	// Inspect every ancestor without following links; no directory reparse point is traversed.
	parts := strings.FieldsFunc(filepath.Clean(rel), func(r rune) bool { return r == '/' || r == '\\' })
	for i := range parts {
		q := filepath.Join(parts[:i+1]...)
		st, e := s.root.Lstat(q)
		if e != nil {
			return nil, a, e
		}
		kind, e := reparse(s.root, q, st)
		if e != nil {
			return nil, a, e
		}
		if kind != "none" {
			if i < len(parts)-1 {
				return nil, a, ErrBoundary
			}
			a.LinkKind = kind
			if kind != "symlink" {
				return nil, a, ErrBoundary
			}
		}
	}
	f, e := s.root.Open(rel)
	if e != nil {
		return nil, a, e
	}
	fail := func(e error) (*os.File, Entry, error) { f.Close(); return nil, a, e }
	st, e := f.Stat()
	if e != nil {
		return fail(e)
	}
	if st.IsDir() {
		if !allowDir || a.LinkKind != "none" {
			return fail(ErrBoundary)
		}
	} else if !st.Mode().IsRegular() {
		return fail(ErrBoundary)
	}
	actual, e := finalPath(f)
	if e != nil {
		return fail(e)
	}
	anchor, e := s.root.Open(".")
	if e != nil {
		return fail(e)
	}
	base, e := finalPath(anchor)
	anchor.Close()
	if e != nil {
		return fail(e)
	}
	r, e := filepath.Rel(base, actual)
	if e != nil || !filepath.IsLocal(r) {
		return fail(ErrBoundary)
	}
	if e = localPath(actual); e != nil {
		return fail(e)
	}
	a.Identity, e = Identity(f)
	if e != nil {
		return fail(e)
	}
	if s.leaf != "" && a.Identity != s.id {
		return fail(ErrBoundary)
	}
	a.Size = st.Size()
	a.Modified = st.ModTime()
	a.IsDir = st.IsDir()
	a.Path = actual
	a.SourcePath = filepath.Join(base, rel)
	return f, a, nil
}
func (s *Scope) OpenFile(rel string) (*os.File, Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.open(rel, false)
}
func (s *Scope) OpenDirectory(rel string) (*os.File, Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, a, err := s.open(rel, true)
	if err == nil && !a.IsDir {
		f.Close()
		return nil, a, ErrBoundary
	}
	return f, a, err
}
func (s *Scope) Walk(ctx context.Context, limits Limits, visit func(Entry) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return os.ErrClosed
	}
	if limits.MaxEntries <= 0 {
		limits.MaxEntries = 100000
	}
	if limits.MaxDepth <= 0 {
		limits.MaxDepth = 64
	}
	n := 0
	var diagnostics []error
	var walk func(string, int) error
	emit := func(rel string, allowDir bool) (Entry, error) {
		if e := ctx.Err(); e != nil {
			return Entry{}, e
		}
		if n >= limits.MaxEntries {
			return Entry{}, ErrLimit
		}
		n++
		f, a, e := s.open(rel, allowDir)
		if f != nil {
			f.Close()
		}
		if e != nil {
			a.Error = e.Error()
			diagnostics = append(diagnostics, fmt.Errorf("%s: %w", rel, e))
		}
		if e = visit(a); e != nil {
			return a, e
		}
		return a, nil
	}
	walk = func(rel string, depth int) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		f, _, e := s.open(rel, true)
		if e != nil {
			diagnostics = append(diagnostics, e)
			return nil
		}
		defer f.Close()
		// Bounded batches: cap retained names, then sort for deterministic visitation.
		var names []string
		for {
			if e = ctx.Err(); e != nil {
				return e
			}
			batch, err := f.ReadDir(128)
			for _, a := range batch {
				names = append(names, a.Name())
				if len(names) > limits.MaxEntries-n {
					break
				}
			}
			if len(names) > limits.MaxEntries-n {
				sort.Strings(names)
				names = names[:limits.MaxEntries-n]
				e = ErrLimit
				break
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					diagnostics = append(diagnostics, err)
				}
				break
			}
		}
		limitErr := e
		sort.Strings(names)
		for _, name := range names {
			q := filepath.Join(rel, name)
			if rel == "." {
				q = name
			}
			a, err := emit(q, true)
			if err != nil {
				return err
			}
			if a.Error != "" || !a.IsDir {
				continue
			}
			switch name {
			case ".git", ".hg", ".svn", ".locks":
				continue
			}
			if depth >= limits.MaxDepth {
				diagnostics = append(diagnostics, ErrLimit)
				continue
			}
			if err = walk(q, depth+1); err != nil {
				return err
			}
		}
		return limitErr
	}
	var e error
	if s.leaf != "" {
		_, e = emit(s.leaf, false)
	} else {
		e = walk(".", 1)
	}
	if e != nil {
		return errors.Join(append(diagnostics, e)...)
	}
	if len(diagnostics) > 0 {
		return errors.Join(append([]error{ErrPartial}, diagnostics...)...)
	}
	return ctx.Err()
}

// checkOrdinary disallows special lock/registry targets.
func checkOrdinary(p string) error {
	st, e := os.Lstat(p)
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() {
		return ErrBoundary
	}
	return nil
}
