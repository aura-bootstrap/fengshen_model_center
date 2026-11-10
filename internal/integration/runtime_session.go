package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// SessionScope is a non-secret task scope. It records which session/task owns a
// request and which frozen bindings the caller declared; it is not a credential.
type SessionScope struct {
	SessionID  string                `json:"session_id"`
	InstanceID string                `json:"instance_id"`
	TaskID     string                `json:"task_id"`
	Bindings   []ExecutionBindingRef `json:"bindings"`
}

func (g SessionScope) MarshalJSON() ([]byte, error) {
	type wire SessionScope
	if g.Bindings == nil {
		g.Bindings = []ExecutionBindingRef{}
	}
	return json.Marshal(wire(g))
}
func (g SessionScope) Allows(task string, ref ExecutionBindingRef) bool {
	if g.TaskID != task || ref.Validate() != nil {
		return false
	}
	for _, b := range g.Bindings {
		if b == ref {
			return true
		}
	}
	return false
}
func copyScope(g SessionScope) SessionScope {
	g.Bindings = append([]ExecutionBindingRef(nil), g.Bindings...)
	return g
}

// strictSessionJSON rejects duplicate names (including escaped spellings), null
// at any depth, unknown fields and trailing values.
func strictSessionJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		t, e := d.Token()
		if e != nil {
			return e
		}
		if t == nil {
			return errors.New("ipc_null_denied")
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				if e != nil {
					return e
				}
				s, ok := key.(string)
				if !ok || seen[s] {
					return errors.New("ipc_duplicate_field")
				}
				seen[s] = true
				if e = walk(); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := walk(); e != nil {
					return e
				}
			}
		default:
			return errors.New("ipc_json_invalid")
		}
		_, e = d.Token()
		return e
	}
	if e := walk(); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("ipc_trailing_json")
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return errors.New("ipc_object_required")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out)
}
