package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/modelmanifest"
)

type client struct {
	base string
	http *http.Client
}

type stateResponse struct {
	Catalog []struct {
		ID string `json:"id"`
	} `json:"catalog"`
	Registry struct {
		Revision      uint64                     `json:"revision"`
		Registrations map[string]json.RawMessage `json:"registrations"`
	} `json:"registry"`
}

type diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type operationResponse struct {
	ID             string       `json:"id"`
	Kind           string       `json:"kind"`
	Status         string       `json:"status"`
	CheckedEntries int          `json:"checked_entries"`
	ProcessedBytes string       `json:"processed_bytes"`
	ResultID       string       `json:"result_id"`
	Hashes         []hashRecord `json:"hashes"`
	Diagnostics    []diagnostic `json:"diagnostics"`
}

type hashRecord struct {
	FileID string `json:"file_id"`
	Status string `json:"status"`
	SHA256 string `json:"sha256"`
	Bytes  string `json:"bytes"`
}

type sizeMetric struct {
	Bytes *string `json:"bytes"`
}

type modelState struct {
	ID                string     `json:"id"`
	DiscoveryStatus   string     `json:"discovery_status"`
	IdentityStatus    string     `json:"identity_status"`
	CompositionStatus string     `json:"composition_status"`
	FileCount         int        `json:"file_count"`
	Size              sizeMetric `json:"size"`
}

type inventoryResponse struct {
	ID          string       `json:"id"`
	Complete    bool         `json:"complete"`
	Stale       bool         `json:"stale"`
	Models      []modelState `json:"models"`
	Diagnostics []diagnostic `json:"diagnostics"`
}

type fileRecord struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Bytes    string `json:"bytes"`
	Identity string `json:"identity"`
	LinkKind string `json:"link_kind"`
}

type filePage struct {
	Files      []fileRecord `json:"files"`
	NextCursor string       `json:"next_cursor"`
}

type reportResponse struct {
	Summary struct {
		Total               int `json:"total"`
		Present             int `json:"present"`
		Missing             int `json:"missing"`
		Unknown             int `json:"unknown"`
		Recognized          int `json:"recognized"`
		CompositionComplete int `json:"composition_complete"`
	} `json:"summary"`
}

type scanRequest struct {
	Revision uint64 `json:"registry_revision"`
}

type verifyRequest struct {
	Inventory string   `json:"inventory_id"`
	Model     string   `json:"model_id"`
	Files     []string `json:"file_ids"`
}

type expectedModel struct {
	count int
	bytes int64
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "[error] %v\n", err)
		os.Exit(1)
	}
}

func defaultStateDir() string {
	if env := strings.TrimSpace(os.Getenv("FENGSHEN_STATE_DIR")); env != "" {
		return env
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(filepath.Dir(exe))
	}
	return "."
}

func defaultManifestPath() string {
	if strings.TrimSpace(os.Getenv("FENGSHEN_STATE_DIR")) == "" {
		if home, err := os.UserHomeDir(); err == nil {
			for _, ch := range []string{"user", "dev", "admin"} {
				p := filepath.Join(home, ".fengshen", "slicer", ch, "model_manifest.json")
				if _, err := os.Stat(p); err == nil {
					return p
				}
			}
		}
	}
	return modelmanifest.DefaultPath(defaultStateDir())
}

