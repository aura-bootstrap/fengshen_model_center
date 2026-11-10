package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const BindingBodyLimit = 64 << 10
const bindingSnapshotLimit = 256 << 10

// These DTOs mirror the management wire, never the center's persisted paths.
type BindingFileSelection struct {
	ModelID string `json:"model_id"`
	FileID  string `json:"file_id"`
}
type BindingSelection struct {
	ExpectedRevision uint64                 `json:"expected_revision"`
	InventoryID      string                 `json:"inventory_id"`
	Files            []BindingFileSelection `json:"files"`
	Component        ExecutionComponentRef  `json:"component"`
	Device           string                 `json:"device"`
}
type BindingView struct {
	Ref         ExecutionBindingRef    `json:"ref"`
	Component   ExecutionComponentRef  `json:"component"`
	InventoryID string                 `json:"inventory_id"`
	Files       []BindingFileSelection `json:"files"`
	Device      string                 `json:"device"`
}
type BindingSnapshot struct {
	SchemaVersion int                    `json:"schema_version"`
	Revision      uint64                 `json:"revision"`
	Bindings      map[string]BindingView `json:"bindings"`
	Error         string                 `json:"error,omitempty"`
}

var errBindingInvalid = errors.New("center_binding_invalid")

func bindingDeviceValid(s string) bool {
	switch s {
	case "cpu", "auto", "cuda", "metal", "vulkan":
		return true
	}
	for _, prefix := range []string{"cuda:", "vulkan:"} {
		if strings.HasPrefix(s, prefix) {
			n := strings.TrimPrefix(s, prefix)
			value, err := strconv.ParseUint(n, 10, 8)
			return err == nil && strconv.FormatUint(value, 10) == n
		}
	}
	return false
}
func (v BindingSelection) Validate(scene string) error {
	def, ok := RuntimeScene(scene)
	if !ok || !opaqueRuntimeID(v.InventoryID) || v.Component.Validate(scene) != nil || !bindingDeviceValid(v.Device) || len(v.Files) == 0 || len(v.Files) > 256 {
		return errBindingInvalid
	}
	allowed, selected, seen := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, id := range def.RequiredModels {
		allowed[id] = true
	}
	for _, id := range def.OptionalModels {
		allowed[id] = true
	}
	for _, group := range def.AlternativeModels {
		for _, id := range group {
			allowed[id] = true
		}
	}
	for _, file := range v.Files {
		if !allowed[file.ModelID] || !opaqueRuntimeID(file.FileID) || seen[file.FileID] {
			return errBindingInvalid
		}
		seen[file.FileID], selected[file.ModelID] = true, true
	}
	if len(def.AlternativeModels) > 0 {
		matches := 0
		for _, group := range def.AlternativeModels {
			complete := len(selected) == len(group)
			for _, id := range group {
				complete = complete && selected[id]
			}
			if complete {
				matches++
			}
		}
		if matches != 1 {
			return errBindingInvalid
		}
	} else {
		for _, id := range def.RequiredModels {
			if !selected[id] {
				return errBindingInvalid
			}
		}
	}
	// File-group completeness and live identity remain the center's responsibility.
	return nil
}

