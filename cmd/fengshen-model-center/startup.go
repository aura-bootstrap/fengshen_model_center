package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

func resolveStartupRoot(root, kind string) (string, error) {
	if kind == "development" {
		return resolveRoot(root)
	}
	if kind != "installed" || root == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("根目录类型或安装根无效")
	}
	root = filepath.Clean(root)
	scope, err := safefs.Open(root)
	if err != nil {
		return "", err
	}
	defer scope.Close()
	if !strings.EqualFold(filepath.Clean(root), filepath.Clean(scope.Path())) {
		return "", fmt.Errorf("安装根不得通过链接重定向")
	}
	return scope.Path(), nil
}

// The component copy must match this center's embedded implementation, not a separate worker.
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
