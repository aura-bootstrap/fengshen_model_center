package components

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/safefs"
)

func TestCachedCurrentMetadataOnly(t *testing.T) {
	for _, change := range []string{"none", "report", "manifest", "binary", "removed", "replacement"} {
		t.Run(change, func(t *testing.T) {
			root, manifest, _ := reportFixture(t)
			mgr := New(root)
			e, err := mgr.Verify(context.Background(), manifest.ComponentID, manifest.Version)
			if err != nil {
				t.Fatal(err)
			}
			dir := ComponentDirectory(root, manifest.ComponentID, manifest.Version)
			path := filepath.Join(dir, manifest.Entry)
			switch change {
			case "report":
				path = filepath.Join(dir, "startup.json")
			case "manifest":
				path = filepath.Join(dir, "manifest.json")
			}
			if change == "removed" {
				err = os.Remove(path)
			} else if change == "replacement" {
				var b []byte
				b, err = os.ReadFile(path)
				if err == nil {
					err = os.Remove(path)
				}
				if err == nil {
					err = os.WriteFile(path, b, 0600)
				}
			} else if change != "none" {
				err = os.Chtimes(path, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := mgr.CachedCurrent(e); got != (change == "none") {
				t.Fatalf("current=%v", got)
			}
			// This read-only check must not create/refresh cache entries.
			if len(mgr.verified) != 1 {
				t.Fatal("cache mutated")
			}
		})
	}
}

func TestCachedCurrentUnknownAndLimit(t *testing.T) {
	root, manifest, _ := reportFixture(t)
	mgr := New(root)
	e := find(t, mgr.List(), manifest.ComponentID)
	if !mgr.CachedCurrent(e) {
		t.Fatal("report observation not current")
	}
	e.ManifestSHA256 = "wrong"
	if mgr.CachedCurrent(e) {
		t.Fatal("different build admitted")
	}
	e = Evidence{ComponentID: "unknown", Version: "1"}
	if mgr.CachedCurrent(e) {
		t.Fatal("unknown admitted")
	}
	e.ComponentID, e.ManifestSHA256 = manifest.ComponentID, "digest"
	mgr.verified[e.ComponentID+"/1"] = cached{manifest: "digest", entries: make([]safefs.Entry, 257)}
	if mgr.CachedCurrent(e) {
		t.Fatal("metadata budget exceeded")
	}
}
