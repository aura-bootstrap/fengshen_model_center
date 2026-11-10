package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/highlight/domain"
	"github.com/aura-bootstrap/fengshen_model_center/components"

	"github.com/aura-bootstrap/fengshen_model_center/safefs"

	"github.com/aura-bootstrap/fengshen_model_center/bindings"
	"github.com/aura-bootstrap/fengshen_model_center/integration"
)

type BindingFileSelection struct {
	ModelID string `json:"model_id"`
	FileID  string `json:"file_id"`
}

type BindingSelection struct {
	ExpectedRevision uint64                            `json:"expected_revision"`
	InventoryID      string                            `json:"inventory_id"`
	Files            []BindingFileSelection            `json:"files"`
	Component        integration.ExecutionComponentRef `json:"component"`
	Device           string                            `json:"device"`
}

type BindingView struct {
	Ref         integration.ExecutionBindingRef   `json:"ref"`
	Component   integration.ExecutionComponentRef `json:"component"`
	InventoryID string                            `json:"inventory_id"`
	Files       []BindingFileSelection            `json:"files"`
	Device      string                            `json:"device"`
}

type BindingSnapshot struct {
	SchemaVersion int                    `json:"schema_version"`
	Revision      uint64                 `json:"revision"`
	Bindings      map[string]BindingView `json:"bindings"`
	Error         string                 `json:"error,omitempty"`
}

func bindingView(snapshot bindings.Snapshot) BindingSnapshot {
	out := BindingSnapshot{SchemaVersion: snapshot.SchemaVersion, Revision: snapshot.Revision, Bindings: map[string]BindingView{}, Error: snapshot.Error}
	for scene, binding := range snapshot.Bindings {
		view := BindingView{Ref: binding.Ref, Component: binding.Component, InventoryID: binding.InventoryID, Device: binding.Device, Files: []BindingFileSelection{}}
		for _, file := range binding.Files {
			view.Files = append(view.Files, BindingFileSelection{ModelID: file.ModelID, FileID: file.FileID})
		}
		out.Bindings[scene] = view
	}
	return out
}

func (m *Manager) BindingSnapshot() BindingSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return bindingView(m.bindingStore.Snapshot())
}

func (m *Manager) PutBinding(ctx context.Context, scene string, request BindingSelection) (BindingSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return BindingSnapshot{}, os.ErrClosed
	}
	binding, err := m.resolveBindingLocked(ctx, scene, request)
	if err != nil {
		return bindingView(m.bindingStore.Snapshot()), err
	}
	snapshot, err := m.bindingStore.Put(ctx, request.ExpectedRevision, binding)
	return bindingView(snapshot), err
}

func (m *Manager) DeleteBinding(ctx context.Context, scene string, revision uint64) (BindingSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return BindingSnapshot{}, os.ErrClosed
	}
	snapshot, err := m.bindingStore.Delete(ctx, revision, scene)
	return bindingView(snapshot), err
}

