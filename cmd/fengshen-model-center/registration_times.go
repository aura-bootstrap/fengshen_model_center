package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/bindings"
	"github.com/aura-bootstrap/fengshen_model_center/internal/registry"
)

// Frozen bindings pin registration_updated_at and bindingCurrentLocked demands
// exact equality. Synthetic registrations are revalidated on every startup, so
// without a stable timestamp each restart would stamp time.Now() and silently
// invalidate every existing binding. The timestamp therefore persists per
// registered identity and only advances when the identity actually changes.
type registrationTime struct {
	Identity  string    `json:"identity"`
	UpdatedAt time.Time `json:"updated_at"`
}

type registrationTimes struct {
	SchemaVersion int                         `json:"schema_version"`
	Registrations map[string]registrationTime `json:"registrations"`
}

func registrationTimesPath(store string) string {
	return filepath.Join(store, "registration-times.json")
}

func loadRegistrationTimes(path string) map[string]registrationTime {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var file registrationTimes
	if json.Unmarshal(raw, &file) != nil || file.SchemaVersion != 1 || file.Registrations == nil {
		return nil
	}
	for id, entry := range file.Registrations {
		if id == "" || entry.Identity == "" || entry.UpdatedAt.IsZero() {
			return nil
		}
	}
	return file.Registrations
}

// seedRegistrationTimes recovers timestamps from already frozen bindings so a
// center upgraded onto this scheme keeps existing task bindings valid. The
// earliest frozen timestamp wins when several bindings disagree.
func seedRegistrationTimes(frozen bindings.Snapshot) map[string]registrationTime {
	seeded := map[string]registrationTime{}
	for _, binding := range frozen.Bindings {
		for _, file := range binding.Files {
			if file.ModelID == "" || file.RegistrationIdentity == "" || file.RegistrationUpdatedAt.IsZero() {
				continue
			}
			prev, ok := seeded[file.ModelID]
			if !ok || file.RegistrationUpdatedAt.Before(prev.UpdatedAt) {
				seeded[file.ModelID] = registrationTime{Identity: file.RegistrationIdentity, UpdatedAt: file.RegistrationUpdatedAt}
			}
		}
	}
	return seeded
}

func writeRegistrationTimes(path string, file registrationTimes) {
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".registration-times-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(raw); err != nil {
		tmp.Close()
		return
	}
	if err = tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(name, path)
}

func stabilizeRegistrationTimes(store string, snap registry.Snapshot, frozen bindings.Snapshot) {
	path := registrationTimesPath(store)
	saved := loadRegistrationTimes(path)
	if saved == nil {
		saved = seedRegistrationTimes(frozen)
	}
	next := registrationTimes{SchemaVersion: 1, Registrations: map[string]registrationTime{}}
	for id, reg := range snap.Registrations {
		if prev, ok := saved[id]; ok && prev.Identity == reg.Identity && !prev.UpdatedAt.IsZero() {
			reg.UpdatedAt = prev.UpdatedAt
			snap.Registrations[id] = reg
		}
		next.Registrations[id] = registrationTime{Identity: reg.Identity, UpdatedAt: reg.UpdatedAt}
	}
	writeRegistrationTimes(path, next)
}