// Package components inventories read-only delivery metadata; it never executes code.
package components

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

const MaxManifestBytes = 256 * 1024

var IDs = []string{"model-center", "whisper-batch", "whisper-stream", "demucs-runner", "seedvc-runner", "haar-consumer", "llama-runtime"}

type File struct {
	Path   string `json:"path"`
	Bytes  string `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type Dependency struct {
	ComponentID string `json:"component_id"`
	Version     string `json:"version"`
}
type BuildInput struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	License string `json:"license"`
}
type Manifest struct {
	Schema              int          `json:"schema"`
	ComponentID         string       `json:"component_id"`
	Version             string       `json:"version"`
	OS                  string       `json:"os"`
	Arch                string       `json:"arch"`
	EntryType           string       `json:"entry_type"`
	Entry               string       `json:"entry"`
	Packaging           string       `json:"packaging"`
	Protocols           []string     `json:"protocols"`
	Files               []File       `json:"files"`
	RuntimeDependencies []Dependency `json:"runtime_dependencies"`
	ModelRefs           []string     `json:"model_refs"`
	DriverRequirements  []string     `json:"driver_requirements"`
	BuildInputs         []BuildInput `json:"build_inputs"`
	StartupReportRef    string       `json:"startup_report_ref,omitempty"`
}

func SafeRelative(s string) bool {
	if s == "" || s == "." || path.Clean(s) != s || strings.ContainsAny(s, "\\:\x00") || strings.HasPrefix(s, "/") {
		return false
	}
	for _, p := range strings.Split(s, "/") {
		if p == ".." || strings.TrimRight(p, " .") != p {
			return false
		}
		stem := strings.ToUpper(strings.SplitN(p, ".", 2)[0])
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || stem == "CONIN$" || stem == "CONOUT$" || len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '0' && stem[3] <= '9' {
			return false
		}
	}
	return true
}
func identifier(s string) bool { return SafeRelative(s) && !strings.Contains(s, "/") && len(s) <= 128 }

// StrictJSON also rejects duplicate object keys, including nested keys.
func StrictJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	if err := jsonValue(d); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("extra JSON content")
	}
	d = json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func jsonValue(d *json.Decoder) error {
	t, e := d.Token()
	if e != nil {
		return e
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := k.(string)
			if !ok || seen[s] {
				return fmt.Errorf("duplicate JSON key")
			}
			seen[s] = true
			if e = jsonValue(d); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e = jsonValue(d); e != nil {
				return e
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, e = d.Token()
	return e
}
func ParseManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	b, e := io.ReadAll(io.LimitReader(r, MaxManifestBytes+1))
	if e != nil {
		return m, e
	}
	if len(b) > MaxManifestBytes {
		return m, fmt.Errorf("manifest exceeds 256 KiB")
	}
	if e = StrictJSON(b, &m); e != nil {
		return m, e
	}
	return m, Validate(m)
}
func Validate(m Manifest) error {
	known := false
	for _, id := range IDs {
		known = known || id == m.ComponentID
	}
	if m.Schema != 1 || !known || !identifier(m.Version) || m.OS == "" || m.Arch == "" {
		return fmt.Errorf("invalid component identity/schema/platform")
	}
	if m.Packaging != "go" && m.Packaging != "native" && m.Packaging != "pyinstaller-onedir" {
		return fmt.Errorf("not a delivered binary packaging")
	}
	ext := strings.ToLower(path.Ext(m.Entry))
	if ext != ".exe" && ext != ".dll" && ext != ".so" && ext != ".dylib" && !(m.OS != "windows" && ext == "") {
		return fmt.Errorf("source/script is not a binary entry")
	}
	if m.EntryType != "executable" && m.EntryType != "library" {
		return fmt.Errorf("invalid binary entry type")
	}
	if len(m.Files) == 0 || len(m.Files) > 100000 {
		return fmt.Errorf("invalid file count")
	}
	seen := map[string]bool{}
	entry := false
	for _, f := range m.Files {
		k := strings.ToLower(f.Path)
		n, e := strconv.ParseInt(f.Bytes, 10, 64)
		h, he := hex.DecodeString(f.SHA256)
		if !SafeRelative(f.Path) || k == "manifest.json" || k == strings.ToLower(m.StartupReportRef) || seen[k] || e != nil || n <= 0 || strconv.FormatInt(n, 10) != f.Bytes || he != nil || len(h) != 32 {
			return fmt.Errorf("invalid/duplicate file: %q", f.Path)
		}
		seen[k] = true
		entry = entry || f.Path == m.Entry
	}
	if !entry {
		return fmt.Errorf("entry not in files")
	}
	if m.StartupReportRef != "" && (!SafeRelative(m.StartupReportRef) || strings.EqualFold(m.StartupReportRef, "manifest.json")) {
		return fmt.Errorf("invalid report reference")
	}
	deps := map[string]bool{}
	for _, d := range m.RuntimeDependencies {
		k := d.ComponentID + "/" + d.Version
		if !identifier(d.ComponentID) || !identifier(d.Version) || deps[k] || k == m.ComponentID+"/"+m.Version {
			return fmt.Errorf("duplicate/cyclic dependency")
		}
		deps[k] = true
	}
	return nil
}
