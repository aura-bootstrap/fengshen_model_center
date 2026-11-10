package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validator(id, p string) (Registration, error) {
	if id != "whisper-asr" {
		return Registration{}, errors.New("unknown whisper-asr")
	}
	return Registration{InputPath: p, ResolvedPath: p, Kind: "file", Identity: "fixture", UpdatedAt: time.Now().UTC()}, nil
}
func TestTransactions(t *testing.T) {
	d := t.TempDir()
	s, e := Open(d, validator)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Open(d, validator); e == nil {
		t.Fatal("double lock")
	}
	a, e := s.Update(context.Background(), 0, "whisper-asr", "a")
	if e != nil || a.Revision != 1 {
		t.Fatal(a, e)
	}
	a.Registrations["evil"] = Registration{}
	if len(s.Snapshot().Registrations) != 1 {
		t.Fatal("alias")
	}
	if _, e = s.Delete(context.Background(), 0, "whisper-asr"); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	s.Close()
	s, e = Open(d, validator)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if s.Snapshot().Revision != 1 {
		t.Fatal(s.Snapshot())
	}
	if _, e = s.Delete(context.Background(), 1, "whisper-asr"); e != nil {
		t.Fatal(e)
	}
}
func TestCorruptReadOnly(t *testing.T) {
	for _, raw := range []string{`{`, `{"schema_version":2,"revision":0,"registrations":{}}`, `{"whisper-asr":"a","revision":1}`, `{"whisper-asr":"a","whisper-asr":"b"}`, strings.Repeat(" ", 256*1024+1)} {
		t.Run(raw[:1], func(t *testing.T) {
			d := t.TempDir()
			p := filepath.Join(d, "registrations.json")
			os.WriteFile(p, []byte(raw), 0600)
			s, e := Open(d, validator)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			if s.Snapshot().Error == "" {
				t.Fatal("not read only")
			}
			if _, e = s.Update(context.Background(), 0, "whisper-asr", "b"); e == nil {
				t.Fatal("corrupt overwritten")
			}
			b, _ := os.ReadFile(p)
			if string(b) != raw {
				t.Fatal("lost original")
			}
		})
	}
}
func TestLegacyPathsMigration(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failed-write"}[fail], func(t *testing.T) {
			d := t.TempDir()
			legacy := filepath.Join(d, "paths.json")
			raw := []byte(`{"whisper-asr":"old"}`)
			if e := os.WriteFile(legacy, raw, 0600); e != nil {
				t.Fatal(e)
			}
			s, e := Open(d, validator)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			if s.Snapshot().Registrations["whisper-asr"].InputPath != "old" {
				t.Fatal(s.Snapshot())
			}
			if _, e := os.Stat(s.path); !os.IsNotExist(e) {
				t.Fatal("Open wrote migration", e)
			}
			if fail {
				s.ops.write = func(*os.File, []byte) error { return errors.New("injected") }
			}
			_, e = s.Update(context.Background(), 0, "whisper-asr", "new")
			if fail != (e != nil) {
				t.Fatal(e)
			}
			b, e := os.ReadFile(legacy)
			if e != nil || string(b) != string(raw) {
				t.Fatal("legacy changed", string(b), e)
			}
			if fail {
				if _, e := os.Stat(s.path); !os.IsNotExist(e) {
					t.Fatal(e)
				}
				return
			}
			s.Close()
			s, e = Open(d, validator)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			if s.Snapshot().Revision != 1 || s.Snapshot().Registrations["whisper-asr"].InputPath != "new" {
				t.Fatal(s.Snapshot())
			}
		})
	}
}

func TestLegacySourceConflict(t *testing.T) {
	for _, scenario := range []string{"changed", "deleted", "destination-created", "both-exist", "corrupt-destination"} {
		t.Run(scenario, func(t *testing.T) {
			d := t.TempDir()
			legacy := filepath.Join(d, "paths.json")
			p := filepath.Join(d, "registrations.json")
			os.WriteFile(legacy, []byte(`{"whisper-asr":"old"}`), 0600)
			if scenario == "both-exist" {
				os.WriteFile(p, []byte(`{"schema_version":1,"revision":5,"registrations":{}}`), 0600)
			}
			if scenario == "corrupt-destination" {
				os.WriteFile(p, []byte(`{`), 0600)
			}
			s, e := Open(d, validator)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			if scenario == "both-exist" {
				if s.Snapshot().Revision != 5 || len(s.Snapshot().Registrations) != 0 {
					t.Fatal(s.Snapshot())
				}
				return
			}
			switch scenario {
			case "changed":
				os.WriteFile(legacy, []byte(`{}`), 0600)
			case "deleted":
				os.Remove(legacy)
			case "destination-created":
				os.WriteFile(p, []byte(`{}`), 0600)
			}
			if _, e := s.Update(context.Background(), 0, "whisper-asr", "new"); !errors.Is(e, ErrReadOnly) {
				t.Fatal(e)
			}
		})
	}
}