func (m *Manager) resolveBindingLocked(ctx context.Context, scene string, request BindingSelection) (bindings.Binding, error) {
	invalid := func() (bindings.Binding, error) {
		return bindings.Binding{}, failure("center_binding_invalid", "请选择明确且完整的模型组成", 422)
	}
	definition, ok := integration.RuntimeScene(scene)
	if !ok || request.Component.Validate(scene) != nil || len(request.Files) == 0 || len(request.Files) > bindings.MaxFiles {
		return invalid()
	}
	inventory := m.inventories[request.InventoryID]
	registration := m.registry.Snapshot()
	if m.closed || inventory == nil || inventory.Stale || request.InventoryID != m.current || inventory.RegistryRevision != registration.Revision || registration.Error != "" {
		return bindings.Binding{}, failure("center_binding_stale", "盘点或登记已变化，请重新扫描并选择", 409)
	}
	allowed := map[string]bool{}
	for _, id := range definition.RequiredModels {
		allowed[id] = true
	}
	for _, id := range definition.OptionalModels {
		allowed[id] = true
	}
	for _, alternative := range definition.AlternativeModels {
		for _, id := range alternative {
			allowed[id] = true
		}
	}
	states := map[string]*ModelState{}
	for i := range inventory.Models {
		states[inventory.Models[i].ID] = &inventory.Models[i]
	}
	selected := map[string][]*ModelFile{}
	seen := map[string]bool{}
	binding := bindings.Binding{Scene: scene, InventoryID: inventory.ID, RegistryRevision: inventory.RegistryRevision, Component: request.Component, Device: request.Device}
	for _, selection := range request.Files {
		if err := ctx.Err(); err != nil {
			return bindings.Binding{}, err
		}
		if !allowed[selection.ModelID] || seen[selection.FileID] {
			return invalid()
		}
		seen[selection.FileID] = true
		state := states[selection.ModelID]
		if state == nil {
			return invalid()
		}
		var file *ModelFile
		for _, candidate := range state.files {
			if candidate.ID == selection.FileID {
				file = candidate
				break
			}
		}
		if file == nil || file.Evidence != "recognized_by_rule" || file.size <= 0 || file.Identity == "" {
			return invalid()
		}
		access := inventory.access[file.ScopeID]
		if access == nil {
			return invalid()
		}
		f, current, err := access.scope.OpenFile(file.relative)
		if f != nil {
			f.Close()
		}
		if err != nil || current.LinkKind != "none" || !sameFile(file, current) {
			return bindings.Binding{}, failure("center_binding_stale", "所选模型文件身份已变化", 409)
		}
		group := selected[selection.ModelID]
		if len(group) > 0 && group[0].Group != file.Group {
			return invalid()
		}
		selected[selection.ModelID] = append(group, file)
		scopePath := access.scope.Path()
		if sameScanPath(scopePath, file.Path) {
			scopePath = filepath.Dir(scopePath)
		}
		scope, err := safefs.Open(scopePath)
		if err != nil || !sameScanPath(scopePath, scope.Path()) {
			if scope != nil {
				scope.Close()
			}
			return bindings.Binding{}, failure("center_binding_stale", "模型目录身份无法确认", 409)
		}
		anchor, entry, err := scope.OpenDirectory(".")
		if anchor != nil {
			anchor.Close()
		}
		check, checked, fileErr := scope.OpenFile(file.relative)
		if check != nil {
			check.Close()
		}
		scope.Close()
		if err != nil || fileErr != nil || checked.LinkKind != "none" || !sameFile(file, checked) {
			return bindings.Binding{}, failure("center_binding_stale", "模型目录或文件身份已变化", 409)
		}
		frozen := bindings.FrozenFile{ModelID: selection.ModelID, FileID: file.ID, Group: file.Group, ScopePath: scopePath, ScopeIdentity: entry.Identity, RelativePath: file.relative, Identity: file.Identity, Bytes: file.Bytes, Modified: file.Modified}
		if registered, ok := registration.Registrations[selection.ModelID]; ok {
			current, err := validateRegistration(selection.ModelID, registered.InputPath)
			if err != nil || current.Identity != registered.Identity || current.ResolvedPath != registered.ResolvedPath || current.Kind != registered.Kind {
				return bindings.Binding{}, failure("center_binding_stale", "模型登记入口已变化", 409)
			}
			frozen.RegistrationInputPath, frozen.RegistrationPath = registered.InputPath, registered.ResolvedPath
			frozen.RegistrationKind, frozen.RegistrationIdentity, frozen.RegistrationUpdatedAt = registered.Kind, registered.Identity, registered.UpdatedAt
		}
		binding.Files = append(binding.Files, frozen)
	}
	if len(definition.AlternativeModels) > 0 {
		matches := 0
		for _, alternative := range definition.AlternativeModels {
			complete := len(selected) == len(alternative)
			for _, id := range alternative {
				complete = complete && len(selected[id]) > 0
			}
			if complete {
				matches++
			}
		}
		if matches != 1 {
			return invalid()
		}
	} else {
		for _, id := range definition.RequiredModels {
			if len(selected[id]) == 0 {
				return invalid()
			}
		}
	}
	for id, files := range selected {
		if !bindingCompositionComplete(states[id].Model, files) {
			return invalid()
		}
	}
	return binding, nil
}

func (m *Manager) runtimeCapabilities() (integration.RuntimeCapabilities, error) {
	observed := m.Components()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := integration.RuntimeCapabilities{ObservedAt: time.Now().UTC(), Capabilities: []integration.RuntimeCapability{}}
	if m.closed {
		return out, os.ErrClosed
	}
	snapshot := m.bindingStore.Snapshot()
	for _, scene := range integration.RuntimeScenes() {
		capability := integration.RuntimeCapability{Scene: scene.ID, State: integration.CapabilityMissingBinding, Reason: "center_binding_missing"}
		binding, exists := snapshot.Bindings[scene.ID]
		if exists {
			ref, component := binding.Ref, binding.Component
			capability.Binding, capability.Component = &ref, &component
			if scene.ID == "highlight.transcribe" {
				vadEnabled := false
				for _, file := range binding.Files {
					if file.ModelID == "whisper-vad" {
						vadEnabled = true
						break
					}
				}
				capability.VADEnabled = &vadEnabled
			}
			capability.State, capability.Reason = integration.CapabilityIncompatible, "center_binding_stale"
			if snapshot.Error == "" && m.bindingCurrentLocked(binding) {
				capability.State, capability.Reason = componentCapability(binding, observed)
				if capability.State == integration.CapabilityAvailable {
					if m.runtimeHandler == nil {
						capability.State, capability.Reason = integration.CapabilityIncompatible, "center_runtime_unavailable"
					} else {
						busy, evidence := m.runtimeHandler.CapabilityEvidence(ref, component)
						capability.Inference = evidence
						if busy {
							capability.State, capability.Reason = integration.CapabilityBusy, "center_runtime_busy"
						} else if evidence != nil && evidence.State == "failed" {
							capability.State = integration.CapabilityFailed
						}
					}
				}
			}
		}
		out.Capabilities = append(out.Capabilities, capability)
	}
	return out, nil
}

