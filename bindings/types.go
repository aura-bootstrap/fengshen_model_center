// Package bindings persists Manager-resolved execution selections. It never opens
// model files and does not attest compatibility, delivery, or successful inference.
package bindings

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aura-bootstrap/fengshen_model_center/integration"
	"github.com/aura-bootstrap/fengshen_model_center/safefs"
)

const (
	MaxBytes = 256 * 1024
	MaxFiles = 256
)

var (
	ErrConflict = errors.New("bindings_conflict: binding revision changed")
	ErrReadOnly = errors.New("bindings_read_only: binding store is frozen")
	ErrCorrupt  = errors.New("bindings_corrupt: invalid persisted bindings")
	ErrInvalid  = errors.New("center_binding_invalid: invalid frozen binding")
)

// FrozenFile is an internal persistence record, not an HTTP path-selection DTO.
// Registration fields are either all absent (a trusted built-in scope) or all set.
// The Manager resolves the inventory, validates complete model composition, and
// records the actual scope and registration identities before calling Put.
// Bytes uses canonical positive decimal int64 text, not a JSON number.
type FrozenFile struct {
	ModelID               string    `json:"model_id"`
	FileID                string    `json:"file_id"`
	Group                 string    `json:"group"`
	ScopePath             string    `json:"scope_path"`
	ScopeIdentity         string    `json:"scope_identity"`
	RelativePath          string    `json:"relative_path"`
	Identity              string    `json:"identity"`
	Bytes                 string    `json:"bytes"`
	Modified              time.Time `json:"modified"`
	RegistrationInputPath string    `json:"registration_input_path"`
	RegistrationPath      string    `json:"registration_path"`
	RegistrationKind      string    `json:"registration_kind"`
	RegistrationIdentity  string    `json:"registration_identity"`
	RegistrationUpdatedAt time.Time `json:"registration_updated_at"`
}

// Binding has no inference-evidence field. Component is a frozen reference only.
// Ref is assigned by the store; a nonzero input Ref must exactly match the current
// stored reference and acts only as an additional optimistic guard.
type Binding struct {
	Scene            string                            `json:"scene"`
	InventoryID      string                            `json:"inventory_id"`
	RegistryRevision uint64                            `json:"registry_revision"`
	Component        integration.ExecutionComponentRef `json:"component"`
	Files            []FrozenFile                      `json:"files"`
	Device           string                            `json:"device"`
	Ref              integration.ExecutionBindingRef   `json:"ref"`
}

type Snapshot struct {
	SchemaVersion int                `json:"schema_version"`
	Revision      uint64             `json:"revision"`
	Bindings      map[string]Binding `json:"bindings"`
	Error         string             `json:"-"`
}