func TestUnavailableRegistrationsRepair(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, del := range []bool{false, true} {
			t.Run(fmt.Sprintf("legacy=%v/delete=%v", legacy, del), func(t *testing.T) {
				d := t.TempDir()
				if legacy {
					os.WriteFile(filepath.Join(d, "paths.json"), []byte(`{"whisper-asr":"missing","campplus":"missing-too"}`), 0600)
				} else {
					s, e := Open(d, validator)
					if e != nil {
						t.Fatal(e)
					}
					if _, e := s.Update(context.Background(), 0, "whisper-asr", "missing"); e != nil {
						t.Fatal(e)
					}
					s.Close()
				}
				v := func(id, p string) (Registration, error) {
					if strings.HasPrefix(p, "missing") {
						return Registration{}, os.ErrNotExist
					}
					return validator(id, p)
				}
				s, e := Open(d, v)
				if e != nil {
					t.Fatal(e)
				}
				defer s.Close()
				before := s.Snapshot()
				if !strings.Contains(before.Error, "registration_unavailable") || before.Registrations["whisper-asr"].InputPath != "missing" {
					t.Fatal(before)
				}
				var after Snapshot
				if del {
					after, e = s.Delete(context.Background(), before.Revision, "whisper-asr")
				} else {
					after, e = s.Update(context.Background(), before.Revision, "whisper-asr", "repaired")
				}
				if e != nil {
					t.Fatal(e)
				}
				if legacy {
					if after.Registrations["campplus"].InputPath != "missing-too" || after.Error == "" {
						t.Fatal(after)
					}
				} else if after.Error != "" {
					t.Fatal(after)
				}
				s.Close()
				s, e = Open(d, v)
				if e != nil {
					t.Fatal(e)
				}
				defer s.Close()
				if strings.HasPrefix(s.Snapshot().Error, "registry_corrupt") {
					t.Fatal(s.Snapshot())
				}
			})
		}
	}
}

func TestChangedRegistrationRepair(t *testing.T) {
	d := t.TempDir()
	s, e := Open(d, validator)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := s.Update(context.Background(), 0, "whisper-asr", "old"); e != nil {
		t.Fatal(e)
	}
	s.Close()
	v := func(id, p string) (Registration, error) {
		r, e := validator(id, p)
		r.Identity = "changed"
		return r, e
	}
	s, e = Open(d, v)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if !strings.Contains(s.Snapshot().Error, "registration_changed") {
		t.Fatal(s.Snapshot())
	}
	if after, e := s.Update(context.Background(), 1, "whisper-asr", "old"); e != nil || after.Error != "" {
		t.Fatal(after, e)
	}
}

func TestStableModelIDs(t *testing.T) {
	ids := []string{"whisper-asr", "whisper-vad", "local-text", "local-vision", "demucs", "demucs-ft", "seed-vc", "seed-whisper", "campplus", "bigvgan", "haar-frontal", "haar-profile"}
	v := func(id, p string) (Registration, error) {
		return Registration{InputPath: p, ResolvedPath: p, Kind: "file", Identity: id, UpdatedAt: time.Now().UTC()}, nil
	}
	s, e := Open(t.TempDir(), v)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	for i, id := range ids {
		if _, e := s.Update(context.Background(), uint64(i), id, id+".bin"); e != nil {
			t.Fatal(id, e)
		}
	}
	b, e := os.ReadFile(s.path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := decode(b, v); e != nil {
		t.Fatal(e)
	}
	b, e = json.Marshal(Snapshot{SchemaVersion: 1, Registrations: map[string]Registration{}, Error: "private diagnostic"})
	if e != nil || strings.Contains(string(b), "private diagnostic") || strings.Contains(string(b), "Error") {
		t.Fatal(string(b), e)
	}
}

func TestStrictLegacyAndUnknownIDs(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"whisper-asr":null}`, `{"whisper-asr":""}`, `{"whisper-asr":"a","whisper-asr":"b"}`, `{"unknown":"missing"}`, `{"schema_version":1,"revision":0,"registrations":{}}`, `{} {}`} {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "paths.json"), []byte(raw), 0600)
		s, e := Open(d, validator)
		if e != nil {
			t.Fatal(e)
		}
		if !strings.HasPrefix(s.Snapshot().Error, "registry_corrupt") {
			t.Errorf("accepted legacy %s", raw)
		}
		s.Close()
	}
	r, _ := validator("whisper-asr", "missing")
	b, _ := json.Marshal(Snapshot{SchemaVersion: 1, Registrations: map[string]Registration{"unknown": r}})
	if _, e := decode(b, func(string, string) (Registration, error) { return Registration{}, os.ErrNotExist }); e == nil {
		t.Fatal("unknown ID treated as unavailable")
	}
	s, e := Open(t.TempDir(), validator)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e := s.Update(context.Background(), 0, "unknown", "missing"); e == nil || !strings.Contains(e.Error(), "model_unknown") {
		t.Fatal(e)
	}
}

func TestMigrationAndExternalChange(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "registrations.json")
	os.WriteFile(p, []byte(`{"whisper-asr":"old"}`), 0600)
	s, e := Open(d, validator)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if s.Snapshot().Registrations["whisper-asr"].InputPath != "old" {
		t.Fatal(s.Snapshot())
	}
	if _, e = s.Update(context.Background(), 0, "whisper-asr", "new"); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(p + ".bak")
	if e != nil || string(b) != `{"whisper-asr":"old"}` {
		t.Fatal(string(b), e)
	}
	os.WriteFile(p, []byte(`{}`), 0600)
	if _, e = s.Update(context.Background(), 1, "whisper-asr", "x"); e == nil || s.Snapshot().Error == "" {
		t.Fatal("external modification not frozen")
	}
}
