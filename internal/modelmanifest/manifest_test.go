package modelmanifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testManifest(path string) Manifest {
	return Manifest{
		Schema:      1,
		GeneratedAt: "2026-11-10T00:00:00Z",
		Root:        filepath.Dir(path),
		Entries: []Entry{{
			ModelID: "whisper-asr",
			Role:    "weight",
			Path:    path,
			Bytes:   1,
			SHA256:  strings.Repeat("a", 64),
		}},
	}
}

func writeRaw(t *testing.T, dir, name string, raw []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeJSON(t *testing.T, dir, name string, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return writeRaw(t, dir, name, raw)
}

func TestDefaultPathAndEntryFilePath(t *testing.T) {
	if got, want := DefaultPath(`C:\Users\fixture`), filepath.Join(`C:\Users\fixture`, ".fengshen", "model_manifest.json"); got != want {
		t.Fatalf("DefaultPath=%q want=%q", got, want)
	}
	if got := (Entry{Path: "a", NewPath: "b"}).FilePath(); got != "a" {
		t.Fatalf("path precedence=%q", got)
	}
	if got := (Entry{NewPath: "b"}).FilePath(); got != "b" {
		t.Fatalf("new_path fallback=%q", got)
	}
}

func TestLoadWriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	weight := filepath.Join(dir, "ggml-small.bin")
	if err := os.WriteFile(weight, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "nested", "model_manifest.json")
	original := testManifest(weight)
	if err := Write(path, original); err != nil {
		t.Fatal(err)
	}
	loaded, raw, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Schema != 1 || loaded.Root != original.Root || len(loaded.Entries) != 1 || loaded.Entries[0] != original.Entries[0] {
		t.Fatalf("round trip=%+v", loaded)
	}
	if len(raw) == 0 || !strings.HasSuffix(string(raw), "\n") {
		t.Fatalf("raw manifest=%q", raw)
	}
	if Revision(raw) == 0 || Revision(raw) != Revision(append([]byte(nil), raw...)) {
		t.Fatal("revision is not stable")
	}
	if Revision(raw) == Revision(append(raw, '\n')) {
		t.Fatal("revision must change with bytes")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".model-manifest-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary files left: %v %v", matches, err)
	}
}

func TestWriteRejectsInvalidSchemaAndSize(t *testing.T) {
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "bad.json"), Manifest{Schema: 2, Entries: []Entry{}}); err == nil {
		t.Fatal("invalid schema accepted")
	}
	path := filepath.Join(dir, "too-large.json")
	entry := Entry{ModelID: "whisper-asr", Path: filepath.Join(dir, strings.Repeat("x", MaxBytes)), Bytes: 1}
	if err := Write(path, Manifest{Schema: 1, Entries: []Entry{entry}}); err == nil {
		t.Fatal("oversized manifest accepted")
	}
}

func TestLoadRejectsInvalidManifest(t *testing.T) {
	dir := t.TempDir()
	valid := testManifest(filepath.Join(dir, "weight.bin"))
	validRaw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	linkTarget := writeRaw(t, dir, "link-target.json", validRaw)
	linkPath := filepath.Join(dir, "link.json")
	linkErr := os.Symlink(linkTarget, linkPath)
	if linkErr != nil {
		linkPath = ""
	}
	tooLarge := writeRaw(t, dir, "too-large.json", []byte(strings.Repeat(" ", MaxBytes+1)))
	badEntry := func(mutate func(*Entry)) Manifest {
		m := valid
		mutate(&m.Entries[0])
		return m
	}
	cases := []struct {
		name string
		path string
	}{
		{"missing", filepath.Join(dir, "missing.json")},
		{"directory", dir},
		{"empty", writeRaw(t, dir, "empty.json", nil)},
		{"oversized", tooLarge},
		{"invalid-json", writeRaw(t, dir, "invalid.json", []byte("{"))},
		{"schema-zero", writeJSON(t, dir, "schema-zero.json", Manifest{Schema: 0, Entries: valid.Entries})},
		{"schema-two", writeJSON(t, dir, "schema-two.json", Manifest{Schema: 2, Entries: valid.Entries})},
		{"no-entries", writeJSON(t, dir, "no-entries.json", Manifest{Schema: 1})},
		{"empty-model", writeJSON(t, dir, "empty-model.json", badEntry(func(e *Entry) { e.ModelID = "" }))},
		{"upper-model", writeJSON(t, dir, "upper-model.json", badEntry(func(e *Entry) { e.ModelID = "Whisper" }))},
		{"underscore-model", writeJSON(t, dir, "underscore-model.json", badEntry(func(e *Entry) { e.ModelID = "whisper_asr" }))},
		{"long-model", writeJSON(t, dir, "long-model.json", badEntry(func(e *Entry) { e.ModelID = strings.Repeat("a", 65) }))},
		{"empty-path", writeJSON(t, dir, "empty-path.json", badEntry(func(e *Entry) { e.Path, e.NewPath = "", "" }))},
		{"relative-path", writeJSON(t, dir, "relative-path.json", badEntry(func(e *Entry) { e.Path = "weights/model.bin" }))},
		{"unclean-path", writeJSON(t, dir, "unclean-path.json", badEntry(func(e *Entry) { e.Path = dir + `\x\..\y` }))},
		{"negative-bytes", writeJSON(t, dir, "negative-bytes.json", badEntry(func(e *Entry) { e.Bytes = -1 }))},
		{"bad-hash", writeJSON(t, dir, "bad-hash.json", badEntry(func(e *Entry) { e.SHA256 = "zz" }))},
		{"short-hash", writeJSON(t, dir, "short-hash.json", badEntry(func(e *Entry) { e.SHA256 = strings.Repeat("a", 63) }))},
	}
	if linkPath != "" {
		cases = append(cases, struct {
			name string
			path string
		}{"symlink", linkPath})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := Load(tc.path); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

func TestLoadAcceptsNewPathAndOptionalFields(t *testing.T) {
	dir := t.TempDir()
	m := Manifest{Schema: 1, Entries: []Entry{{
		ModelID: "demucs",
		NewPath: filepath.Join(dir, "955717e8-fixture.th"),
	}}}
	path := writeJSON(t, dir, "new-path.json", m)
	loaded, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Entries[0].FilePath(); got != m.Entries[0].NewPath {
		t.Fatalf("new_path=%q", got)
	}
}

func TestSHA256ByPath(t *testing.T) {
	hashA := strings.ToUpper(strings.Repeat("a", 64))
	hashB := strings.Repeat("b", 64)
	m := Manifest{Entries: []Entry{
		{Path: `C:\Weights\A.bin`, SHA256: hashA},
		{NewPath: `C:\Weights\B.bin`, SHA256: hashB},
		{Path: `C:\Weights\C.bin`},
		{Path: ``, SHA256: hashB},
	}}
	index := SHA256ByPath(m)
	if len(index) != 2 {
		t.Fatalf("index=%v", index)
	}
	if got := index[strings.ToLower(`C:\Weights\A.bin`)]; got != strings.ToLower(hashA) {
		t.Fatalf("hash A=%q", got)
	}
	if got := index[strings.ToLower(`C:\Weights\B.bin`)]; got != strings.ToLower(hashB) {
		t.Fatalf("hash B=%q", got)
	}
}
