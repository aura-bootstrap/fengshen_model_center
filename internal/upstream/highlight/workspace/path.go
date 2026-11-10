package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidatePath validates a strict descendant of an existing real work root.
// Every existing path component is checked with Lstat, so symlinks, Windows
// junctions, and other reparse points are rejected before media commands use
// the path. A missing final component is allowed for a prospective output.
func ValidatePath(root, path string) error {
	if err := validateRoot(root); err != nil {
		return err
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("%w: resolve path root", ErrUnsafePath)
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%w: resolve descendant path", ErrUnsafePath)
	}
	rootAbs = filepath.Clean(rootAbs)
	pathAbs = filepath.Clean(pathAbs)
	relative, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: path must be a strict descendant of the work root", ErrUnsafePath)
	}

	current := rootAbs
	components := strings.Split(relative, string(filepath.Separator))
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("%w: invalid path component", ErrUnsafePath)
		}
		current = filepath.Join(current, component)
		info, inspectErr := os.Lstat(current)
		if inspectErr != nil {
			if errors.Is(inspectErr, os.ErrNotExist) && index == len(components)-1 {
				continue
			}
			return fmt.Errorf("%w: inspect path component: %v", ErrUnsafePath, inspectErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info) {
			return fmt.Errorf("%w: linked or reparse path component", ErrUnsafePath)
		}
		if index < len(components)-1 && !info.IsDir() {
			return fmt.Errorf("%w: non-directory parent component", ErrUnsafePath)
		}
	}

	resolvedRoot, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return fmt.Errorf("%w: resolve path root: %v", ErrUnsafePath, err)
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(pathAbs))
	if err != nil {
		return fmt.Errorf("%w: resolve output parent: %v", ErrUnsafePath, err)
	}
	parentRelative, err := filepath.Rel(resolvedRoot, resolvedParent)
	if err != nil || parentRelative == ".." || filepath.IsAbs(parentRelative) || strings.HasPrefix(parentRelative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: resolved output parent escapes the work root", ErrUnsafePath)
	}
	return nil
}

// ValidateRegularFile requires an existing ordinary file below root.
func ValidateRegularFile(root, path string) error {
	if err := ValidatePath(root, path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: inspect regular file: %w", ErrUnsafePath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: output is not a regular file", ErrUnsafePath)
	}
	return nil
}

// RemoveRegularFile removes only a validated ordinary descendant. Missing
// files are idempotent; linked, reparse, directory, and outside paths are kept.
func RemoveRegularFile(root, path string) error {
	if err := ValidateRegularFile(root, path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove validated temporary file: %w", err)
	}
	return nil
}
