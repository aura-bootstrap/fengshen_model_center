// Package workspace provides process-owned temporary task directories.
package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const (
	rootDirectoryName = ".myclip_work"
	markerName        = ".myclip_owner"
	markerContents    = "myclip-go workspace owner v1\n"
	taskNameLength    = 32
)

var (
	// ErrUnsafePath reports that a path failed an ownership or link-safety check.
	ErrUnsafePath = errors.New("unsafe workspace path")
	// ErrClosed reports use of a workspace or task after it was closed.
	ErrClosed = errors.New("workspace is closed")
)

type outputLock struct {
	mu sync.Mutex
}

var outputLocks = struct {
	sync.Mutex
	byRoot map[string]*outputLock
}{byRoot: make(map[string]*outputLock)}

// Workspace owns a .myclip_work root. Open workspaces for the same resolved
// output directory are serialized within this process.
type Workspace struct {
	root     string
	lock     *outputLock
	mu       sync.Mutex
	active   map[string]*Task
	closing  bool
	closed   bool
	closeErr error
}

// Task is a uniquely named temporary directory owned by a Workspace.
type Task struct {
	workspace *Workspace
	path      string
	mu        sync.Mutex
	closed    bool
}

// Open creates or opens outputFolder/.myclip_work, rejects a linked root, and
// removes stale directories only after validating their ownership markers.
// The caller must Close the returned workspace to release the output lock.
func Open(outputFolder string) (*Workspace, error) {
	if strings.TrimSpace(outputFolder) == "" {
		return nil, fmt.Errorf("output folder is required")
	}
	outputAbs, err := filepath.Abs(outputFolder)
	if err != nil {
		return nil, fmt.Errorf("resolve output folder: %w", err)
	}
	if err := os.MkdirAll(outputAbs, 0o755); err != nil {
		return nil, fmt.Errorf("create output folder: %w", err)
	}
	root := filepath.Join(outputAbs, rootDirectoryName)
	if err := os.Mkdir(root, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create workspace root: %w", err)
	}
	if err := validateRoot(root); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	root = filepath.Clean(resolved)

	outputLocks.Lock()
	lock := outputLocks.byRoot[canonicalPath(root)]
	if lock == nil {
		lock = &outputLock{}
		outputLocks.byRoot[canonicalPath(root)] = lock
	}
	outputLocks.Unlock()
	lock.mu.Lock()

	w := &Workspace{root: root, lock: lock, active: make(map[string]*Task)}
	if err := w.CleanupStale(); err != nil {
		lock.mu.Unlock()
		return nil, err
	}
	return w, nil
}

// Root returns the resolved work-root path. It is never recursively removed.
func (w *Workspace) Root() string { return w.root }

// NewTask creates a 32-lowercase-hex task directory and its ownership marker.
func (w *Workspace) NewTask() (*Task, error) {
	w.mu.Lock()
	if w.closing || w.closed {
		w.mu.Unlock()
		return nil, ErrClosed
	}
	w.mu.Unlock()

	for attempt := 0; attempt < 100; attempt++ {
		var random [16]byte
		if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
			return nil, fmt.Errorf("generate task name: %w", err)
		}
		name := hex.EncodeToString(random[:])
		path := filepath.Join(w.root, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return nil, fmt.Errorf("create task directory: %w", err)
		}
		if err := writeMarker(path); err != nil {
			// The directory is still empty if marker creation failed. Never use
			// RemoveAll on a directory whose complete marker was not verified.
			removeErr := os.Remove(path)
			return nil, errors.Join(fmt.Errorf("write task ownership marker: %w", err), removeErr)
		}
		task := &Task{workspace: w, path: path}
		w.mu.Lock()
		if w.closing || w.closed {
			w.mu.Unlock()
			cleanupErr := cleanupOwnedTask(w.root, path)
			return nil, errors.Join(ErrClosed, cleanupErr)
		}
		w.active[name] = task
		w.mu.Unlock()
		return task, nil
	}
	return nil, fmt.Errorf("could not allocate a unique task directory")
}