func text(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func deviceValid(s string) bool {
	if s == "cpu" || s == "auto" || s == "cuda" || s == "metal" || s == "vulkan" {
		return true
	}
	for _, prefix := range []string{"cuda:", "vulkan:"} {
		if strings.HasPrefix(s, prefix) {
			n := strings.TrimPrefix(s, prefix)
			i, err := strconv.ParseUint(n, 10, 8)
			return err == nil && strconv.FormatUint(i, 10) == n
		}
	}
	return false
}

func validateBinding(b Binding) error {
	def, ok := integration.RuntimeScene(b.Scene)
	if !ok || !text(b.InventoryID, 128) || b.Component.Validate(b.Scene) != nil || !deviceValid(b.Device) || len(b.Files) == 0 || len(b.Files) > MaxFiles {
		return ErrInvalid
	}
	allowed := map[string]bool{}
	for _, id := range def.RequiredModels {
		allowed[id] = true
	}
	for _, id := range def.OptionalModels {
		allowed[id] = true
	}
	for _, alternative := range def.AlternativeModels {
		for _, id := range alternative {
			allowed[id] = true
		}
	}
	groups := map[string]string{}
	registrations := map[string]FrozenFile{}
	ids, locations, identities := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, f := range b.Files {
		if !allowed[f.ModelID] || !text(f.FileID, 128) || !text(f.Group, 4096) || !text(f.Identity, 512) || f.Modified.IsZero() || f.Modified.Year() < 1 || f.Modified.Year() > 9999 {
			return ErrInvalid
		}
		n, err := strconv.ParseInt(f.Bytes, 10, 64)
		if err != nil || n <= 0 || strconv.FormatInt(n, 10) != f.Bytes {
			return ErrInvalid
		}
		if !text(f.ScopeIdentity, 512) || !text(f.ScopePath, 32768) || safefs.ValidatePath(f.ScopePath) != nil || !text(f.RelativePath, 32768) {
			return ErrInvalid
		}
		// Interpret both separators on every host. Never allow traversal, ADS,
		// device aliases, or a scope-root request to survive persistence.
		rel := strings.ReplaceAll(f.RelativePath, `\`, "/")
		if strings.HasPrefix(rel, "/") || strings.Contains(rel, ":") {
			return ErrInvalid
		}
		for _, part := range strings.Split(rel, "/") {
			if part == "" || part == "." || part == ".." {
				return ErrInvalid
			}
		}
		if !filepath.IsLocal(filepath.FromSlash(rel)) || safefs.ValidatePath(filepath.Join(f.ScopePath, filepath.FromSlash(rel))) != nil {
			return ErrInvalid
		}
		registered := f.RegistrationInputPath != "" || f.RegistrationPath != "" || f.RegistrationKind != "" || f.RegistrationIdentity != "" || !f.RegistrationUpdatedAt.IsZero()
		if registered {
			if !text(f.RegistrationInputPath, 32768) || !text(f.RegistrationPath, 32768) || safefs.ValidatePath(f.RegistrationInputPath) != nil || safefs.ValidatePath(f.RegistrationPath) != nil || (f.RegistrationKind != "file" && f.RegistrationKind != "directory") || !text(f.RegistrationIdentity, 512) || f.RegistrationUpdatedAt.IsZero() || f.RegistrationUpdatedAt.Year() < 1 || f.RegistrationUpdatedAt.Year() > 9999 {
				return ErrInvalid
			}
		}
		if group, exists := groups[f.ModelID]; exists {
			previous := registrations[f.ModelID]
			if group != f.Group || previous.ScopePath != f.ScopePath || previous.ScopeIdentity != f.ScopeIdentity || previous.RegistrationInputPath != f.RegistrationInputPath || previous.RegistrationPath != f.RegistrationPath || previous.RegistrationKind != f.RegistrationKind || previous.RegistrationIdentity != f.RegistrationIdentity || !previous.RegistrationUpdatedAt.Equal(f.RegistrationUpdatedAt) {
				return ErrInvalid
			}
		}
		groups[f.ModelID], registrations[f.ModelID] = f.Group, f
		location := filepath.Clean(filepath.Join(f.ScopePath, filepath.FromSlash(rel)))
		// Windows names are case insensitive; normalizing also prevents ambiguous
		// bindings from becoming acceptable merely by moving a document to Linux.
		location = strings.ToLower(strings.ReplaceAll(location, `\`, "/"))
		if ids[f.FileID] || locations[location] || identities[f.Identity] {
			return ErrInvalid
		}
		ids[f.FileID], locations[location], identities[f.Identity] = true, true, true
	}
	if len(def.AlternativeModels) != 0 {
		matches := 0
		for _, alternative := range def.AlternativeModels {
			complete := len(groups) == len(alternative)
			for _, id := range alternative {
				if _, found := groups[id]; !found {
					complete = false
				}
			}
			if complete {
				matches++
			}
		}
		if matches != 1 {
			return ErrInvalid
		}
	} else {
		for _, id := range def.RequiredModels {
			if _, found := groups[id]; !found {
				return ErrInvalid
			}
		}
	}
	return nil
}

func normalized(b Binding) Binding {
	b.Files = append([]FrozenFile(nil), b.Files...)
	for i := range b.Files {
		b.Files[i].Modified = b.Files[i].Modified.UTC()
		b.Files[i].RegistrationUpdatedAt = b.Files[i].RegistrationUpdatedAt.UTC()
	}
	sort.Slice(b.Files, func(i, j int) bool {
		if b.Files[i].ModelID != b.Files[j].ModelID {
			return b.Files[i].ModelID < b.Files[j].ModelID
		}
		return b.Files[i].FileID < b.Files[j].FileID
	})
	return b
}

// contentHash excludes the entire reference, including revision. Thus an unchanged
// selection has the same digest after a new commit, but never the same reference.
func contentHash(b Binding) (string, error) {
	b = normalized(b)
	b.Ref = integration.ExecutionBindingRef{}
	raw, err := json.Marshal(b)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func clone(state Snapshot) Snapshot {
	m := make(map[string]Binding, len(state.Bindings))
	for scene, b := range state.Bindings {
		b.Files = append([]FrozenFile(nil), b.Files...)
		m[scene] = b
	}
	state.Bindings = m
	return state
}
