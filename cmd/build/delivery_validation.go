package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

const centerPath = "model-center/fengshen-model-center.exe"
const componentPath = "model-center/components"

type resourceLayout struct {
	Schema     int      `json:"schema"`
	Center     string   `json:"center"`
	Components string   `json:"components"`
	RootKind   string   `json:"root_kind"`
	ModelRoots []string `json:"model_roots"`
	HaarFiles  []string `json:"haar_files"`
}

// Parse only the bounded bytes whose digest is actually bound in delivery.
func deliveryBytes(s *safefs.Scope, item components.File) ([]byte, error) {
	f, a, err := s.OpenFile(item.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if a.LinkKind != "none" || a.Size > components.MaxManifestBytes {
		return nil, fmt.Errorf("invalid metadata: %s", item.Path)
	}
	b, err := io.ReadAll(io.LimitReader(f, components.MaxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(b)
	if fmt.Sprint(len(b)) != item.Bytes || hex.EncodeToString(h[:]) != item.SHA256 {
		return nil, fmt.Errorf("metadata changed: %s", item.Path)
	}
	return b, nil
}

func validateDelivery(ctx context.Context, s *safefs.Scope, d Delivery) error {
	files := map[string]components.File{}
	for _, f := range d.Files {
		files[f.Path] = f
	}
	exe, ok := files[centerPath]
	if !ok || exe.Bytes == "0" {
		return fmt.Errorf("delivery requires center executable")
	}
	layoutFile, ok := files["resource-layout.json"]
	if !ok {
		return fmt.Errorf("delivery requires resource-layout.json")
	}
	b, err := deliveryBytes(s, layoutFile)
	if err != nil {
		return err
	}
	var layout resourceLayout
	if err = components.StrictJSON(b, &layout); err != nil {
		return err
	}
	if layout.Schema != 1 || layout.Center != centerPath || layout.Components != componentPath || layout.RootKind != "installed" || layout.ModelRoots == nil || layout.HaarFiles == nil || len(layout.ModelRoots) > 64 || len(layout.HaarFiles) > 2 {
		return fmt.Errorf("invalid resource layout")
	}
	seen := map[string]bool{}
	for i, paths := range [][]string{layout.ModelRoots, layout.HaarFiles} {
		for _, rel := range paths {
			key := strings.ToLower(rel)
			if !components.SafeRelative(rel) || seen[key] {
				return fmt.Errorf("invalid/duplicate layout resource: %s", rel)
			}
			seen[key] = true
			resource, e := safefs.Open(s.Path() + "/" + rel)
			if e != nil {
				return fmt.Errorf("invalid layout resource: %s: %w", rel, e)
			}
			resolved := resource.Path()
			resource.Close()
			expected := filepath.Join(s.Path(), filepath.FromSlash(rel))
			st, e := os.Stat(expected)
			if e != nil || !strings.EqualFold(filepath.Clean(resolved), filepath.Clean(expected)) || st.IsDir() != (i == 0) {
				return fmt.Errorf("invalid layout resource type/link: %s", rel)
			}
			if i == 1 {
				if _, e := hash(ctx, s, rel); e != nil {
					return e
				}
			}
			if i == 1 && path.Base(rel) != "haarcascade_frontalface_default.xml" && path.Base(rel) != "haarcascade_profileface.xml" {
				return fmt.Errorf("invalid Haar resource")
			}
		}
	}
	manifests := map[string]components.Manifest{}
	reports := map[string]bool{}
	centerFound := false
	for _, item := range d.Files {
		if path.Base(item.Path) != "manifest.json" {
			continue
		}
		b, err = deliveryBytes(s, item)
		if err != nil {
			return err
		}
		m, e := components.ParseManifest(bytes.NewReader(b))
		if e != nil {
			return fmt.Errorf("%s: %w", item.Path, e)
		}
		prefix := componentPath + "/" + m.ComponentID + "/" + m.Version + "/"
		key := m.ComponentID + "/" + m.Version
		if item.Path != prefix+"manifest.json" || manifests[key].ComponentID != "" {
			return fmt.Errorf("manifest outside fixed component layout or duplicated")
		}
		manifests[key] = m
		if m.StartupReportRef == "" {
			return fmt.Errorf("delivery requires startup report: %s", key)
		}
		report, exists := files[prefix+m.StartupReportRef]
		if !exists {
			return fmt.Errorf("startup report not bound: %s", key)
		}
		body, e := deliveryBytes(s, report)
		if e != nil {
			return e
		}
		p, e := components.ParseStartupReport(bytes.NewReader(body), m, item.SHA256)
		if e != nil {
			return fmt.Errorf("%s: %w", report.Path, e)
		}
		if p.StartupTest != "passed" || p.CleanEnvironment != "not_verified" {
			return fmt.Errorf("delivery requires metadata smoke, not clean-environment claims: %s", key)
		}
		reports[report.Path] = true
		for _, want := range m.Files {
			got, e := hash(ctx, s, prefix+want.Path)
			if e != nil {
				return e
			}
			if got.Bytes != want.Bytes || got.SHA256 != strings.ToLower(want.SHA256) {
				return fmt.Errorf("component file digest mismatch: %s", got.Path)
			}
			if m.ComponentID == "model-center" && want.Path == m.Entry {
				if centerFound || m.Entry != "fengshen-model-center.exe" || m.EntryType != "executable" || m.Packaging != "go" || m.OS != "windows" || m.Arch != "amd64" || got.SHA256 != exe.SHA256 || got.Bytes != exe.Bytes {
					return fmt.Errorf("center manifest/executable mismatch")
				}
				centerFound = true
			}
		}
	}
	if !centerFound {
		return fmt.Errorf("delivery requires matching center manifest and startup report")
	}
	for _, f := range d.Files {
		if path.Base(f.Path) == "startup-report.json" && !reports[f.Path] {
			return fmt.Errorf("orphan startup report: %s", f.Path)
		}
	}
	visiting, done := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(key string) error {
		if visiting[key] {
			return fmt.Errorf("cyclic component dependency")
		}
		if done[key] {
			return nil
		}
		m, ok := manifests[key]
		if !ok {
			return fmt.Errorf("missing delivered dependency: %s", key)
		}
		visiting[key] = true
		for _, dep := range m.RuntimeDependencies {
			if e := visit(dep.ComponentID + "/" + dep.Version); e != nil {
				return e
			}
		}
		visiting[key] = false
		done[key] = true
		return nil
	}
	for key := range manifests {
		if err := visit(key); err != nil {
			return err
		}
	}
	// Refuse any observable file movement while assembling the evidence.
	for _, want := range d.Files {
		got, e := hash(ctx, s, want.Path)
		if e != nil {
			return e
		}
		if got != want {
			return fmt.Errorf("delivery changed: %s", want.Path)
		}
	}
	return nil
}
