package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/aura-bootstrap/fengshen_model_center/bindings"
	"github.com/aura-bootstrap/fengshen_model_center/components"
	"github.com/aura-bootstrap/fengshen_model_center/integration"
	"github.com/aura-bootstrap/fengshen_model_center/runtimeadapter"
	"github.com/aura-bootstrap/fengshen_model_center/runtimeprocess"
	"github.com/aura-bootstrap/fengshen_model_center/safefs"
)

type runtimePins struct {
	files       []*os.File
	directories map[string]bool
	once        sync.Once
}

func (p *runtimePins) release() {
	p.once.Do(func() {
		for i := len(p.files) - 1; i >= 0; i-- {
			p.files[i].Close()
		}
	})
}

func (p *runtimePins) directory(path string) error {
	if !filepath.IsAbs(path) {
		return safefs.ErrBoundary
	}
	path = filepath.Clean(path)
	key := strings.ToLower(path)
	if p.directories[key] {
		return nil
	}
	if parent := filepath.Dir(path); parent != path {
		if err := p.directory(parent); err != nil {
			return err
		}
	}
	f, err := runtimeprocess.PinDirectory(path)
	if err != nil {
		return err
	}
	p.files = append(p.files, f)
	if p.directories == nil {
		p.directories = map[string]bool{}
	}
	p.directories[key] = true
	return nil
}

func (m *Manager) runtimeBinding(ref integration.ExecutionBindingRef, component integration.ExecutionComponentRef) (bindings.Binding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot := m.bindingStore.Snapshot()
	binding, ok := snapshot.Bindings[ref.Scene]
	if m.closed || snapshot.Error != "" || !ok || binding.Ref != ref || binding.Component != component || !m.bindingCurrentLocked(binding) {
		return bindings.Binding{}, errors.New("center_binding_stale")
	}
	return binding, nil
}

func (m *Manager) validateRuntimeBinding(ctx context.Context, ref integration.ExecutionBindingRef, component integration.ExecutionComponentRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := m.runtimeBinding(ref, component); err != nil {
		return err
	}
	for _, evidence := range m.Components() {
		if evidence.ComponentID == component.ID && evidence.Version == component.Version && evidence.Manifest != nil && evidence.ManifestSHA256 == component.ManifestSHA256 && evidence.Platform == "matched" && evidence.FilesState == "complete" && evidence.StartupTest == "passed" && contains(evidence.Manifest.Protocols, component.Protocol) {
			return nil
		}
	}
	return errors.New("center_component_missing")
}

func (m *Manager) resolveRuntimeBinding(ctx context.Context, ref integration.ExecutionBindingRef, component integration.ExecutionComponentRef) (out runtimeadapter.Resolved, err error) {
	binding, err := m.runtimeBinding(ref, component)
	if err != nil {
		return out, err
	}
	evidence, err := m.components.Verify(ctx, component.ID, component.Version)
	if err != nil || evidence.Manifest == nil || evidence.ManifestSHA256 != component.ManifestSHA256 || evidence.Platform != "matched" || evidence.Integrity != "matched" || evidence.StartupTest != "passed" || !contains(evidence.Manifest.Protocols, component.Protocol) {
		return out, errors.New("center_component_unverified")
	}
	pins := &runtimePins{}
	out.Release = pins.release
	defer func() {
		if err != nil {
			out.Release()
		}
	}()
	out.ComponentRoot = components.ComponentDirectory(m.componentRoot, component.ID, component.Version)
	if err = pins.directory(out.ComponentRoot); err != nil {
		return out, errors.New("center_component_unverified")
	}
	for _, file := range binding.Files {
		if err = ctx.Err(); err != nil {
			return out, err
		}
		path := filepath.Join(file.ScopePath, file.RelativePath)
		if err = pins.directory(filepath.Dir(path)); err != nil {
			return out, errors.New("center_binding_stale")
		}
		scope, e := safefs.Open(file.ScopePath)
		if e != nil {
			return out, errors.New("center_binding_stale")
		}
		f, directory, e := scope.OpenDirectory(".")
		if f != nil {
			f.Close()
		}
		if e != nil || directory.Identity != file.ScopeIdentity {
			scope.Close()
			return out, errors.New("center_binding_stale")
		}
		pinned, e := runtimeprocess.PinFile(path)
		scope.Close()
		if e != nil {
			return out, errors.New("center_binding_stale")
		}
		pins.files = append(pins.files, pinned)
		st, statErr := pinned.Stat()
		identity, idErr := safefs.Identity(pinned)
		if statErr != nil || idErr != nil || identity != file.Identity || strconv.FormatInt(st.Size(), 10) != file.Bytes || !st.ModTime().Equal(file.Modified) {
			return out, errors.New("center_binding_stale")
		}
		out.Models = append(out.Models, runtimeadapter.ModelFile{ModelID: file.ModelID, Path: path})
	}
	for _, file := range evidence.Manifest.Files {
		path := filepath.Join(out.ComponentRoot, filepath.FromSlash(file.Path))
		if err = pins.directory(filepath.Dir(path)); err != nil {
			return out, errors.New("center_component_unverified")
		}
		pinned, e := runtimeprocess.PinFile(path)
		if e != nil {
			return out, errors.New("center_component_unverified")
		}
		pins.files = append(pins.files, pinned)
	}
	if _, err = m.runtimeBinding(ref, component); err != nil {
		return out, err
	}
	evidence, err = m.components.Verify(ctx, component.ID, component.Version)
	if err != nil || evidence.ManifestSHA256 != component.ManifestSHA256 {
		return out, errors.New("center_component_unverified")
	}
	out.Binding, out.Manifest = binding, *evidence.Manifest
	for _, file := range out.Manifest.Files {
		if file.Path == out.Manifest.Entry {
			out.Entry, err = runtimeprocess.VerifyBinary(filepath.Join(out.ComponentRoot, filepath.FromSlash(file.Path)), file.SHA256, component.ID+"/"+component.Version, ref.SHA256)
			if err != nil {
				return out, err
			}
			if component.ID == "haar-consumer" {
				// Haar runs in this process. A valid component copy alone cannot
				// authenticate a different center's embedded implementation.
				executable, e := os.Executable()
				if e != nil {
					return out, errors.New("center_component_unverified")
				}
				if e = pins.directory(filepath.Dir(executable)); e != nil {
					return out, errors.New("center_component_unverified")
				}
				pinned, e := runtimeprocess.PinFile(executable)
				if e != nil {
					return out, errors.New("center_component_unverified")
				}
				pins.files = append(pins.files, pinned)
				if _, e = runtimeprocess.VerifyBinary(executable, file.SHA256, component.ID+"/"+component.Version, ref.SHA256); e != nil {
					return out, errors.New("center_component_unverified")
				}
			}
			return out, nil
		}
	}
	return out, errors.New("center_component_missing")
}