func componentCapability(binding bindings.Binding, observed []components.Evidence) (integration.CapabilityState, string) {
	for _, evidence := range observed {
		if evidence.ComponentID != binding.Component.ID || evidence.Version != binding.Component.Version {
			continue
		}
		if evidence.Manifest == nil || evidence.FilesState != "complete" {
			return integration.CapabilityMissingComponent, "center_component_missing"
		}
		if evidence.Platform != "matched" || evidence.ManifestSHA256 != binding.Component.ManifestSHA256 || evidence.Integrity != "matched" || evidence.StartupTest != "passed" {
			return integration.CapabilityIncompatible, "center_component_unverified"
		}
		if !contains(evidence.Manifest.Protocols, binding.Component.Protocol) {
			return integration.CapabilityIncompatible, "center_protocol_unsupported"
		}
		if !componentAdapterSupported(binding) {
			return integration.CapabilityIncompatible, "center_adapter_missing"
		}
		if binding.Scene == "highlight.transcribe" && !integration.WhisperVADSupported(binding.Component) {
			for _, file := range binding.Files {
				if file.ModelID == "whisper-vad" {
					return integration.CapabilityIncompatible, "center_whisper_vad_unsupported"
				}
			}
		}
		return integration.CapabilityAvailable, ""
	}
	return integration.CapabilityMissingComponent, "center_component_missing"
}

// This allowlist describes implemented wire contracts, not inference evidence.
func componentAdapterSupported(binding bindings.Binding) bool {
	c := binding.Component
	switch binding.Scene {
	case "highlight.transcribe":
		return c.ID == "whisper-batch" && c.Protocol == domain.CenterWhisperBatchProtocol || c.ID == "whisper-stream" && c.Protocol == domain.CenterWhisperStreamProtocol
	case "highlight.score", "highlight.vision":
		return c.ID == "llama-runtime" && c.Protocol == domain.CenterLlamaProtocol
	case "timbre.separate":
		return c.ID == "demucs-runner" && c.Protocol == integration.DemucsCLIProtocol
	case "timbre.convert":
		return c.ID == "seedvc-runner" && c.Protocol == integration.SeedVCCLIProtocol
	case "image.face":
		return c.ID == "haar-consumer" && c.Protocol == "haar/v1"
	default:
		return false
	}
}

func (m *Manager) bindingCurrentLocked(binding bindings.Binding) bool {
	registration := m.registry.Snapshot()
	if registration.Error != "" || registration.Revision != binding.RegistryRevision {
		return false
	}
	for _, file := range binding.Files {
		if registered, exists := registration.Registrations[file.ModelID]; exists {
			if registered.InputPath != file.RegistrationInputPath || registered.ResolvedPath != file.RegistrationPath || registered.Identity != file.RegistrationIdentity || registered.Kind != file.RegistrationKind || !registered.UpdatedAt.Equal(file.RegistrationUpdatedAt) {
				return false
			}
			current, err := validateRegistration(file.ModelID, registered.InputPath)
			if err != nil || current.Identity != registered.Identity || current.ResolvedPath != registered.ResolvedPath || current.Kind != registered.Kind {
				return false
			}
		} else if file.RegistrationIdentity != "" {
			return false
		}
		scope, err := safefs.Open(file.ScopePath)
		if err != nil {
			return false
		}
		anchor, directory, directoryErr := scope.OpenDirectory(".")
		if anchor != nil {
			anchor.Close()
		}
		f, entry, fileErr := scope.OpenFile(file.RelativePath)
		if f != nil {
			f.Close()
		}
		scope.Close()
		if directoryErr != nil || fileErr != nil || !sameScanPath(file.ScopePath, scope.Path()) || directory.Identity != file.ScopeIdentity || entry.LinkKind != "none" || entry.Identity != file.Identity || strconv.FormatInt(entry.Size, 10) != file.Bytes || !entry.Modified.Equal(file.Modified) {
			return false
		}
	}
	return true
}

func bindingCompositionComplete(model Model, files []*ModelFile) bool {
	alternatives := [][]string{}
	if len(model.Components) > 0 {
		alternatives = append(alternatives, model.Components)
	}
	alternatives = append(alternatives, model.ComponentAlternatives...)
	if repo, parts := demucsHFComponents(model); repo != "" {
		alternatives = append(alternatives, parts)
	}
	for _, parts := range alternatives {
		if len(parts) != len(files) {
			continue
		}
		used := map[int]bool{}
		complete := true
		for _, pattern := range parts {
			matched := false
			for i, file := range files {
				if match, _ := filepath.Match(pattern, filepath.Base(file.Path)); match && !used[i] {
					used[i], matched = true, true
					break
				}
			}
			complete = complete && matched
		}
		if complete {
			return true
		}
	}
	return false
}
