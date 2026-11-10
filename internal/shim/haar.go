package shim

import (
	"errors"
	"io"
	"net/http"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/facehaar"
)

func (s *Server) classify(w http.ResponseWriter, r *http.Request) {
	if !s.requireMode(w, ModeHaar) {
		return
	}
	contentType := r.Header.Get("Content-Type")
	if contentType != "image/jpeg" && contentType != "image/png" {
		writeError(w, http.StatusUnsupportedMediaType, componentprotocol.Fail("component_request"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, facehaar.MaxImageBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, componentprotocol.Fail("component_too_large"))
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusUnprocessableEntity, componentprotocol.Fail("component_request"))
		return
	}
	result, err := s.detector.Detect(r.Context(), body)
	if err != nil {
		if errors.Is(err, facehaar.ErrImageTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, componentprotocol.Fail("component_too_large"))
			return
		}
		writeError(w, http.StatusUnprocessableEntity, componentprotocol.Fail("component_request"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"kind": string(result.Kind), "algorithm": "haar/v1"})
}
