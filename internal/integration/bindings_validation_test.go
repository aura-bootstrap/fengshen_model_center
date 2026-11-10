package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func validBindingSelection() BindingSelection {
	return BindingSelection{ExpectedRevision: 7, InventoryID: "inventory_1", Files: []BindingFileSelection{{ModelID: "local-text", FileID: "file_1"}}, Component: ExecutionComponentRef{ID: "llama-runtime", Version: "v1", ManifestSHA256: strings.Repeat("a", 64), Protocol: "fengshen.llama/v1"}, Device: "cpu"}
}
func TestBindingPutWireAndSnapshot(t *testing.T) {
	v := validBindingSelection()
	snapshot := BindingSnapshot{SchemaVersion: 1, Revision: 8, Bindings: map[string]BindingView{"highlight.score": {Ref: ExecutionBindingRef{Scene: "highlight.score", Revision: 8, SHA256: strings.Repeat("b", 64)}, Component: v.Component, InventoryID: v.InventoryID, Files: v.Files, Device: v.Device}}}
	raw, _ := json.Marshal(snapshot)
	f := &bindingBoundaryFake{status: 200, body: string(raw)}
	result, err := NewBindingsClient(f).PutBinding(context.Background(), "highlight.score", v)
	if err != nil || result.Revision != 8 || f.method != "PUT" || f.path != BindingsPath+"/highlight.score" || f.requests != 1 || f.closes != 1 {
		t.Fatalf("%+v %v", f, err)
	}
	want, _ := json.Marshal(v)
	if f.payload != string(want) {
		t.Fatal(f.payload)
	}
	if _, err := DecodeBindingSelection(want, "highlight.score"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(string(want), `"expected_revision":7,`, "", 1),
		strings.Replace(string(want), `"files":[`, `"files":null,"extra":[`, 1),
		strings.Replace(string(want), `"model_id":"local-text"`, `"model_id":"local-text","model_id":"local-text"`, 1),
		strings.Replace(string(want), `"file_id":"file_1"`, `"file_id":null`, 1),
		strings.Replace(string(want), `"device":"cpu"`, `"Device":"cpu"`, 1),
		strings.Replace(string(want), `"device":"cpu"`, `"device":"cpu --exec"`, 1),
		strings.Replace(string(want), `"file_id":"file_1"`, `"file_id":"C:/private"`, 1),
		strings.Replace(string(want), `"version":"v1"`, `"version":"v1","path":"secret"`, 1),
	} {
		if _, err := DecodeBindingSelection([]byte(bad), "highlight.score"); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
func TestBindingSceneComposition(t *testing.T) {
	for _, scene := range RuntimeScenes() {
		v := validBindingSelection()
		v.Component.ID = scene.Components[0]
		v.Files = nil
		for i, id := range scene.RequiredModels {
			v.Files = append(v.Files, BindingFileSelection{ModelID: id, FileID: strings.Repeat("f", i+1)})
		}
		if v.Validate(scene.ID) != nil {
			t.Fatal(scene.ID)
		}
		v.Files = append(v.Files, v.Files[0])
		if v.Validate(scene.ID) == nil {
			t.Fatal("duplicate file", scene.ID)
		}
	}
	v := validBindingSelection()
	v.Component.ID = "demucs-runner"
	v.Files = []BindingFileSelection{{ModelID: "demucs", FileID: "a"}, {ModelID: "demucs-ft", FileID: "b"}}
	if v.Validate("timbre.separate") == nil {
		t.Fatal("mixed alternatives")
	}
	v = validBindingSelection()
	v.Component.ID = "seedvc-runner"
	v.Files = []BindingFileSelection{{ModelID: "seed-vc", FileID: "a"}}
	if v.Validate("timbre.convert") == nil {
		t.Fatal("incomplete suite")
	}
}
func TestBindingSnapshotStoreDiagnosticProjection(t *testing.T) {
	f := &bindingBoundaryFake{status: 200, body: `{"schema_version":1,"revision":0,"bindings":{},"error":"bindings_external_change: C:/secret stdout token"}`}
	v, err := NewBindingsClient(f).Bindings(context.Background())
	if err != nil || v.Error != "bindings_external_change" {
		t.Fatal(v, err)
	}
}
