package runtimeadapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"slices"

	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeassets"
	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimejobs"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/facehaar"
)

// face sends the bound image to the Haar shim over loopback HTTP. The center no
// longer executes the detector in-process.
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
		if (f.ModelID != "haar-frontal" && f.ModelID != "haar-profile") || paths[f.ModelID] != "" || !filepath.IsAbs(f.Path) {
			return empty, errTimbreBinding
		}
		paths[f.ModelID] = f.Path
	}
	if len(paths) != 2 || paths["haar-frontal"] == paths["haar-profile"] {
		return empty, errTimbreBinding
	}
	session, err := e.newShimSession(ctx, "haar", "", paths["haar-frontal"], paths["haar-profile"])
	if err != nil {
		return empty, err
	}
	defer session.Close()
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
	contentType := http.DetectContentType(data)
	if contentType != "image/jpeg" && contentType != "image/png" {
		return empty, runtimeassets.ErrInvalid
	}
	if emit != nil {
		if err = emit(runtimejobs.Progress{Stage: runtimejobs.StageRunning}); err != nil {
			return empty, err
		}
	}
	kind, err := session.client.Classify(ctx, contentType, data)
	if err != nil {
		if ctx.Err() != nil {
			return empty, ctx.Err()
		}
		return empty, runtimeassets.ErrInvalid
	}
	var result runtimejobs.FaceResult
	switch kind {
	case "frontal":
		result = runtimejobs.Frontal
	case "profile":
		result = runtimejobs.Profile
	case "no_face":
		result = runtimejobs.NoFace
	default:
		return empty, runtimeassets.ErrInvalid
	}
	if err = ctx.Err(); err != nil {
		return empty, err
	}
	return runtimejobs.Result{Component: req.Body.Component, Face: result}, nil
}
