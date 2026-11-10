package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aura-bootstrap/fengshen_model_center/integration"
)

func TestEngineTrustBuild(t *testing.T) {
	root, _ := deliveryFixture(t)
	ctx := context.Background()
	if e := delivery(ctx, root); e != nil {
		t.Fatal(e)
	}
	dp := filepath.Join(root, "model-center", "delivery.json")
	before, _ := os.ReadFile(dp)
	center, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(centerPath)))
	cs := fmt.Sprintf("%x", sha256.Sum256(center))
	ds := fmt.Sprintf("%x", sha256.Sum256(before))
	template := filepath.Join(t.TempDir(), "engines.json")
	put(t, filepath.Join(root, "fengshen-engine.exe"), []byte("compiled engine fixture; not executed"))
	valid := `{"schema":1,"engines":[{"inputpath":"fengshen-engine.exe","edition":"developer","management":true}]}`
	for _, bad := range []string{`{}`, `{"schema":1,"engines":null}`, `{"schema":1,"engines":[{"inputpath":"../outside.exe","edition":"user","management":false}]}`, `{"schema":1,"engines":[{"inputpath":"fengshen-engine.exe","edition":"user","management":true}]}`, `{"schema":1,"Engines":[]}`, `{"schema":1,"schema":1,"engines":[]}`} {
		put(t, template, []byte(bad))
		if engineTrust(ctx, root, template, cs, ds) == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	put(t, template, []byte(valid))
	if engineTrust(ctx, root, template, "", ds) == nil {
		t.Fatal("missing expected digest accepted")
	}
	if engineTrust(ctx, root, template, ds, ds) == nil {
		t.Fatal("wrong center accepted")
	}
	if e := engineTrust(ctx, root, template, cs, ds); e != nil {
		t.Fatal(e)
	}
	engines, e := integration.LoadEngineTrust(ctx, root, cs, ds)
	if e != nil || len(engines) != 1 || engines[0].Edition != "developer" || !engines[0].Management {
		t.Fatalf("%+v %v", engines, e)
	}
	b, _ := os.ReadFile(filepath.Join(root, "fengshen-engine.exe"))
	if engines[0].SHA256 != fmt.Sprintf("%x", sha256.Sum256(b)) {
		t.Fatal("not actual engine hash")
	}
	if engineTrust(ctx, root, template, cs, ds) == nil {
		t.Fatal("overwrote trust")
	}
	if e := delivery(ctx, root); e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(dp)
	if string(before) != string(after) {
		t.Fatal("delivery changed after post-build trust")
	}
}
