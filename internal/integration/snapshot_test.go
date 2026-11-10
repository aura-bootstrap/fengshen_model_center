package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSnapshotNewEvidenceContract(t *testing.T) {
	now := time.Now()
	base := func() DependencySnapshot {
		return DependencySnapshot{Schema: 1, FinishedAt: now,
			Components: []Evidence{{ID: "demucs-runner", Packaging: "pyinstaller-onedir", Platform: "matched", Files: "complete", Integrity: "matched", StartupTest: "passed", StartupBuildBound: true, ProtocolEvidence: "not_declared", Inference: "not_tested"}},
			Models:     []Evidence{{ID: "demucs", Discovery: "found", Composition: "complete", Dependency: "not_applicable", NonEmpty: true, Inference: "not_tested"}}}
	}
	good := base()
	b, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	var wire DependencySnapshot
	if err = decode(strings.NewReader(string(b)), MaxResponse, &wire); err != nil {
		t.Fatal(err)
	}
	if err = validateSnapshot(&wire, now); err != nil || !wire.Models[0].NonEmpty || !wire.Components[0].StartupBuildBound {
		t.Fatalf("%+v %v", wire, err)
	}
	tests := map[string]func(*DependencySnapshot){
		"dependency":          func(s *DependencySnapshot) { s.Models[0].Dependency = "assumed" },
		"empty":               func(s *DependencySnapshot) { s.Models[0].Discovery = "empty_file" },
		"partial":             func(s *DependencySnapshot) { s.Models[0].Composition = "incomplete" },
		"integrity":           func(s *DependencySnapshot) { s.Components[0].Integrity = "not_checked" },
		"startup":             func(s *DependencySnapshot) { s.Components[0].StartupTest = "not_tested" },
		"platform":            func(s *DependencySnapshot) { s.Components[0].Platform = "mismatch" },
		"files":               func(s *DependencySnapshot) { s.Components[0].Files = "incomplete" },
		"protocol":            func(s *DependencySnapshot) { s.Components[0].ProtocolEvidence = "verified" },
		"duplicate-model":     func(s *DependencySnapshot) { s.Models = append(s.Models, s.Models[0]) },
		"duplicate-component": func(s *DependencySnapshot) { s.Components = append(s.Components, s.Components[0]) },
		"kind-model":          func(s *DependencySnapshot) { s.Models[0].StartupBuildBound = true },
		"kind-component":      func(s *DependencySnapshot) { s.Components[0].NonEmpty = true },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			s := base()
			change(&s)
			if validateSnapshot(&s, now) == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	for _, size := range []int{64, 65} {
		s := base()
		s.Components = nil
		for i := 0; i < size; i++ {
			s.Components = append(s.Components, Evidence{ID: "component-" + strings.Repeat("x", i), Inference: "not_tested"})
		}
		if err := validateSnapshot(&s, now); (err == nil) != (size == 64) {
			t.Fatalf("component limit %d: %v", size, err)
		}
	}
	for _, malformed := range []string{
		strings.Replace(string(b), `"nonempty":true`, `"nonempty":"true"`, 1),
		strings.Replace(string(b), `"startup_build_bound":true`, `"startup_build_bound":1`, 1),
		strings.Replace(string(b), `"dependency":"not_applicable"`, `"dependency":true`, 1),
	} {
		var invalid DependencySnapshot
		if decode(strings.NewReader(malformed), MaxResponse, &invalid) == nil {
			t.Fatal("incorrect field type accepted")
		}
	}
	for _, dependency := range []string{"", "unknown", "not_checked", "not_applicable", "files_found", "incomplete"} {
		s := base()
		s.Models[0].Dependency = dependency
		if err := validateSnapshot(&s, now); err != nil {
			t.Fatal(dependency, err)
		}
	}
}
