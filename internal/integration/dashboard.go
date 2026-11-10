package integration

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"time"
)

// ParseFilesQuery is shared by the host and admin API. No filesystem paths or
// caller-selected page sizes are accepted by this read-only capability.
func ParseFilesQuery(u *url.URL) (inventoryID, modelID, cursor string, err error) {
	invalid := errors.New("model_files_query_invalid")
	if u.ForceQuery {
		return "", "", "", invalid
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil {
		return "", "", "", invalid
	}
	for k, v := range q {
		if (k != "inventory_id" && k != "model_id" && k != "cursor") || len(v) != 1 {
			return "", "", "", invalid
		}
	}
	inventoryID, modelID, cursor = q.Get("inventory_id"), q.Get("model_id"), q.Get("cursor")
	b, e := hex.DecodeString(inventoryID)
	if e != nil || len(b) != 16 || len(inventoryID) != 32 || !identifierCode(inventoryID, false) || !identifierCode(modelID, false) || !identifierCode(cursor, true) {
		return "", "", "", invalid
	}
	return inventoryID, modelID, cursor, nil
}
func identifierCode(s string, empty bool) bool {
	if len(s) > 128 || s == "" && !empty {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func jsonKind(b json.RawMessage, kind byte, nullable bool) bool {
	b = bytes.TrimSpace(b)
	return json.Valid(b) && (len(b) > 0 && b[0] == kind || nullable && bytes.Equal(b, []byte("null")))
}
func validateDashboard(v Dashboard) error {
	if v.Schema != 1 || !jsonKind(v.State, '{', false) || bytes.Equal(bytes.TrimSpace(v.State), []byte("{}")) || !jsonKind(v.Inventory, '{', true) || !jsonKind(v.LastCompleteInventory, '{', true) || !jsonKind(v.Components, '[', false) {
		return errors.New("dashboard_invalid")
	}
	var state map[string]json.RawMessage
	if json.Unmarshal(v.State, &state) != nil || len(state) == 0 {
		return errors.New("dashboard_invalid")
	}
	return nil
}
func validateReport(v Report) error {
	invalid := errors.New("report_invalid")
	if validateDashboard(v.Dashboard) != nil || v.ObservedAt.IsZero() || !jsonKind(v.Models, '[', false) {
		return invalid
	}
	var models []json.RawMessage
	if json.Unmarshal(v.Models, &models) != nil || len(models) > 64 {
		return invalid
	}
	seen := make(map[string]bool, len(models))
	var present, missing, unknown int
	for _, raw := range models {
		if !jsonKind(raw, '{', false) {
			return invalid
		}
		var model map[string]json.RawMessage
		if json.Unmarshal(raw, &model) != nil {
			return invalid
		}
		var id string
		if json.Unmarshal(model["id"], &id) != nil || !identifierCode(id, false) || seen[id] {
			return invalid
		}
		seen[id] = true
		switch string(bytes.TrimSpace(model["exists"])) {
		case "true":
			present++
		case "false":
			missing++
		case "null":
			unknown++
		default:
			return invalid
		}
	}
	s := v.Summary
	if s.Total != len(models) || s.Present != present || s.Missing != missing || s.Unknown != unknown || s.Recognized < 0 || s.Recognized > s.Total || s.CompositionComplete < 0 || s.CompositionComplete > s.Total {
		return invalid
	}
	return nil
}

func validateFilesPage(v FilesPage) error {
	if v.Schema != 1 || !jsonKind(v.Files, '[', false) || !identifierCode(v.NextCursor, true) {
		return errors.New("model_files_invalid")
	}
	var files []json.RawMessage
	if json.Unmarshal(v.Files, &files) != nil || len(files) > 200 {
		return errors.New("model_files_invalid")
	}
	for _, f := range files {
		if !jsonKind(f, '{', false) {
			return errors.New("model_files_invalid")
		}
	}
	return nil
}

// Dashboard never launches the center or performs initial build verification.
func (c *Client) Dashboard(ctx context.Context) (Dashboard, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	var v Dashboard
	h, e := c.connect(ctx)
	if e != nil {
		return v, e
	}
	if e = fetchLimited(ctx, h, DashboardPath, &v, 3*time.Second, MaxDashboardResponse); e == nil {
		if v.InstanceID != h.InstanceID || v.Build != h.Build {
			e = identityError(errors.New("dashboard_binding"))
		} else {
			e = validateDashboard(v)
		}
	}
	if e != nil {
		return Dashboard{}, e
	}
	return v, nil
}

// Report reads existing evidence without launching the center or verifying a new build.
func (c *Client) Report(ctx context.Context) (Report, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	var v Report
	h, e := c.connect(ctx)
	if e != nil {
		return v, e
	}
	if e = fetchLimited(ctx, h, ReportPath, &v, 3*time.Second, MaxDashboardResponse); e == nil {
		if v.InstanceID != h.InstanceID || v.Build != h.Build {
			e = identityError(errors.New("report_binding"))
		} else {
			e = validateReport(v)
		}
	}
	if e != nil {
		return Report{}, e
	}
	return v, nil
}

func (c *Client) ModelFiles(ctx context.Context, inventoryID, modelID, cursor string) (FilesPage, error) {
	q := url.Values{"inventory_id": {inventoryID}, "model_id": {modelID}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	u := &url.URL{Path: FilesPath, RawQuery: q.Encode()}
	if _, _, _, e := ParseFilesQuery(u); e != nil {
		return FilesPage{}, e
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	var v FilesPage
	h, e := c.connect(ctx)
	if e != nil {
		return v, e
	}
	if e = fetchLimited(ctx, h, u.RequestURI(), &v, 3*time.Second, MaxDashboardResponse); e == nil {
		if v.InstanceID != h.InstanceID || v.Build != h.Build {
			e = identityError(errors.New("model_files_binding"))
		} else {
			e = validateFilesPage(v)
		}
	}
	if e != nil {
		return FilesPage{}, e
	}
	return v, nil
}