func run() error {
	base := flag.String("base", "http://127.0.0.1:18120", "center base URL")
	manifestPath := flag.String("manifest", "", "model manifest path (default: first existing channel config root, else <state-dir>/model_manifest.json)")
	verifyHashes := flag.Bool("verify-hashes", false, "read and SHA-256 verify every model file")
	hashModel := flag.String("hash-model", "", "only verify hashes for this model")
	timeout := flag.Duration("timeout", 30*time.Minute, "operation timeout")
	pageSize := flag.Int("page-size", 2, "file page size")
	flag.Parse()
	if *pageSize < 1 || *pageSize > 200 {
		return fmt.Errorf("page-size must be between 1 and 200")
	}
	if *hashModel != "" && !*verifyHashes {
		return fmt.Errorf("-hash-model requires -verify-hashes")
	}
	if *manifestPath == "" {
		*manifestPath = defaultManifestPath()
	}
	c := &client{base: strings.TrimRight(*base, "/"), http: &http.Client{Timeout: 60 * time.Second}}

	step("manifest")
	manifest, _, err := modelmanifest.Load(*manifestPath)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	if len(manifest.Entries) == 0 {
		return fmt.Errorf("manifest has no entries: %s", *manifestPath)
	}
	expected := map[string]*expectedModel{}
	var totalBytes int64
	for _, entry := range manifest.Entries {
		if entry.ModelID == "" {
			return fmt.Errorf("manifest entry without model_id")
		}
		path := entry.FilePath()
		if path == "" {
			return fmt.Errorf("model %s: manifest entry without path", entry.ModelID)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("model %s: %w", entry.ModelID, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("model %s: not a regular file: %s", entry.ModelID, path)
		}
		if entry.Bytes > 0 && info.Size() != entry.Bytes {
			return fmt.Errorf("model %s: size mismatch for %s: manifest=%d disk=%d", entry.ModelID, path, entry.Bytes, info.Size())
		}
		value := expected[entry.ModelID]
		if value == nil {
			value = &expectedModel{}
			expected[entry.ModelID] = value
		}
		value.count++
		value.bytes += info.Size()
		totalBytes += info.Size()
	}
	fmt.Printf("entries=%d models=%d bytes=%d\n", len(manifest.Entries), len(expected), totalBytes)

	step("API state")
	raw, _, err := c.call("GET", "/api/state", nil, 200)
	if err != nil {
		return err
	}
	var state stateResponse
	if err = json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("decode state: %w", err)
	}
	catalog := map[string]bool{}
	for _, model := range state.Catalog {
		if model.ID == "" {
			return fmt.Errorf("state catalog contains a model without id")
		}
		catalog[model.ID] = true
	}
	if len(catalog) != len(expected) {
		return fmt.Errorf("catalog models=%d, manifest models=%d", len(catalog), len(expected))
	}
	for id := range expected {
		if !catalog[id] {
			return fmt.Errorf("manifest model missing from catalog: %s", id)
		}
	}
	for id := range catalog {
		if expected[id] == nil {
			return fmt.Errorf("catalog model has no manifest entries: %s", id)
		}
	}
	if len(state.Registry.Registrations) != len(catalog) {
		return fmt.Errorf("registry registrations=%d, catalog models=%d", len(state.Registry.Registrations), len(catalog))
	}
	pass("catalog=%d registrations=%d revision=%d", len(catalog), len(state.Registry.Registrations), state.Registry.Revision)

	step("API components and bindings")
	raw, _, err = c.call("GET", "/api/components", nil, 200)
	if err != nil {
		return err
	}
	var components []struct {
		ComponentID string `json:"component_id"`
	}
	if err = json.Unmarshal(raw, &components); err != nil {
		return fmt.Errorf("decode components: %w", err)
	}
	if len(components) == 0 {
		return fmt.Errorf("components payload is empty")
	}
	raw, _, err = c.call("GET", "/api/bindings", nil, 200)
	if err != nil {
		return err
	}
	var bindings struct {
		Bindings json.RawMessage `json:"bindings"`
	}
	if err = json.Unmarshal(raw, &bindings); err != nil {
		return fmt.Errorf("decode bindings: %w", err)
	}
	if len(bindings.Bindings) == 0 {
		return fmt.Errorf("bindings payload missing bindings")
	}
	pass("components=%d bindings=present", len(components))

	step("API guards")
	guards := []struct {
		method string
		path   string
		body   any
		want   int
	}{
		{"GET", "/api/unknown", nil, 404},
		{"GET", "/api/inventories/missing", nil, 404},
		{"GET", "/api/operations/missing", nil, 410},
		{"GET", "/", nil, 404},
		{"PUT", "/api/state", map[string]any{}, 405},
		{"GET", "/api/report?path=x", nil, 400},
		{"POST", "/api/operations/scan", map[string]any{}, 422},
	}
	for _, guard := range guards {
		if _, _, err = c.call(guard.method, guard.path, guard.body, guard.want); err != nil {
			return err
		}
	}
	_, integrationStatus, err := c.call("GET", "/api/integration/health", nil, 200, 503)
	if err != nil {
		return err
	}
	_, runtimeStatus, err := c.call("GET", "/api/runtime/jobs", nil, 200, 401, 403, 503)
	if err != nil {
		return err
	}
	pass("guards=7 integration_http=%d runtime_http=%d", integrationStatus, runtimeStatus)

	step("model scan")
	started, _, err := c.call("POST", "/api/operations/scan", scanRequest{Revision: state.Registry.Revision}, 202)
	if err != nil {
		return err
	}
	var start operationResponse
	if err = json.Unmarshal(started, &start); err != nil {
		return fmt.Errorf("decode scan operation: %w", err)
	}
	if start.ID == "" {
		return fmt.Errorf("scan operation id is empty")
	}
	scan, err := c.waitOperation(start.ID, *timeout)
	if err != nil {
		return err
	}
	if scan.Status != "completed" {
		return fmt.Errorf("scan status=%s diagnostics=%d", scan.Status, len(scan.Diagnostics))
	}
	if scan.ResultID == "" {
		return fmt.Errorf("scan result id is empty")
	}
	if scan.CheckedEntries < len(manifest.Entries) {
		return fmt.Errorf("scan checked entries=%d, manifest entries=%d", scan.CheckedEntries, len(manifest.Entries))
	}
	pass("scan completed checked_entries=%d processed_bytes=%s", scan.CheckedEntries, scan.ProcessedBytes)

	step("inventory")
	raw, _, err = c.call("GET", "/api/inventories/"+url.PathEscape(scan.ResultID), nil, 200)
	if err != nil {
		return err
	}
	var inventory inventoryResponse
	if err = json.Unmarshal(raw, &inventory); err != nil {
		return fmt.Errorf("decode inventory: %w", err)
	}
	if !inventory.Complete {
		return fmt.Errorf("inventory incomplete diagnostics=%d", len(inventory.Diagnostics))
	}
	if inventory.Stale {
		return fmt.Errorf("inventory is stale")
	}
	if len(inventory.Models) != len(catalog) {
		return fmt.Errorf("inventory models=%d, catalog models=%d", len(inventory.Models), len(catalog))
	}
	models := map[string]modelState{}
	for _, model := range inventory.Models {
		models[model.ID] = model
	}
	ids := make(map[string][]string, len(models))
	ordered := make([]string, 0, len(expected))
	for id := range expected {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	totalFiles := 0
	for _, id := range ordered {
		want := expected[id]
		model, found := models[id]
		if !found {
			return fmt.Errorf("inventory missing model: %s", id)
		}
		if model.FileCount != want.count {
			return fmt.Errorf("model %s file_count=%d, manifest entries=%d", id, model.FileCount, want.count)
		}
		if model.DiscoveryStatus != "found" {
			return fmt.Errorf("model %s discovery_status=%s", id, model.DiscoveryStatus)
		}
		if model.IdentityStatus != "recognized_by_rule" {
			return fmt.Errorf("model %s identity_status=%s", id, model.IdentityStatus)
		}
		if model.CompositionStatus != "complete" {
			return fmt.Errorf("model %s composition_status=%s", id, model.CompositionStatus)
		}
		if model.Size.Bytes == nil || *model.Size.Bytes != strconv.FormatInt(want.bytes, 10) {
			value := "<nil>"
			if model.Size.Bytes != nil {
				value = *model.Size.Bytes
			}
			return fmt.Errorf("model %s size=%s, want %d", id, value, want.bytes)
		}
		fileIDs, fileBytes, err := c.fetchFiles(inventory.ID, id, *pageSize)
		if err != nil {
			return err
		}
		if len(fileIDs) != want.count {
			return fmt.Errorf("model %s files=%d, want %d", id, len(fileIDs), want.count)
		}
		if fileBytes != want.bytes {
			return fmt.Errorf("model %s file bytes=%d, want %d", id, fileBytes, want.bytes)
		}
		ids[id] = fileIDs
		totalFiles += len(fileIDs)
		fmt.Printf("model %-14s files=%d bytes=%d\n", id, len(fileIDs), fileBytes)
	}
	first := ordered[0]
	if _, _, err = c.call("GET", "/api/inventories/"+url.PathEscape(inventory.ID)+"/models/missing/files", nil, 404); err != nil {
		return err
	}
	if _, _, err = c.call("GET", "/api/inventories/"+url.PathEscape(inventory.ID)+"/models/"+url.PathEscape(first)+"/files?limit=x", nil, 422); err != nil {
		return err
	}
	pass("inventory complete=12 models=%d files=%d", len(inventory.Models), totalFiles)

	step("report after scan")
	raw, _, err = c.call("GET", "/api/report", nil, 200)
	if err != nil {
		return err
	}
	var report reportResponse
	if err = json.Unmarshal(raw, &report); err != nil {
		return fmt.Errorf("decode report: %w", err)
	}
	summary := report.Summary
	if summary.Total != len(catalog) || summary.Present != summary.Total || summary.Missing != 0 || summary.Unknown != 0 || summary.Recognized != summary.Total || summary.CompositionComplete != summary.Total {
		return fmt.Errorf("report summary total=%d present=%d missing=%d unknown=%d recognized=%d composition_complete=%d",
			summary.Total, summary.Present, summary.Missing, summary.Unknown, summary.Recognized, summary.CompositionComplete)
	}
	pass("report total=%d present=%d composition_complete=%d", summary.Total, summary.Present, summary.CompositionComplete)

	if *verifyHashes {
		step("SHA-256 verification")
		verifyModels := ordered
		if *hashModel != "" {
			if expected[*hashModel] == nil {
				return fmt.Errorf("unknown hash-model: %s", *hashModel)
			}
			verifyModels = []string{*hashModel}
		}
		verified := 0
		for _, id := range verifyModels {
			files := ids[id]
			for offset := 0; offset < len(files); offset += 100 {
				end := offset + 100
				if end > len(files) {
					end = len(files)
				}
				chunk := files[offset:end]
				raw, _, err = c.call("POST", "/api/operations/verify", verifyRequest{Inventory: inventory.ID, Model: id, Files: chunk}, 202)
				if err != nil {
					return err
				}
				var started operationResponse
				if err = json.Unmarshal(raw, &started); err != nil {
					return fmt.Errorf("decode verify operation: %w", err)
				}
				done, err := c.waitOperation(started.ID, *timeout)
				if err != nil {
					return err
				}
				if done.Status != "completed" {
					return fmt.Errorf("verify %s status=%s diagnostics=%d", id, done.Status, len(done.Diagnostics))
				}
				if len(done.Hashes) != len(chunk) {
					return fmt.Errorf("verify %s hashes=%d, want %d", id, len(done.Hashes), len(chunk))
				}
				for _, hash := range done.Hashes {
					if hash.Status != "fingerprint_computed" || len(hash.SHA256) != 64 {
						return fmt.Errorf("verify %s file %s status=%s sha_len=%d", id, hash.FileID, hash.Status, len(hash.SHA256))
					}
				}
				verified += len(chunk)
			}
			fmt.Printf("hashes %-14s files=%d\n", id, len(files))
		}
		pass("hash verification files=%d", verified)
	} else {
		fmt.Printf("\nhashes skipped (use -verify-hashes to read and verify %d files, %d bytes)\n", totalFiles, totalBytes)
	}

	mode := "availability"
	if *verifyHashes {
		mode = "availability + SHA-256"
	}
	fmt.Printf("\nPASS: %s verified: %d models / %d files / %d bytes; components=%d\n", mode, len(catalog), totalFiles, totalBytes, len(components))
	return nil
}

func (c *client) waitOperation(id string, timeout time.Duration) (operationResponse, error) {
	deadline := time.Now().Add(timeout)
	for {
		raw, _, err := c.call("GET", "/api/operations/"+url.PathEscape(id), nil, 200)
		if err != nil {
			return operationResponse{}, err
		}
		var operation operationResponse
		if err = json.Unmarshal(raw, &operation); err != nil {
			return operationResponse{}, fmt.Errorf("decode operation %s: %w", id, err)
		}
		switch operation.Status {
		case "running", "cancelling":
			if time.Now().After(deadline) {
				return operation, fmt.Errorf("operation %s timeout after %s: status=%s", id, timeout, operation.Status)
			}
			time.Sleep(200 * time.Millisecond)
		default:
			return operation, nil
		}
	}
}

func (c *client) fetchFiles(inventoryID, modelID string, pageSize int) ([]string, int64, error) {
	var ids []string
	var total int64
	cursor := ""
	seenCursors := map[string]bool{}
	for page := 0; ; page++ {
		if page > 1000 {
			return nil, 0, fmt.Errorf("model %s file pagination exceeded 1000 pages", modelID)
		}
		path := "/api/inventories/" + url.PathEscape(inventoryID) + "/models/" + url.PathEscape(modelID) + "/files?limit=" + strconv.Itoa(pageSize)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		raw, _, err := c.call("GET", path, nil, 200)
		if err != nil {
			return nil, 0, err
		}
		var result filePage
		if err = json.Unmarshal(raw, &result); err != nil {
			return nil, 0, fmt.Errorf("decode model %s files: %w", modelID, err)
		}
		for _, file := range result.Files {
			if file.ID == "" || file.Path == "" || file.Identity == "" {
				return nil, 0, fmt.Errorf("model %s file record incomplete: id=%q path=%q identity=%q", modelID, file.ID, file.Path, file.Identity)
			}
			size, err := strconv.ParseInt(file.Bytes, 10, 64)
			if err != nil {
				return nil, 0, fmt.Errorf("model %s file %s bytes=%q", modelID, file.ID, file.Bytes)
			}
			ids = append(ids, file.ID)
			total += size
		}
		if result.NextCursor == "" {
			break
		}
		if seenCursors[result.NextCursor] {
			return nil, 0, fmt.Errorf("model %s file pagination cursor repeated", modelID)
		}
		seenCursors[result.NextCursor] = true
		cursor = result.NextCursor
	}
	return ids, total, nil
}

func (c *client) call(method, path string, body any, want ...int) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("read %s %s: %w", method, path, err)
	}
	for _, status := range want {
		if response.StatusCode == status {
			return raw, response.StatusCode, nil
		}
	}
	snippet := strings.TrimSpace(string(raw))
	if len(snippet) > 300 {
		snippet = snippet[:300]
	}
	return raw, response.StatusCode, fmt.Errorf("%s %s -> HTTP %d, want %v: %s", method, path, response.StatusCode, want, snippet)
}

func step(name string) {
	fmt.Printf("\n=== %s ===\n", name)
}

func pass(format string, args ...any) {
	fmt.Printf("PASS: "+format+"\n", args...)
}
