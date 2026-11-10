//go:build windows

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func deliveryDigest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func deliveryFixture(t *testing.T) (*Client, deliveryManifest, resourceLayout) {
	t.Helper()
	c := &Client{cfg: Config{InstallRoot: t.TempDir()}}
	for _, rel := range []string{"model-center", "components/test", "models"} {
		if e := os.MkdirAll(filepath.Join(c.cfg.InstallRoot, rel), 0700); e != nil {
			t.Fatal(e)
		}
	}
	writeDeliveryFile(t, c.exe(), []byte("center fixture, not executable"))
	c.cfg.CenterSHA256 = deliveryDigest([]byte("center fixture, not executable"))
	writeDeliveryFile(t, filepath.Join(c.cfg.InstallRoot, "components/test/manifest.json"), []byte("{}"))
	writeDeliveryFile(t, filepath.Join(c.cfg.InstallRoot, "components/test/startup-report.json"), []byte("{}"))
	writeDeliveryFile(t, filepath.Join(c.cfg.InstallRoot, "haar.xml"), []byte("haar"))
	l := resourceLayout{Schema: 1, Center: centerRelative, Components: "model-center/components", RootKind: "installed", ModelRoots: []string{}, HaarFiles: []string{}}
	d := deliveryManifest{Schema: 1, Inference: "not_tested", CleanEnvironment: "not_verified"}
	b, _ := json.Marshal(l)
	writeDeliveryFile(t, filepath.Join(c.cfg.InstallRoot, "resource-layout.json"), b)
	for _, rel := range []string{centerRelative, "resource-layout.json", "components/test/manifest.json", "components/test/startup-report.json"} {
		raw, e := os.ReadFile(filepath.Join(c.cfg.InstallRoot, rel))
		if e != nil {
			t.Fatal(e)
		}
		d.Files = append(d.Files, deliveryFile{rel, strconv.Itoa(len(raw)), deliveryDigest(raw)})
	}
	saveDelivery(t, c, d)
	return c, d, l
}
func writeDeliveryFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if e := os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
}
func saveDelivery(t *testing.T, c *Client, d deliveryManifest) {
	t.Helper()
	b, e := json.Marshal(d)
	if e != nil {
		t.Fatal(e)
	}
	writeDeliveryFile(t, c.delivery(), b)
	c.cfg.DeliverySHA256 = deliveryDigest(b)
}
func saveLayout(t *testing.T, c *Client, d *deliveryManifest, l resourceLayout) {
	t.Helper()
	b, _ := json.Marshal(l)
	writeDeliveryFile(t, filepath.Join(c.cfg.InstallRoot, "resource-layout.json"), b)
	for i := range d.Files {
		if d.Files[i].Path == "resource-layout.json" {
			d.Files[i].Bytes = strconv.Itoa(len(b))
			d.Files[i].SHA256 = deliveryDigest(b)
		}
	}
	saveDelivery(t, c, *d)
}
func TestWindowsTrustedDeliverySemanticErrors(t *testing.T) {
	tests := map[string]func(*Client, *deliveryManifest){
		"schema":            func(c *Client, d *deliveryManifest) { d.Schema = 2 },
		"inference":         func(c *Client, d *deliveryManifest) { d.Inference = "passed" },
		"clean environment": func(c *Client, d *deliveryManifest) { d.CleanEnvironment = "verified" },
		"duplicate":         func(c *Client, d *deliveryManifest) { d.Files = append(d.Files, d.Files[0]) },
		"case duplicate": func(c *Client, d *deliveryManifest) {
			f := d.Files[0]
			f.Path = strings.ToUpper(f.Path)
			d.Files = append(d.Files, f)
		},
		"escape":                func(c *Client, d *deliveryManifest) { d.Files[0].Path = "../center.exe" },
		"absolute":              func(c *Client, d *deliveryManifest) { d.Files[0].Path = c.exe() },
		"self":                  func(c *Client, d *deliveryManifest) { d.Files[0].Path = "model-center/delivery.json" },
		"center missing":        func(c *Client, d *deliveryManifest) { d.Files = d.Files[1:] },
		"layout missing":        func(c *Client, d *deliveryManifest) { d.Files = append(d.Files[:1], d.Files[2:]...) },
		"wrong exact center":    func(c *Client, d *deliveryManifest) { d.Files[0].Path = strings.ToUpper(centerRelative) },
		"center digest binding": func(c *Client, d *deliveryManifest) { c.cfg.CenterSHA256 = strings.Repeat("a", 64) },
		"size":                  func(c *Client, d *deliveryManifest) { d.Files[0].Bytes = "1" },
		"size noncanonical":     func(c *Client, d *deliveryManifest) { d.Files[0].Bytes = "01" },
		"size negative":         func(c *Client, d *deliveryManifest) { d.Files[0].Bytes = "-1" },
		"size overflow":         func(c *Client, d *deliveryManifest) { d.Files[0].Bytes = "9223372036854775808" },
		"sha syntax":            func(c *Client, d *deliveryManifest) { d.Files[2].SHA256 = "bad" },
		"sha mismatch":          func(c *Client, d *deliveryManifest) { d.Files[2].SHA256 = strings.Repeat("a", 64) },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			c, d, _ := deliveryFixture(t)
			change(c, &d)
			saveDelivery(t, c, d)
			if _, e := c.verifyDelivery(context.Background()); e == nil {
				t.Fatal("accepted trusted but invalid delivery")
			}
		})
	}
}
func TestWindowsDeliveryLayoutAndHash(t *testing.T) {
	for _, name := range []string{"valid", "center", "components", "schema", "root kind", "null roots", "null haar", "root escape", "haar absolute", "root file", "haar directory", "missing root", "layout tampered", "exe tampered", "report tampered", "delivery digest", "note", "duplicate key"} {
		t.Run(name, func(t *testing.T) {
			c, d, l := deliveryFixture(t)
			switch name {
			case "center":
				l.Center = "model-center/other.exe"
			case "components":
				l.Components = "other"
			case "schema":
				l.Schema = 2
			case "root kind":
				l.RootKind = "development"
			case "null roots":
				l.ModelRoots = nil
			case "null haar":
				l.HaarFiles = nil
			case "root escape":
				l.ModelRoots = []string{"../outside"}
			case "haar absolute":
				l.HaarFiles = []string{c.exe()}
			case "root file":
				l.ModelRoots = []string{"haar.xml"}
			case "haar directory":
				l.HaarFiles = []string{"models"}
			case "missing root":
				l.ModelRoots = []string{"absent"}
			case "valid":
				l.ModelRoots = []string{"models"}
				l.HaarFiles = []string{"haar.xml"}
			}
			saveLayout(t, c, &d, l)
			switch name {
			case "layout tampered":
				path := filepath.Join(c.cfg.InstallRoot, "resource-layout.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw[0] = ' '
				writeDeliveryFile(t, path, raw)
			case "exe tampered":
				writeDeliveryFile(t, c.exe(), []byte(strings.Repeat("x", len("center fixture, not executable"))))
			case "report tampered":
				writeDeliveryFile(t, filepath.Join(c.cfg.InstallRoot, "components/test/startup-report.json"), []byte("[]"))
			case "delivery digest":
				c.cfg.DeliverySHA256 = strings.Repeat("a", 64)
			case "note", "duplicate key":
				b, _ := os.ReadFile(filepath.Join(c.cfg.InstallRoot, "resource-layout.json"))
				field := `"note":"forbidden",`
				if name == "duplicate key" {
					field = `"schema":1,`
				}
				b = append([]byte("{"+field), b[1:]...)
				writeDeliveryFile(t, filepath.Join(c.cfg.InstallRoot, "resource-layout.json"), b)
				d.Files[1].Bytes = strconv.Itoa(len(b))
				d.Files[1].SHA256 = deliveryDigest(b)
				saveDelivery(t, c, d)
			}
			_, e := c.verifyDelivery(context.Background())
			if name == "valid" {
				if e != nil {
					t.Fatal(e)
				}
			} else if e == nil {
				t.Fatal("accepted invalid layout/hash")
			}
		})
	}
}
func TestWindowsDeliveryStrictJSON(t *testing.T) {
	for _, suffix := range []string{`,"extra":true}`, `,"schema":1}`, `} {}`} {
		t.Run(suffix, func(t *testing.T) {
			c, _, _ := deliveryFixture(t)
			b, _ := os.ReadFile(c.delivery())
			b = append(b[:len(b)-1], []byte(suffix)...)
			writeDeliveryFile(t, c.delivery(), b)
			c.cfg.DeliverySHA256 = deliveryDigest(b)
			if _, e := c.verifyDelivery(context.Background()); e == nil {
				t.Fatal("accepted invalid JSON")
			}
		})
	}
}
func TestWindowsDeliveryCacheInvalidation(t *testing.T) {
	for _, rel := range []string{"resource-layout.json", "components/test/manifest.json", "components/test/startup-report.json", "model-center/delivery.json", centerRelative, "haar.xml", "models"} {
		t.Run(rel, func(t *testing.T) {
			c, d, l := deliveryFixture(t)
			l.ModelRoots = []string{"models"}
			l.HaarFiles = []string{"haar.xml"}
			saveLayout(t, c, &d, l)
			files, e := c.verifyDelivery(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			id, size, mod, e := metadata(c.delivery())
			if e != nil {
				t.Fatal(e)
			}
			p := processIdentity{PID: 123, Image: c.exe(), Created: "creation"}
			c.cache = cacheEntry{Identity: p, Expected: c.cfg.CenterSHA256, Delivery: c.cfg.DeliverySHA256, DeliveryID: id, DeliverySize: size, DeliveryModified: mod, Files: files}
			if e = c.verify(context.Background(), p, false); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(c.cfg.InstallRoot, rel)
			if rel == "models" {
				if e = os.Remove(path); e != nil {
					t.Fatal(e)
				}
			} else {
				writeDeliveryFile(t, path, []byte("changed payload"))
			}
			if e = c.verify(context.Background(), p, false); e != ErrUnconfirmed {
				t.Fatalf("cache not invalidated: %v", e)
			}
			if c.cache.Expected != "" {
				t.Fatal("stale cache retained")
			}
		})
	}
}
func TestWindowsDeliveryRejectLinks(t *testing.T) {
	c, d, l := deliveryFixture(t)
	target := t.TempDir()
	link := filepath.Join(c.cfg.InstallRoot, "linked")
	if e := os.Symlink(target, link); e != nil {
		t.Skipf("symlink unavailable: %v", e)
	}
	l.ModelRoots = []string{"linked"}
	saveLayout(t, c, &d, l)
	if _, e := c.verifyDelivery(context.Background()); e == nil {
		t.Fatal("accepted linked model root")
	}
	l.ModelRoots = []string{}
	l.HaarFiles = []string{"linked/haar.xml"}
	writeDeliveryFile(t, filepath.Join(target, "haar.xml"), []byte("haar"))
	saveLayout(t, c, &d, l)
	if _, e := c.verifyDelivery(context.Background()); e == nil {
		t.Fatal("accepted linked haar ancestor")
	}
}
