package runtimeadapter

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"

	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/facehaar"
	"github.com/aura-bootstrap/fengshen_model_center/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/runtimejobs"
	"github.com/aura-bootstrap/fengshen_model_center/safefs"
)

func faceBoundXML(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		return nil, errTimbreBinding
	}
	parent, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	leaf, err := parent.Derive(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer leaf.Close()
	f, entry, err := leaf.OpenFile(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const maxXML = 8 << 20
	if entry.Size <= 0 || entry.Size > maxXML {
		return nil, errTimbreBinding
	}
	data, err := io.ReadAll(io.LimitReader(timbreContextReader{ctx, f}, maxXML+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != entry.Size {
		return nil, errTimbreBinding
	}
	return data, ctx.Err()
}

func (e *Executor) face(ctx context.Context, req runtimejobs.Request, r Resolved, emit runtimejobs.Emit) (runtimejobs.Result, error) {
	empty := runtimejobs.Result{}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if req.Binding.Scene != "image.face" || req.Body.Face == nil {
		return empty, errors.New("center_request_invalid")
	}
	if req.Body.Component.ID != "haar-consumer" || r.Manifest.ComponentID != "haar-consumer" || r.Manifest.Packaging != "go" || r.Manifest.Version != req.Body.Component.Version || req.Body.Component.Protocol != "haar/v1" || !slices.Contains(r.Manifest.Protocols, "haar/v1") {
		return empty, errors.New("center_component_missing")
	}
	paths := map[string]string{}
	for _, f := range r.Models {
		if (f.ModelID != "haar-frontal" && f.ModelID != "haar-profile") || paths[f.ModelID] != "" {
			return empty, errTimbreBinding
		}
		paths[f.ModelID] = f.Path
	}
	if len(paths) != 2 || paths["haar-frontal"] == paths["haar-profile"] {
		return empty, errTimbreBinding
	}
	frontal, err := faceBoundXML(ctx, paths["haar-frontal"])
	if err != nil {
		return empty, err
	}
	profile, err := faceBoundXML(ctx, paths["haar-profile"])
	if err != nil {
		return empty, err
	}
	detector, err := facehaar.New(frontal, profile)
	if err != nil {
		return empty, errTimbreBinding
	}
	src, meta, err := e.assets.Open(ctx, req.TaskID, req.Body.Face.AssetID)
	if err != nil {
		return empty, err
	}
	defer src.Close()
	if meta.Kind != runtimeassets.Image {
		return empty, runtimeassets.ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(timbreContextReader{ctx, src}, facehaar.MaxImageBytes+1))
	if err != nil {
		return empty, err
	}
	if len(data) > facehaar.MaxImageBytes {
		return empty, runtimeassets.ErrInvalid
	}
	if emit != nil {
		if err = emit(runtimejobs.Progress{Stage: runtimejobs.StageRunning}); err != nil {
			return empty, err
		}
	}
	detected, err := detector.Detect(ctx, data)
	if err != nil {
		if ctx.Err() != nil {
			return empty, ctx.Err()
		}
		return empty, runtimeassets.ErrInvalid
	}
	var kind runtimejobs.FaceResult
	switch detected.Kind {
	case facehaar.Frontal:
		kind = runtimejobs.Frontal
	case facehaar.Profile:
		kind = runtimejobs.Profile
	case facehaar.NoFace:
		kind = runtimejobs.NoFace
	default:
		return empty, runtimeassets.ErrInvalid
	}
	if err = ctx.Err(); err != nil {
		return empty, err
	}
	return runtimejobs.Result{Component: req.Body.Component, Face: kind}, nil
}
