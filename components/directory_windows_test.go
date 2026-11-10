package components

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestListDoesNotTraverseJunctionLayers(t *testing.T) {
	for _, layer := range []string{"root", "component", "version"} {
		t.Run(layer, func(t *testing.T) {
			source, _ := fixture(t)
			root := t.TempDir()
			var target, link string
			switch layer {
			case "root":
				target = source
				link = filepath.Join(root, "components")
				root = link
			case "component":
				target = filepath.Join(source, "whisper-batch")
				link = filepath.Join(root, "whisper-batch")
			case "version":
				target = filepath.Join(source, "whisper-batch", "1")
				if err := os.Mkdir(filepath.Join(root, "whisper-batch"), 0700); err != nil {
					t.Fatal(err)
				}
				link = filepath.Join(root, "whisper-batch", "1")
			}
			out, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput()
			if err != nil {
				t.Skipf("junction creation unavailable: %v %s", err, out)
			}
			defer os.Remove(link)
			mgr := New(root)
			e := find(t, mgr.List(), "whisper-batch")
			if e.Packaging != "pending_build" || e.StartupTest != "not_tested" {
				t.Fatalf("junction traversed: %+v", e)
			}
			if e, err = mgr.Verify(context.Background(), "whisper-batch", "1"); err == nil || e.Integrity == "matched" {
				t.Fatalf("junction verified: %+v %v", e, err)
			}
		})
	}
}
