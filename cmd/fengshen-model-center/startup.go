package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

type resourceLayout struct {
	Schema     int      `json:"schema"`
	Center     string   `json:"center"`
	Components string   `json:"components"`
	RootKind   string   `json:"root_kind"`
	ModelRoots []string `json:"model_roots"`
	HaarFiles  []string `json:"haar_files"`
}

func resolveStartupRoot(root, kind string) (string, resourceLayout, error) {
	var layout resourceLayout
	if kind == "development" {
		resolved, err := resolveRoot(root)
		return resolved, layout, err
	}
	if kind != "installed" || root == "" || !filepath.IsAbs(root) {
		return "", layout, fmt.Errorf("根目录类型或安装根无效")
	}
	root = filepath.Clean(root)
	scope, err := safefs.Open(root)
	if err != nil {
		return "", layout, err
	}
	defer scope.Close()
	if !strings.EqualFold(filepath.Clean(root), filepath.Clean(scope.Path())) {
		return "", layout, fmt.Errorf("安装根不得通过链接重定向")
	}
	f, entry, err := scope.OpenFile("resource-layout.json")
	if err != nil {
		return "", layout, err
	}
	defer f.Close()
	if entry.LinkKind != "none" {
		return "", layout, fmt.Errorf("安装布局不得为链接")
	}
	body, err := io.ReadAll(io.LimitReader(f, components.MaxManifestBytes+1))
	if err != nil {
		return "", layout, err
	}
	if len(body) > components.MaxManifestBytes {
		return "", layout, fmt.Errorf("安装布局过大")
	}
	if err := components.StrictJSON(body, &layout); err != nil {
		return "", layout, err
	}
	if layout.Schema != 1 || layout.Center != "model-center/fengshen-model-center.exe" || layout.Components != "model-center/components" || layout.RootKind != "installed" || layout.ModelRoots == nil || layout.HaarFiles == nil || len(layout.ModelRoots) > 64 || len(layout.HaarFiles) > 2 {
		return "", layout, fmt.Errorf("安装布局字段无效")
	}
	seen := map[string]bool{}
	for i, rel := range append(append([]string{}, layout.ModelRoots...), layout.HaarFiles...) {
		key := strings.ToLower(rel)
		if !components.SafeRelative(rel) || seen[key] {
			return "", layout, fmt.Errorf("安装资源相对路径无效或重复")
		}
		seen[key] = true
		var resource *os.File
		var observed safefs.Entry
		if i < len(layout.ModelRoots) {
			resource, observed, err = scope.OpenDirectory(filepath.FromSlash(rel))
		} else {
			resource, observed, err = scope.OpenFile(filepath.FromSlash(rel))
		}
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", layout, err
		}
		resource.Close()
		if observed.LinkKind != "none" {
			return "", layout, fmt.Errorf("安装资源不允许链接")
		}
	}
	for _, rel := range layout.HaarFiles {
		base := filepath.Base(filepath.FromSlash(rel))
		if base != "haarcascade_frontalface_default.xml" && base != "haarcascade_profileface.xml" {
			return "", layout, fmt.Errorf("Haar 文件身份无效")
		}
	}
	return scope.Path(), layout, nil
}

// The component copy authenticates this center's embedded implementation, not a separate worker.
func writeHaarComponentInfo(w io.Writer) {
	_ = json.NewEncoder(w).Encode(map[string]any{"schema": 1, "component_id": "haar-consumer", "version": buildVersion, "os": runtime.GOOS, "arch": runtime.GOARCH, "packaging": "go", "protocols": []string{"haar/v1"}, "embedded": true, "inference": "not_tested"})
}

func writeComponentInfo(w io.Writer) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schema":       1,
		"component_id": "model-center",
		"version":      buildVersion,
		"os":           runtime.GOOS,
		"arch":         runtime.GOARCH,
		"packaging":    "go",
		"protocols": []string{
			componentprotocol.StreamWSv1,
			componentprotocol.BatchHTTPv1,
			componentprotocol.TimbreHTTPv1,
			componentprotocol.HaarHTTPv1,
			componentprotocol.LlamaHTTPv1,
		},
		"inference": "not_tested",
	})
}
