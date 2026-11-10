package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/registry"
)

const rulesVersion = "2026-11-08/v3"

type Diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	ModelID string `json:"model_id,omitempty"`
	ScopeID string `json:"scope_id,omitempty"`
}
type SizeMetric struct {
	Bytes           *string `json:"bytes"`
	Complete        bool    `json:"complete"`
	CountedFiles    int     `json:"counted_files"`
	IdentityQuality string  `json:"identity_quality"`
}
type ModelFile struct {
	ID                 string      `json:"id"`
	ScopeID            string      `json:"scope_id"`
	Path               string      `json:"path"`
	Aliases            []string    `json:"aliases"`
	Bytes              string      `json:"bytes"`
	Modified           time.Time   `json:"modified"`
	Identity           string      `json:"identity"`
	LinkKind           string      `json:"link_kind"`
	Evidence           string      `json:"evidence"`
	Group              string      `json:"group"`
	DuplicateCandidate bool        `json:"duplicate_candidate"`
	Hash               *HashRecord `json:"hash,omitempty"`
	// ContentEqual is an inventory-local group ID, not a deletion or space-saving claim.
	ContentEqual string `json:"content_equal,omitempty"`
	relative     string
	size         int64
}
type ModelState struct {
	Model
	DiscoveryStatus    string            `json:"discovery_status"`
	IdentityStatus     string            `json:"identity_status"`
	CompositionStatus  string            `json:"composition_status"`
	DependencyStatus   string            `json:"dependency_status"`
	VerificationStatus string            `json:"verification_status"`
	Inference          string            `json:"inference"`
	Size               SizeMetric        `json:"size"`
	FileCount          int               `json:"file_count"`
	Groups             map[string]string `json:"groups"`
	files              []*ModelFile
}
type ScopeResult struct {
	ID       string   `json:"id"`
	Path     string   `json:"path"`
	Kind     string   `json:"kind"`
	Status   string   `json:"status"`
	Complete bool     `json:"complete"`
	ModelIDs []string `json:"model_ids"`
}
type RuntimeEnvironment struct {
	ScopeID           string  `json:"scope_id"`
	Kind              string  `json:"kind"`
	InterpreterPath   string  `json:"interpreter_path"`
	InterpreterStatus string  `json:"interpreter_status"`
	InterpreterBytes  *string `json:"interpreter_bytes"`
	DeliveryStatus    string  `json:"delivery_status"`
	Inference         string  `json:"inference"`
	Message           string  `json:"message"`
}

type Inventory struct {
	ID                  string               `json:"id"`
	RulesVersion        string               `json:"rules_version"`
	RegistryRevision    uint64               `json:"registry_revision"`
	StartedAt           time.Time            `json:"started_at"`
	FinishedAt          time.Time            `json:"finished_at"`
	Complete            bool                 `json:"complete"`
	Stale               bool                 `json:"stale"`
	Models              []ModelState         `json:"models"`
	Scopes              []ScopeResult        `json:"scopes"`
	WeightSize          SizeMetric           `json:"weight_size"`
	AssetSize           SizeMetric           `json:"asset_size"`
	RuntimeSize         SizeMetric           `json:"runtime_size"`
	RuntimeEnvironments []RuntimeEnvironment `json:"runtime_environments"`
	SeedSuiteSize       SizeMetric           `json:"seed_suite_size"`
	Diagnostics         []Diagnostic         `json:"diagnostics"`
	ProjectRoot         string               `json:"project_root"`
	access              map[string]*accessScope
}
type HashRecord struct {
	FileID    string    `json:"file_id"`
	Status    string    `json:"status"`
	SHA256    string    `json:"sha256,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	Bytes     string    `json:"bytes"`
	Message   string    `json:"message"`
}
type Operation struct {
	ID             string               `json:"id"`
	Kind           string               `json:"kind"`
	Status         string               `json:"status"`
	StartedAt      time.Time            `json:"started_at"`
	FinishedAt     *time.Time           `json:"finished_at,omitempty"`
	CheckedEntries int                  `json:"checked_entries"`
	ProcessedBytes string               `json:"processed_bytes"`
	ResultID       string               `json:"result_id,omitempty"`
	Hashes         []HashRecord         `json:"hashes"`
	Evidence       *components.Evidence `json:"evidence,omitempty"`
	Diagnostics    []Diagnostic         `json:"diagnostics"`
}
type State struct {
	StorageError          string            `json:"storage_error,omitempty"`
	Catalog               []Model           `json:"catalog"`
	Registry              registry.Snapshot `json:"registry"`
	CurrentInventory      string            `json:"current_inventory"`
	LastCompleteInventory string            `json:"last_complete_inventory"`
	LastAttempt           *Operation        `json:"last_attempt"`
	ActiveOperation       *Operation        `json:"active_operation"`
	ProjectRoot           string            `json:"project_root"`
	Capabilities          map[string]any    `json:"capabilities"`
}
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

func (e *apiError) Error() string                    { return e.Message }
func failure(code, message string, status int) error { return &apiError{code, message, status} }
func opaqueID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Errorf("生成随机标识失败: %w", err))
	}
	return hex.EncodeToString(b)
}
func metric(bytes int64, count int, complete, known bool) SizeMetric {
	m := SizeMetric{Complete: complete, CountedFiles: count, IdentityQuality: "file_identity"}
	if known {
		s := strconv.FormatInt(bytes, 10)
		m.Bytes = &s
	}
	return m
}
func capabilities() map[string]any {
	return map[string]any{"custom_url_model_configurable": false, "center_inference_available": false, "inventory_available": true, "reason": "中心本期仅提供盘点、路径登记与摘要，不提供模型推理。"}
}
