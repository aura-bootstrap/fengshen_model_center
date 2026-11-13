package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/modelmanifest"
)

// writeModelManifest generates the single user manifest from model roots. A root
// that already carries a hub manifest reuses its declared hashes; otherwise the
// catalog recognition rules decide which files are weights.
func writeModelManifest(roots []string, target string) error {
	var entries []modelmanifest.Entry
	seen := map[string]bool{}
	add := func(e modelmanifest.Entry) {
		p := filepath.Clean(e.FilePath())
		key := e.ModelID + "\x00" + strings.ToLower(p)
		if seen[key] {
			return
		}
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 {
			return
		}
		e.Path, e.NewPath = p, ""
		if e.Bytes <= 0 {
			e.Bytes = info.Size()
		}
		seen[key] = true
		entries = append(entries, e)
	}
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			continue
		}
		root = filepath.Clean(root)
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			continue
		}
		hub := filepath.Join(root, "manifest.json")
		if m, _, err := modelmanifest.Load(hub); err == nil {
			for _, e := range m.Entries {
				add(e)
			}
			continue
		}
		count := 0
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || count > modelmanifest.MaxEntries {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			count++
			for _, model := range catalog() {
				if recognizes(model, p) {
					info, err := d.Info()
					if err != nil {
						continue
					}
					add(modelmanifest.Entry{ModelID: model.ID, Role: "weight", Path: p, Bytes: info.Size()})
					break
				}
			}
			return nil
		})
	}
	if len(entries) == 0 {
		return fmt.Errorf("未能在指定目录识别到模型权重")
	}
	m := modelmanifest.Manifest{Schema: 1, GeneratedAt: time.Now().UTC().Format(time.RFC3339), Root: roots[0], Entries: entries}
	if err := modelmanifest.Write(target, m); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "已写入 %s（%s 条，%s 字节）\n", target, strconv.Itoa(len(entries)), strconv.FormatInt(totalBytes(entries), 10))
	return nil
}

func totalBytes(entries []modelmanifest.Entry) int64 {
	var n int64
	for _, e := range entries {
		n += e.Bytes
	}
	return n
}

func splitRoots(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ";") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
