package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
)

func TestMetadataEntryProcess(t *testing.T) {
	if os.Getenv("FENGSHEN_TEST_METADATA_ENTRY") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			flag.CommandLine = flag.NewFlagSet("metadata-entry", flag.ExitOnError)
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestMetadataEntriesDoNotStartCenter(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args  []string
		id    string
		valid bool
	}{
		{[]string{"--haar-component-info"}, "haar-consumer", true},
		{[]string{"--component-info"}, "model-center", true},
		{[]string{"--haar-component-info", "--component-info"}, "", false},
		{[]string{"--haar-component-info=true"}, "", false},
		{[]string{"--haar-component-info=false", "--component-info"}, "", false},
		{[]string{"--haar-component-info", "--root=missing"}, "", false},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, append([]string{"-test.run=^TestMetadataEntryProcess$", "--"}, tc.args...)...)
		cmd.Env = append(os.Environ(), "FENGSHEN_TEST_METADATA_ENTRY=1")
		cmd.Dir = t.TempDir() // No project, layout, XML or model tree exists.
		output, err := cmd.Output()
		if !tc.valid {
			if err == nil || len(output) != 0 {
				t.Fatalf("non-single metadata argv accepted: %v %s %v", tc.args, output, err)
			}
			continue
		}
		var info struct {
			ComponentID string `json:"component_id"`
			Inference   string `json:"inference"`
		}
		if err != nil {
			t.Fatalf("metadata entry %v: %v", tc.args, err)
		}
		if err := json.Unmarshal(output, &info); err != nil || info.ComponentID != tc.id || info.Inference != "not_tested" {
			t.Fatalf("metadata entry: %s %v", output, err)
		}
		entries, err := os.ReadDir(cmd.Dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("metadata entry wrote startup state: %v %v", entries, err)
		}
	}
}

func TestHaarComponentInfoIsEmbeddedMetadataOnly(t *testing.T) {
	for _, haar := range []bool{false, true} {
		var output bytes.Buffer
		if haar {
			writeHaarComponentInfo(&output)
		} else {
			writeComponentInfo(&output)
		}
		var info struct {
			Schema      int      `json:"schema"`
			ComponentID string   `json:"component_id"`
			Version     string   `json:"version"`
			OS          string   `json:"os"`
			Arch        string   `json:"arch"`
			Packaging   string   `json:"packaging"`
			Inference   string   `json:"inference"`
			Protocols   []string `json:"protocols"`
			Embedded    bool     `json:"embedded"`
		}
		if err := json.Unmarshal(output.Bytes(), &info); err != nil {
			t.Fatal(err)
		}
		if info.Schema != 1 || info.Version != buildVersion || info.OS != runtime.GOOS || info.Arch != runtime.GOARCH || info.Packaging != "go" || info.Inference != "not_tested" {
			t.Fatalf("invalid metadata: %+v", info)
		}
		if haar {
			if info.ComponentID != "haar-consumer" || !info.Embedded || len(info.Protocols) != 1 || info.Protocols[0] != "haar/v1" {
				t.Fatalf("invalid embedded Haar metadata: %+v", info)
			}
		} else if info.ComponentID != "model-center" || info.Embedded || strings.Join(info.Protocols, ",") != strings.Join([]string{componentprotocol.StreamWSv1, componentprotocol.BatchHTTPv1, componentprotocol.TimbreHTTPv1, componentprotocol.HaarHTTPv1, componentprotocol.LlamaHTTPv1}, ",") {
			t.Fatalf("center metadata changed: %+v", info)
		}
	}
}
