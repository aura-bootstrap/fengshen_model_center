package integration

import (
	"bytes"
	"encoding/json"
	"log"
	"regexp"
	"strings"
	"testing"
)

func TestDescriptorPhaseDiagnostics(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	for _, phase := range []string{"write", "sync", "close", "publish"} {
		descriptorPhase(phase)()
	}
	want := regexp.MustCompile(`center_descriptor phase=(write|sync|close|publish) event=(start|done) elapsed_ms=[0-9]+$`)
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != 8 {
		t.Fatalf("phase record count=%d", len(lines))
	}
	for i, phase := range []string{"write", "sync", "close", "publish"} {
		if !want.MatchString(lines[2*i]) || !strings.HasSuffix(lines[2*i], "phase="+phase+" event=start elapsed_ms=0") || !want.MatchString(lines[2*i+1]) || !strings.Contains(lines[2*i+1], "phase="+phase+" event=done elapsed_ms=") {
			t.Fatal("missing fixed start/done diagnostics")
		}
	}
	output.Reset()
	for _, phase := range []string{"secret-token", "write\nsecret", strings.Repeat("secret", 1000), `W:\private\instance.json`, "http://private.invalid"} {
		descriptorPhase(phase)()
	}
	if output.Len() != 0 {
		t.Fatal("unapproved phase leaked")
	}
}

func TestStrictProtocol(t *testing.T) {
	for _, s := range []string{`{"schema":1,"extra":true}`, `{"schema":1} {}`, strings.Repeat(" ", 8193)} {
		var d Descriptor
		if decode(bytes.NewBufferString(s), 8192, &d) == nil {
			t.Fatalf("accepted invalid protocol: %.40s", s)
		}
	}
}
func TestLiteralLoopback(t *testing.T) {
	for _, a := range []string{"localhost:18120", "0.0.0.0:18120", "127.0.0.1:0", "127.0.0.1:80/x"} {
		if _, e := canonical(a); e == nil {
			t.Fatal(a)
		}
	}
	for _, a := range []string{"127.0.0.1:18120", "[::1]:18120"} {
		if _, e := canonical(a); e != nil {
			t.Fatal(e)
		}
	}
}
func TestNoCredentialsInDescriptor(t *testing.T) {
	b, _ := json.Marshal(Descriptor{})
	if strings.Contains(string(b), "token") {
		t.Fatal(string(b))
	}
}
