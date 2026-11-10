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
func deliveryFixture(t *testing.T) (*Client, deliveryManifest) {
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
	d := deliveryManifest{Schema: 1, Inference: "not_tested", CleanEnvironment: "not_verified"}
	for _, rel := range []string{centerRelative, "components/test/manifest.json", "components/test/startup-report.json"} {
		raw, e := os.ReadFile(filepath.Join(c.cfg.InstallRoot, rel))
		if e != nil {
			t.Fatal(e)
		}
		d.Files = append(d.Files, deliveryFile{rel, strconv.Itoa(len(raw)), deliveryDigest(raw)})
	}
	saveDelivery(t, c, d)
	return c, d
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
			c, d := deliveryFixture(t)
			change(c, &d)
			saveDelivery(t, c, d)
			if _, e := c.verifyDelivery(context.Background()); e == nil {
				t.Fatal("accepted trusted but invalid delivery")
			}
		})
	}
}
func TestWindowsDeliveryStrictJSON(t *testing.T) {
	for _, suffix := range []string{`,"extra":true}`, `,"schema":1}`, `} {}`} {
		t.Run(suffix, func(t *testing.T) {
			c, _ := deliveryFixture(t)
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
	for _, rel := range []string{"components/test/manifest.json", "components/test/startup-report.json", "model-center/delivery.json", centerRelative} {
		t.Run(rel, func(t *testing.T) {
			c, _ := deliveryFixture(t)
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
			writeDeliveryFile(t, path, []byte("changed payload"))
			if e = c.verify(context.Background(), p, false); e != ErrUnconfirmed {
				t.Fatalf("cache not invalidated: %v", e)
			}
			if c.cache.Expected != "" {
				t.Fatal("stale cache retained")
			}
		})
	}
}
