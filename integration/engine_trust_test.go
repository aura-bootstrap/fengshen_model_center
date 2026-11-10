package integration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEngineTrustLoad(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "model-center"), 0700)
	p := filepath.Join(root, "model-center", "engine-trust.json")
	digest := strings.Repeat("a", 64)
	if _, e := LoadEngineTrust(context.Background(), root, digest, digest); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("missing: %v", e)
	}
	os.WriteFile(filepath.Join(root, "engine.exe"), []byte("not hashed during loading"), 0600)
	raw := `{"schema":1,"center_sha256":"` + digest + `","delivery_sha256":"` + digest + `","engines":[{"path":"engine.exe","sha256":"` + digest + `","edition":"user","management":false}]}`
	os.WriteFile(p, []byte(raw), 0600)
	got, e := LoadEngineTrust(context.Background(), root, digest, digest)
	if e != nil || len(got) != 1 || got[0].Path != filepath.Join(root, "engine.exe") {
		t.Fatalf("load: %+v %v", got, e)
	}
	cases := []string{
		strings.Replace(raw, `"schema":1`, `"schema":1,"Schema":1`, 1),
		strings.Replace(raw, `"schema":1`, `"Schema":1`, 1),
		strings.Replace(raw, `"schema":1`, `"schema":null`, 1),
		strings.Replace(raw, `"schema":1`, `"schema":1,"schema":1`, 1),
		strings.Replace(raw, `"schema":1`, `"schema":2`, 1),
		strings.Replace(raw, `"management":false`, `"management":true`, 1),
		strings.Replace(raw, `"management":false`, `"management":null`, 1),
		strings.Replace(raw, `,"management":false`, ``, 1),
		strings.Replace(raw, `"path":"engine.exe"`, `"path":"engine.exe","Path":"engine.exe"`, 1),
		strings.Replace(raw, `"management":false`, `"extra":false`, 1),
		strings.Replace(raw, `engine.exe`, `../engine.exe`, 1),
		strings.Replace(raw, `engine.exe`, `engine.cmd`, 1),
		strings.Replace(raw, `"edition":"user"`, `"edition":"dev"`, 1),
		raw + ` {}`, string(append([]byte(raw), 255)), strings.Repeat(" ", 65537),
	}
	var doc map[string]json.RawMessage
	json.Unmarshal([]byte(raw), &doc)
	for _, eng := range []string{`[]`, `null`, `[` + strings.Repeat(string(doc["engines"])[1:len(doc["engines"])-1]+",", 32) + string(doc["engines"])[1:len(doc["engines"])-1] + `]`, `[` + string(doc["engines"])[1:len(doc["engines"])-1] + `,` + strings.Replace(string(doc["engines"])[1:len(doc["engines"])-1], "engine.exe", "ENGINE.exe", 1) + `]`} {
		d := map[string]json.RawMessage{}
		for k, v := range doc {
			d[k] = v
		}
		d["engines"] = json.RawMessage(eng)
		b, _ := json.Marshal(d)
		cases = append(cases, string(b))
	}
	for i, b := range cases {
		os.WriteFile(p, []byte(b), 0600)
		if _, e := LoadEngineTrust(context.Background(), root, digest, digest); e == nil {
			t.Errorf("accepted invalid %d", i)
		}
	}
	os.WriteFile(p, []byte(raw), 0600)
	for _, want := range []string{"", strings.Repeat("A", 64), strings.Repeat("b", 64)} {
		if _, e := LoadEngineTrust(context.Background(), root, want, digest); e == nil {
			t.Errorf("accepted expected digest %q", want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := LoadEngineTrust(ctx, root, digest, digest); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancel: %v", e)
	}
	os.Remove(filepath.Join(root, "engine.exe"))
	if _, e := LoadEngineTrust(context.Background(), root, digest, digest); e == nil || errors.Is(e, os.ErrNotExist) {
		t.Fatalf("missing listed engine must be corruption, not absent trust: %v", e)
	}
	os.Mkdir(filepath.Join(root, "engine.exe"), 0700)
	if _, e := LoadEngineTrust(context.Background(), root, digest, digest); e == nil {
		t.Fatal("directory accepted")
	}
}

func TestEngineTrustLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.Mkdir(filepath.Join(root, "model-center"), 0700)
	digest := strings.Repeat("a", 64)
	os.WriteFile(filepath.Join(outside, "engine.exe"), []byte("engine"), 0600)
	if e := os.Symlink(filepath.Join(outside, "engine.exe"), filepath.Join(root, "engine.exe")); e != nil {
		t.Skip(e)
	}
	raw := `{"schema":1,"center_sha256":"` + digest + `","delivery_sha256":"` + digest + `","engines":[{"path":"engine.exe","sha256":"` + digest + `","edition":"admin","management":true}]}`
	os.WriteFile(filepath.Join(root, "model-center", "engine-trust.json"), []byte(raw), 0600)
	if _, e := LoadEngineTrust(context.Background(), root, digest, digest); e == nil {
		t.Fatal("linked engine accepted")
	}
	os.Remove(filepath.Join(root, "engine.exe"))
	os.WriteFile(filepath.Join(root, "engine.exe"), []byte("engine"), 0600)
	alias := filepath.Join(t.TempDir(), "install")
	if e := os.Symlink(root, alias); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadEngineTrust(context.Background(), alias, digest, digest); e == nil {
		t.Fatal("linked install root accepted")
	}
	trust := filepath.Join(root, "model-center", "engine-trust.json")
	os.Rename(trust, filepath.Join(outside, "trust.json"))
	if e := os.Symlink(filepath.Join(outside, "trust.json"), trust); e != nil {
		t.Fatal(e)
	}
	if _, e := LoadEngineTrust(context.Background(), root, digest, digest); e == nil {
		t.Fatal("linked trust accepted")
	}
}
