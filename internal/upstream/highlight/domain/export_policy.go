package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
)

type ExportDurationSeconds struct {
	Min     int `json:"min"`
	Max     int `json:"max"`
	Default int `json:"default"`
}
type ExportPolicy struct {
	Schema                int                   `json:"schema"`
	ExportDurationSeconds ExportDurationSeconds `json:"export_duration_seconds"`
}
type ExportPolicyRef struct {
	Kind    string `json:"kind"`
	Version int64  `json:"version"`
	SHA256  string `json:"sha256"`
}

// DraftExportPolicy is only a draft for explicit creation, never a read fallback.
func DraftExportPolicy() ExportPolicy {
	return ExportPolicy{Schema: 1, ExportDurationSeconds: ExportDurationSeconds{Min: 20, Max: 120, Default: 60}}
}
func (p ExportPolicy) Validate() error {
	d := p.ExportDurationSeconds
	if p.Schema != 1 || d.Min < 1 || d.Min > d.Default || d.Default > d.Max || d.Max > 600 {
		return errors.New("highlight_export_duration_invalid")
	}
	return nil
}
func (r ExportPolicyRef) Validate() error {
	hash, err := hex.DecodeString(r.SHA256)
	if r.Kind != "highlight_export" || r.Version < 1 || err != nil || len(hash) != 32 || hex.EncodeToString(hash) != r.SHA256 {
		return errors.New("highlight_export_policy_changed")
	}
	return nil
}
func CanonicalExportPolicy(p ExportPolicy) (json.RawMessage, string, error) {
	if err := p.Validate(); err != nil {
		return nil, "", err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256(data)
	return data, hex.EncodeToString(hash[:]), nil
}

// DecodeExportPolicy requires exact field names, integer JSON values and all fields.
// It rejects null and duplicate keys at both object levels before typed decoding.
func DecodeExportPolicy(data []byte) (ExportPolicy, error) {
	var p ExportPolicy
	outer, err := exportPolicyObject(data, []string{"schema", "export_duration_seconds"})
	if err != nil {
		return p, err
	}
	if _, err = exportPolicyObject(outer["export_duration_seconds"], []string{"min", "max", "default"}); err != nil {
		return p, err
	}
	type plain ExportPolicy
	if err = json.Unmarshal(data, (*plain)(&p)); err != nil {
		return p, err
	}
	return p, p.Validate()
}
func (p *ExportPolicy) UnmarshalJSON(data []byte) error {
	decoded, err := DecodeExportPolicy(data)
	if err != nil {
		return err
	}
	*p = decoded
	return nil
}

func exportPolicyObject(data []byte, required []string) (map[string]json.RawMessage, error) {
	invalid := errors.New("highlight_export_duration_invalid")
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, invalid
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err = dec.Token()
		if err != nil {
			return nil, invalid
		}
		key, ok := tok.(string)
		if !ok {
			return nil, invalid
		}
		if _, ok = fields[key]; ok {
			return nil, invalid
		}
		allowed := false
		for _, name := range required {
			if name == key {
				allowed = true
			}
		}
		if !allowed {
			return nil, invalid
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, invalid
		}
		fields[key] = value
	}
	if _, err = dec.Token(); err != nil {
		return nil, invalid
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || len(fields) != len(required) {
		return nil, invalid
	}
	return fields, nil
}