func writeMarker(taskPath string) error {
	file, err := os.OpenFile(filepath.Join(taskPath, markerName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(file, markerContents)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

// Dir returns the task directory path.
func (t *Task) Dir() string { return t.path }

// Cleanup validates and recursively removes this task. It is idempotent.
func (t *Task) Cleanup() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()

	w := t.workspace
	if err := cleanupOwnedTask(w.root, t.path); err != nil {
		return err
	}
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	w.mu.Lock()
	delete(w.active, filepath.Base(t.path))
	w.mu.Unlock()
	return nil
}

// CleanupStale removes valid, inactive owned tasks. Unknown files and malformed
// directory names are preserved. Unsafe task-shaped entries return an error.
func (w *Workspace) CleanupStale() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return ErrClosed
	}
	active := make(map[string]bool, len(w.active))
	for name := range w.active {
		active[name] = true
	}
	w.mu.Unlock()

	if err := validateRoot(w.root); err != nil {
		return err
	}
	entries, err := os.ReadDir(w.root)
	if err != nil {
		return fmt.Errorf("read workspace root: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if active[name] || !validTaskName(name) {
			continue
		}
		path := filepath.Join(w.root, name)
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			errs = append(errs, fmt.Errorf("inspect stale task %q: %w", name, err))
			continue
		}
		if isReparsePoint(info) || info.Mode()&os.ModeSymlink != 0 {
			errs = append(errs, fmt.Errorf("%w: linked task %q", ErrUnsafePath, name))
			continue
		}
		if !info.IsDir() {
			// Unknown ordinary files are never removed.
			continue
		}
		if err := cleanupOwnedTask(w.root, path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close cleans all tasks created by this workspace, then stale tasks, and
// releases the output lock. Cleanup errors do not prevent lock release.
func (w *Workspace) Close() error {
	w.mu.Lock()
	if w.closed {
		err := w.closeErr
		w.mu.Unlock()
		return err
	}
	w.closing = true
	tasks := make([]*Task, 0, len(w.active))
	for _, task := range w.active {
		tasks = append(tasks, task)
	}
	w.mu.Unlock()

	var errs []error
	for _, task := range tasks {
		if err := task.Cleanup(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := w.CleanupStale(); err != nil {
		errs = append(errs, err)
	}
	err := errors.Join(errs...)

	w.mu.Lock()
	w.closeErr = err
	w.closed = true
	w.closing = false
	w.mu.Unlock()
	w.lock.mu.Unlock()
	return err
}

func cleanupOwnedTask(root, taskPath string) error {
	return cleanupOwnedDirectory(root, taskPath, validTaskName, "task")
}

// CreateSourceDir creates or reopens a task-owned highlight_sources or
// source_N directory. Reopening requires the same complete ownership marker;
// pre-existing unmarked directories are never adopted.
func CreateSourceDir(workDir, name string) (string, error) {
	if !validSourceName(name) {
		return "", fmt.Errorf("%w: invalid source directory name %q", ErrUnsafePath, name)
	}
	rootAbs, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("%w: resolve source root", ErrUnsafePath)
	}
	rootAbs = filepath.Clean(rootAbs)
	if err := os.MkdirAll(rootAbs, 0o700); err != nil {
		return "", fmt.Errorf("create source root: %w", err)
	}
	if err := validateRoot(rootAbs); err != nil {
		return "", err
	}
	path := filepath.Join(rootAbs, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("create owned source directory %q: %w", name, err)
		}
		if err := validateOwnedDirectory(rootAbs, path, validSourceName, "source"); err != nil {
			return "", err
		}
		return path, nil
	}
	if err := writeMarker(path); err != nil {
		removeErr := os.Remove(path)
		return "", errors.Join(fmt.Errorf("write source ownership marker: %w", err), removeErr)
	}
	return path, nil
}

// CleanupSourceDir removes one owned highlight_sources or source_N directory.
// Missing directories are idempotent; links, reparse points, foreign markers,
// non-direct children, and outside paths are preserved and rejected.
func CleanupSourceDir(workDir, path string) error {
	return cleanupOwnedDirectory(workDir, path, validSourceName, "source")
}

// CleanupTraditionalSourceDirs removes only owned source_N children of a task
// work directory. The work directory and every unrelated entry are preserved.
func CleanupTraditionalSourceDirs(workDir string) error {
	if err := validateRoot(workDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return fmt.Errorf("read task source root: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if name == "highlight_sources" || !validSourceName(name) {
			continue
		}
		if err := CleanupSourceDir(workDir, filepath.Join(workDir, name)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func cleanupOwnedDirectory(root, path string, validName func(string) bool, kind string) error {
	if err := validateOwnedDirectory(root, path, validName, kind); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove owned %s %q: %w", kind, filepath.Base(path), err)
	}
	return nil
}

func validateOwnedTask(root, taskPath string) error {
	return validateOwnedDirectory(root, taskPath, validTaskName, "task")
}

func validateOwnedDirectory(root, path string, validName func(string) bool, kind string) error {
	if err := validateRoot(root); err != nil {
		return err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("%w: resolve %s root", ErrUnsafePath, kind)
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%w: resolve %s", ErrUnsafePath, kind)
	}
	rootAbs = filepath.Clean(rootAbs)
	pathAbs = filepath.Clean(pathAbs)
	name := filepath.Base(pathAbs)
	if !validName(name) || canonicalPath(filepath.Dir(pathAbs)) != canonicalPath(rootAbs) {
		return fmt.Errorf("%w: %s is not a direct child of the work root", ErrUnsafePath, kind)
	}

	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(pathAbs))
	if err != nil {
		return fmt.Errorf("%w: resolve %s parent: %v", ErrUnsafePath, kind, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return fmt.Errorf("%w: resolve %s root: %v", ErrUnsafePath, kind, err)
	}
	if canonicalPath(resolvedParent) != canonicalPath(resolvedRoot) {
		return fmt.Errorf("%w: resolved %s parent differs from root", ErrUnsafePath, kind)
	}

	info, err := os.Lstat(pathAbs)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info) || !info.IsDir() {
		return fmt.Errorf("%w: %s must be a real directory", ErrUnsafePath, kind)
	}
	markerPath := filepath.Join(pathAbs, markerName)
	markerInfo, err := os.Lstat(markerPath)
	if err != nil {
		return fmt.Errorf("%w: ownership marker unavailable: %v", ErrUnsafePath, err)
	}
	if markerInfo.Mode()&os.ModeSymlink != 0 || isReparsePoint(markerInfo) || !markerInfo.Mode().IsRegular() {
		return fmt.Errorf("%w: ownership marker is not a regular file", ErrUnsafePath)
	}
	if markerInfo.Size() != int64(len(markerContents)) {
		return fmt.Errorf("%w: ownership marker mismatch", ErrUnsafePath)
	}
	contents, err := os.ReadFile(markerPath)
	if err != nil || string(contents) != markerContents {
		return fmt.Errorf("%w: ownership marker mismatch", ErrUnsafePath)
	}
	return nil
}

func validateRoot(root string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect workspace root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info) || !info.IsDir() {
		return fmt.Errorf("%w: work root must be a real directory", ErrUnsafePath)
	}
	return nil
}

func validTaskName(name string) bool {
	if len(name) != taskNameLength {
		return false
	}
	for _, char := range name {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validSourceName(name string) bool {
	if name == "highlight_sources" {
		return true
	}
	if len(name) < len("source_")+1 || len(name) > len("source_")+4 || !strings.HasPrefix(name, "source_") {
		return false
	}
	digits := strings.TrimPrefix(name, "source_")
	if digits[0] < '1' || digits[0] > '9' {
		return false
	}
	for _, char := range digits[1:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func canonicalPath(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}
