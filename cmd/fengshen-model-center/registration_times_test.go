package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/bindings"
	"github.com/aura-bootstrap/fengshen_model_center/internal/registry"
)

func TestRegistrationTimesSurviveRestart(t *testing.T) {
	root := t.TempDir()
	store := t.TempDir()
	put(t, filepath.Join(root, "models", "ggml-small.bin"), "weights")
	manifest := testManifest(t, root)
	first, err := NewManager(root, store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	before := first.registrySnap
	if len(before.Registrations) == 0 {
		t.Fatal("夹具未产生登记")
	}
	first.Close()
	second, err := NewManager(root, store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for id, reg := range second.registrySnap.Registrations {
		prev, ok := before.Registrations[id]
		if !ok {
			t.Fatalf("重启后丢失登记 %s", id)
		}
		if !reg.UpdatedAt.Equal(prev.UpdatedAt) {
			t.Fatalf("登记 %s 时间戳跨重启漂移: %s -> %s", id, prev.UpdatedAt, reg.UpdatedAt)
		}
	}
}

func TestRegistrationTimesAdvanceOnIdentityChange(t *testing.T) {
	root := t.TempDir()
	store := t.TempDir()
	put(t, filepath.Join(root, "models", "ggml-small.bin"), "weights")
	manifest := testManifest(t, root)
	first, err := NewManager(root, store, manifest)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	var before time.Time
	for modelID, reg := range first.registrySnap.Registrations {
		id, before = modelID, reg.UpdatedAt
	}
	first.Close()
	// A different directory yields a different NTFS file identity for the
	// same file name; deleting and recreating in place can reuse the ID.
	put(t, filepath.Join(root, "other", "ggml-small.bin"), "weights")
	second, err := NewManager(root, store, testManifest(t, filepath.Join(root, "other")))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	reg, ok := second.registrySnap.Registrations[id]
	if !ok {
		t.Fatalf("登记 %s 消失", id)
	}
	if reg.UpdatedAt.Equal(before) {
		t.Fatal("登记身份变化后时间戳未更新")
	}
}

func TestStabilizeRegistrationTimesSeedsFromFrozenBindings(t *testing.T) {
	store := t.TempDir()
	frozen := time.Date(2026, 11, 12, 4, 35, 27, 210942400, time.UTC)
	bindingSnapshot := bindings.Snapshot{Bindings: map[string]bindings.Binding{
		"highlight.transcribe": {Files: []bindings.FrozenFile{{
			ModelID: "whisper-asr", RegistrationIdentity: "identity-a", RegistrationUpdatedAt: frozen,
		}}},
	}}
	snap := registry.Snapshot{Registrations: map[string]registry.Registration{
		"whisper-asr": {Identity: "identity-a", UpdatedAt: time.Now().UTC()},
	}}
	stabilizeRegistrationTimes(store, snap, bindingSnapshot)
	if got := snap.Registrations["whisper-asr"].UpdatedAt; !got.Equal(frozen) {
		t.Fatalf("未采用绑定冻结时间戳: %s", got)
	}
	restarted := registry.Snapshot{Registrations: map[string]registry.Registration{
		"whisper-asr": {Identity: "identity-a", UpdatedAt: time.Now().UTC()},
	}}
	stabilizeRegistrationTimes(store, restarted, bindings.Snapshot{})
	if got := restarted.Registrations["whisper-asr"].UpdatedAt; !got.Equal(frozen) {
		t.Fatalf("重启后未保留种子时间戳: %s", got)
	}
}

func TestStabilizeRegistrationTimesKeepsFreshStampOnIdentityMismatch(t *testing.T) {
	store := t.TempDir()
	old := time.Date(2026, 11, 12, 4, 35, 27, 0, time.UTC)
	bindingSnapshot := bindings.Snapshot{Bindings: map[string]bindings.Binding{
		"highlight.transcribe": {Files: []bindings.FrozenFile{{
			ModelID: "whisper-asr", RegistrationIdentity: "identity-old", RegistrationUpdatedAt: old,
		}}},
	}}
	now := time.Now().UTC()
	snap := registry.Snapshot{Registrations: map[string]registry.Registration{
		"whisper-asr": {Identity: "identity-new", UpdatedAt: now},
	}}
	stabilizeRegistrationTimes(store, snap, bindingSnapshot)
	if got := snap.Registrations["whisper-asr"].UpdatedAt; !got.Equal(now) {
		t.Fatalf("身份不匹配时不应采用旧时间戳: %s", got)
	}
}