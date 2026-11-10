package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/aura-bootstrap/fengshen_model_center/components"
	"github.com/aura-bootstrap/fengshen_model_center/integration"
)

func decodeRuntimeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	defer r.Body.Close()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		writeAPIError(w, failure("center_request_too_large", "请求超过64KiB或无法读取", 413))
		return false
	}
	raw = bytes.TrimSpace(raw)
	if !utf8.Valid(raw) || len(raw) == 0 || raw[0] != '{' || components.StrictJSON(raw, out) != nil || !exactBindingJSONFields(raw) {
		writeAPIError(w, failure("center_request_invalid", "请求字段、类型或JSON格式无效", 400))
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return true
		}
		if err != nil || token == nil {
			writeAPIError(w, failure("center_request_invalid", "请求不接受null字段", 400))
			return false
		}
	}
}

// encoding/json matches struct fields case-insensitively. Binding requests use
// exact wire names at each object level, including file and component objects.
func exactBindingJSONFields(raw []byte) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return false
	}
	exact := func(raw json.RawMessage, allowed map[string]bool) bool {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return false
		}
		for name := range fields {
			if !allowed[name] {
				return false
			}
		}
		return true
	}
	for name, value := range object {
		switch name {
		case "expected_revision", "inventory_id", "device":
		case "files":
			var files []json.RawMessage
			if json.Unmarshal(value, &files) != nil {
				return false
			}
			for _, file := range files {
				if !exact(file, map[string]bool{"model_id": true, "file_id": true}) {
					return false
				}
			}
		case "component":
			if !exact(value, map[string]bool{"id": true, "version": true, "manifest_sha256": true, "protocol": true}) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func serveBindingHTTP(manager *Manager, w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, manager.BindingSnapshot())
		return
	}
	scene := strings.TrimPrefix(r.URL.Path, integration.BindingsPath+"/")
	if r.Method == http.MethodDelete {
		var request struct {
			Revision *uint64 `json:"expected_revision"`
		}
		if !decodeRuntimeJSON(w, r, &request) {
			return
		}
		if request.Revision == nil {
			writeAPIError(w, failure("center_request_invalid", "缺少绑定版本", 422))
			return
		}
		value, err := manager.DeleteBinding(r.Context(), scene, *request.Revision)
		respond(w, http.StatusOK, value, err)
		return
	}
	var request struct {
		Revision    *uint64                           `json:"expected_revision"`
		InventoryID string                            `json:"inventory_id"`
		Files       []BindingFileSelection            `json:"files"`
		Component   integration.ExecutionComponentRef `json:"component"`
		Device      string                            `json:"device"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if request.Revision == nil {
		writeAPIError(w, failure("center_request_invalid", "缺少绑定版本", 422))
		return
	}
	value, err := manager.PutBinding(r.Context(), scene, BindingSelection{ExpectedRevision: *request.Revision, InventoryID: request.InventoryID, Files: request.Files, Component: request.Component, Device: request.Device})
	respond(w, http.StatusOK, value, err)
}
