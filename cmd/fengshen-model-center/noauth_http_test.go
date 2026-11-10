package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
)

func TestBindingsEndpointNeedsNoSession(t *testing.T) {
	m := httpManager(t)
	h := newHandler(m, "127.0.0.1:18120", nil)
	w := httpRequest(t, h, "GET", "/api/bindings", "", 200)
	if !strings.Contains(w.Body.String(), `"bindings"`) {
		t.Fatalf("unexpected bindings payload: %s", w.Body.String())
	}
}

func TestReadOnlyIntegrationNeedsNoToken(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"sidecar", "models"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0700); err != nil {
			t.Fatal(err)
		}
	}
	host, err := integration.NewHost(integration.HostConfig{
		Config:  integration.Config{InstallRoot: root, Root: root, RootKind: "development", DataDir: t.TempDir()},
		Address: "127.0.0.1:18120",
		Build:   "test-build",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = host.Register(); err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	m := httpManager(t)
	h := newHandler(m, "127.0.0.1:18120", host)
	w := httpRequest(t, h, "GET", "/api/integration/health", "", 200)
	if !strings.Contains(w.Body.String(), `"state"`) {
		t.Fatalf("unexpected health payload: %s", w.Body.String())
	}
}