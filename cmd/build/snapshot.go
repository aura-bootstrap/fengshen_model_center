package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

// Include the complete native/resource tree, so added inputs also invalidate locks.
func nativeResourceInputs(repo string, add func(string) error) error {
	return filepath.WalkDir(filepath.Join(repo, "components-src"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("linked native resource input: %s", p)
		}
		if !d.IsDir() {
			return add(p)
		}
		return nil
	})
}

// snapshot uses Go's own resolved dependency and embed input lists, including
// tests, module metadata, the compiler tree, and this pipeline's non-Go inputs.
// It never downloads: callers set GOPROXY=off, GOTOOLCHAIN=local; dependencies
// are vendored, so the module cache is not consulted for remote fetches.
func snapshot(ctx context.Context, repo, output string, engines, check bool) error {
	goexe, err := exec.LookPath("go")
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, goexe, "env", "GOROOT", "GOMODCACHE")
	env, err := cmd.Output()
	if err != nil {
		return err
	}
	roots := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(env), "\r", "")), "\n")
	if len(roots) != 2 {
		return fmt.Errorf("unexpected Go input roots")
	}
	groups := []struct {
		root, name string
		paths      map[string]bool
	}{
		{repo, "source-inputs.lock.json", map[string]bool{}},
		{roots[0], "compiler-inputs.lock.json", map[string]bool{}},
		{roots[1], "dependency-inputs.lock.json", map[string]bool{}},
	}
	add := func(p string) error {
		p = filepath.Clean(p)
		for i := range groups {
			rel, e := filepath.Rel(groups[i].root, p)
			if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
				groups[i].paths[filepath.ToSlash(rel)] = true
				return nil
			}
		}
		return fmt.Errorf("uncontrolled input outside source/toolchain/cache: %s", p)
	}
	modules := []string{"."}
	if engines {
		modules = append(modules, "src/admin_client/backend", "src/dev_client/backend", "src/user_client/backend")
	}
	for _, mod := range modules {
		args := []string{"-C", filepath.Join(repo, mod), "list", "-deps", "-test", "-json", "./..."}
		if mod != "." {
			args[len(args)-1] = "./cmd/fengshen-engine"
		}
		cmd := exec.CommandContext(ctx, goexe, args...)
		cmd.Stderr = os.Stderr
		pipe, e := cmd.StdoutPipe()
		if e != nil {
			return e
		}
		if e = cmd.Start(); e != nil {
			return e
		}
		decoder := json.NewDecoder(pipe)
		for {
			var pkg struct {
				ImportPath, ForTest                                                                                                                                                             string
				Dir                                                                                                                                                                             string
				GoFiles, CgoFiles, CFiles, CXXFiles, MFiles, HFiles, FFiles, SFiles, SwigFiles, SwigCXXFiles, SysoFiles, EmbedFiles, TestGoFiles, XTestGoFiles, TestEmbedFiles, XTestEmbedFiles []string
				Module                                                                                                                                                                          *struct{ Dir, GoMod string }
			}
			e = decoder.Decode(&pkg)
			if e == io.EOF {
				break
			}
			if e != nil {
				cmd.Process.Kill()
				cmd.Wait()
				return e
			}
			// Synthetic test-main is a cache product, not a source input; its
			// generator is already pinned with the complete Go toolchain.
			if strings.HasSuffix(pkg.ImportPath, ".test") {
				continue
			}
			lists := [][]string{pkg.GoFiles, pkg.CgoFiles, pkg.CFiles, pkg.CXXFiles, pkg.MFiles, pkg.HFiles, pkg.FFiles, pkg.SFiles, pkg.SwigFiles, pkg.SwigCXXFiles, pkg.SysoFiles, pkg.EmbedFiles, pkg.TestGoFiles, pkg.XTestGoFiles, pkg.TestEmbedFiles, pkg.XTestEmbedFiles}
			for _, list := range lists {
				for _, file := range list {
					if err := add(filepath.Join(pkg.Dir, file)); err != nil {
						cmd.Process.Kill()
						cmd.Wait()
						return err
					}
				}
			}
			if pkg.Module != nil && pkg.Module.GoMod != "" {
				if err := add(pkg.Module.GoMod); err != nil {
					cmd.Process.Kill()
					cmd.Wait()
					return err
				}
				sum := filepath.Join(filepath.Dir(pkg.Module.GoMod), "go.sum")
				if _, err := os.Stat(sum); err == nil {
					if err = add(sum); err != nil {
						cmd.Process.Kill()
						cmd.Wait()
						return err
					}
				}
			}
		}
		if e = cmd.Wait(); e != nil {
			return e
		}
	}
	for _, rel := range []string{"installer/build_model_center.ps1", "installer/build_whisper_stream.ps1", "installer/build_whisper_batch.ps1", "installer/build_llama_runtime.ps1", "sidecar/model_center_frozen_runner.py"} {
		if err = add(filepath.Join(repo, rel)); err != nil {
			return err
		}
	}
	if err = nativeResourceInputs(repo, add); err != nil {
		return err
	}
	// Compile/link tools themselves are inputs, not merely the go launcher.
	for _, rel := range []string{"bin", "pkg/tool", "lib", "VERSION", "go.env"} {
		base := filepath.Join(roots[0], rel)
		err = filepath.WalkDir(base, func(p string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("linked compiler input")
			}
			if !d.IsDir() {
				return add(p)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for _, g := range groups {
		if check {
			if len(g.paths) == 0 {
				continue
			}
			lock, e := verifyLock(ctx, g.root, filepath.Join(output, g.name))
			if e != nil {
				return e
			}
			if len(lock.Inputs) != len(g.paths) {
				return fmt.Errorf("input set changed: %s", g.name)
			}
			for _, in := range lock.Inputs {
				if !g.paths[in.Path] {
					return fmt.Errorf("input set changed: %s", in.Path)
				}
			}
			continue
		}
		if len(g.paths) == 0 {
			continue
		}
		s, e := safefs.Open(g.root)
		if e != nil {
			return e
		}
		lock := Lock{Schema: 1, Inputs: []LockedInput{}}
		paths := make([]string, 0, len(g.paths))
		for p := range g.paths {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			f, e := hash(ctx, s, p)
			if e != nil {
				s.Close()
				return fmt.Errorf("snapshot %s/%s: %w", g.root, p, e)
			}
			lock.Inputs = append(lock.Inputs, LockedInput{Path: p, SHA256: f.SHA256, Version: "local-snapshot", License: "local input; provenance only, review required"})
		}
		s.Close()
		b, e := json.MarshalIndent(lock, "", "  ")
		if e != nil {
			return e
		}
		if len(b) > maxLockBytes {
			return fmt.Errorf("input lock too large")
		}
		target := filepath.Join(output, g.name)
		if e = os.WriteFile(target, b, 0600); e != nil {
			return e
		}
		if e = sealLock(ctx, g.root, target); e != nil {
			return e
		}
	}
	return nil
}