// DecodeBindingSelection enforces exact wire names, duplicate/null rejection,
// required revision presence and uint64 integer semantics before any handshake.
func DecodeBindingSelection(raw []byte, scene string) (BindingSelection, error) {
	var v BindingSelection
	if len(raw) > BindingBodyLimit || runtimeDecode(raw, &v) != nil || strictSessionJSON(raw, &v) != nil {
		return v, errBindingInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields["expected_revision"] == nil || v.Validate(scene) != nil {
		return BindingSelection{}, errBindingInvalid
	}
	return v, nil
}
func DecodeBindingDelete(raw []byte) (uint64, error) {
	var v struct {
		Revision *uint64 `json:"expected_revision"`
	}
	if len(raw) > BindingBodyLimit || runtimeDecode(raw, &v) != nil || strictSessionJSON(raw, &v) != nil || v.Revision == nil {
		return 0, errBindingInvalid
	}
	return *v.Revision, nil
}
func validateBindingSnapshot(v *BindingSnapshot) error {
	if v.SchemaVersion != 1 || v.Bindings == nil || len(v.Bindings) > len(RuntimeScenes()) {
		return errRuntimeInvalid
	}
	if v.Error != "" {
		// The store's diagnostic includes text. Project only known stable codes.
		code := strings.SplitN(v.Error, ":", 2)[0]
		switch code {
		case "bindings_corrupt", "bindings_read_only", "bindings_external_change", "bindings_commit_uncertain", "bindings_write_failed":
			v.Error = code
		default:
			return errRuntimeInvalid
		}
	}
	for scene, view := range v.Bindings {
		selection := BindingSelection{InventoryID: view.InventoryID, Files: view.Files, Component: view.Component, Device: view.Device}
		if view.Ref.Scene != scene || view.Ref.Validate() != nil || view.Ref.Revision > v.Revision || selection.Validate(scene) != nil {
			return errRuntimeInvalid
		}
	}
	return nil
}

// BindingHTTPError retains status and a fixed diagnostic, not upstream messages.
type BindingHTTPError struct {
	Status int
	Code   string
}

func (e *BindingHTTPError) Error() string { return e.Code }
func bindingErrorCode(code string) string {
	switch code {
	case "center_binding_stale", "center_binding_invalid", "center_binding_store_unavailable", "center_request_invalid", "center_request_too_large", "center_session_expired", "center_session_denied", "center_identity_unconfirmed":
		return code
	default:
		return "center_binding_request_failed"
	}
}

type BindingsClient struct{ client *Client }

func NewBindingsClient(c *Client) *BindingsClient {
	return &BindingsClient{client: c}
}
func (c *BindingsClient) Bindings(ctx context.Context) (BindingSnapshot, error) {
	return c.request(ctx, http.MethodGet, BindingsPath, nil)
}
func (c *BindingsClient) PutBinding(ctx context.Context, scene string, v BindingSelection) (BindingSnapshot, error) {
	if v.Validate(scene) != nil {
		return BindingSnapshot{}, errBindingInvalid
	}
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > BindingBodyLimit {
		return BindingSnapshot{}, errBindingInvalid
	}
	return c.request(ctx, http.MethodPut, BindingsPath+"/"+scene, raw)
}
func (c *BindingsClient) DeleteBinding(ctx context.Context, scene string, revision uint64) (BindingSnapshot, error) {
	if _, ok := RuntimeScene(scene); !ok {
		return BindingSnapshot{}, errBindingInvalid
	}
	raw, _ := json.Marshal(struct {
		Revision uint64 `json:"expected_revision"`
	}{revision})
	return c.request(ctx, http.MethodDelete, BindingsPath+"/"+scene, raw)
}
func (c *BindingsClient) request(ctx context.Context, method, path string, raw []byte) (BindingSnapshot, error) {
	var out BindingSnapshot
	if c == nil || c.client == nil {
		return out, errors.New("center_session_invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	address, err := c.client.address(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if errors.Is(err, ErrUnsupported) {
			return out, ErrUnsupported
		}
		for _, code := range []string{"center_unbuilt", "center_missing", "center_port_conflict", "center_identity_mismatch", "center_identity_unconfirmed"} {
			if err.Error() == code || strings.HasPrefix(err.Error(), code+":") {
				return out, errors.New(code)
			}
		}
		return out, errors.New("center_identity_unconfirmed")
	}
	base, err := canonical(address)
	if err != nil {
		return out, errors.New("center_session_invalid")
	}
	var body io.Reader
	if raw != nil {
		body = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(base, "/")+path, body)
	if err != nil {
		return out, errors.New("center_request_invalid")
	}
	r.GetBody = nil // Never replay a write with an unknown outcome.
	if raw != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	h := SessionHTTPClient(15 * time.Second)
	h.Transport.(*http.Transport).DisableCompression = true
	defer h.CloseIdleConnections()
	response, err := h.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, errors.New("center_transport_failed")
	}
	if response == nil || response.Body == nil {
		return out, errRuntimeInvalid
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Encoding") != "" {
		return out, errRuntimeInvalid
	}
	data, err := runtimeRead(response, bindingSnapshotLimit)
	if err != nil {
		return out, errRuntimeInvalid
	}
	if response.StatusCode != http.StatusOK {
		var wire struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		code := "center_binding_request_failed"
		if runtimeDecode(data, &wire) == nil {
			code = bindingErrorCode(wire.Error.Code)
		}
		return out, &BindingHTTPError{Status: response.StatusCode, Code: code}
	}
	typ, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || typ != "application/json" || runtimeDecode(data, &out) != nil || strictSessionJSON(data, &out) != nil || validateBindingSnapshot(&out) != nil {
		return BindingSnapshot{}, errRuntimeInvalid
	}
	return out, nil
}
func (c *Client) Bindings(ctx context.Context) (BindingSnapshot, error) {
	return NewBindingsClient(c).Bindings(ctx)
}
func (c *Client) PutBinding(ctx context.Context, scene string, v BindingSelection) (BindingSnapshot, error) {
	return NewBindingsClient(c).PutBinding(ctx, scene, v)
}
func (c *Client) DeleteBinding(ctx context.Context, scene string, revision uint64) (BindingSnapshot, error) {
	return NewBindingsClient(c).DeleteBinding(ctx, scene, revision)
}
