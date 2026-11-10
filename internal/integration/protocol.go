// Package integration implements local model-center discovery.
// Descriptors are non-secret hints; process identity is checked only for
// liveness, never as a credential.
package integration

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	Protocol             = "fengshen.model-center.integration/v1"
	Product              = "fengshen-model-center"
	HealthPath           = "/api/integration/health"
	DependenciesPath     = "/api/integration/dependencies"
	DashboardPath        = "/api/integration/dashboard"
	ReportPath           = "/api/integration/report"
	FilesPath            = "/api/integration/model-files"
	MaxDashboardResponse = 4 * 1024 * 1024
	DescriptorName       = "instance.json"
	MaxDescriptor        = 8192
	MaxResponse          = 256 * 1024
)

var ErrUnsupported = errors.New("center_platform_unsupported: 当前平台不支持中心集成")

// Config is trusted engine configuration, never HTTP/user form input.
// InstallRoot contains model-center/fengshen-model-center.exe,
// model-center/delivery.json and resource-layout.json.
// DataDir must be a separate private integration directory, NOT registry data.
// Root is passed to the center as -root; RootKind is development or installed.
type Config struct {
	InstallRoot string
	Root        string
	RootKind    string
	DataDir     string
}

type Descriptor struct {
	Schema           int    `json:"schema"`
	InstanceID       string `json:"instance_id"`
	PID              uint32 `json:"pid"`
	ProcessStartedAt string `json:"process_started_at"`
	LoopbackAddress  string `json:"loopback_address"`
	Build            string `json:"build"`
	RootKind         string `json:"root_kind"`
	RootIdentity     string `json:"root_identity"`
	DataIdentity     string `json:"data_identity"`
}

type Health struct {
	Schema     int    `json:"schema"`
	Product    string `json:"product"`
	Protocol   string `json:"protocol"`
	Build      string `json:"build"`
	InstanceID string `json:"instance_id"`
	State      string `json:"state"`
}

type Status struct {
	Installed     bool      `json:"installed"`
	ProcessState  string    `json:"process_state"`
	IdentityState string    `json:"identity_state"`
	HealthState   string    `json:"health_state"`
	InstanceID    string    `json:"instance_id,omitempty"`
	Build         string    `json:"build,omitempty"`
	Protocol      string    `json:"protocol,omitempty"`
	URL           string    `json:"url,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
	Reason        string    `json:"reason,omitempty"`
}

// Evidence contains only stable codes, not paths or free-form diagnostics.
// Inference is always not_tested; these fields never imply inference readiness.
type Evidence struct {
	ID                string `json:"id"`
	Packaging         string `json:"packaging,omitempty"`
	Platform          string `json:"platform,omitempty"`
	Files             string `json:"files,omitempty"`
	Integrity         string `json:"integrity,omitempty"`
	StartupTest       string `json:"startup_test,omitempty"`
	ProtocolEvidence  string `json:"protocol_evidence,omitempty"`
	Discovery         string `json:"discovery,omitempty"`
	Composition       string `json:"composition,omitempty"`
	Dependency        string `json:"dependency,omitempty"`
	NonEmpty          bool   `json:"nonempty"`
	StartupBuildBound bool   `json:"startup_build_bound"`
	Optional          bool   `json:"optional"`
	Inference         string `json:"inference"`
}
type DependencySnapshot struct {
	Schema           int        `json:"schema"`
	InstanceID       string     `json:"instance_id"`
	Build            string     `json:"build"`
	ObservedAt       time.Time  `json:"observed_at"`
	InventoryID      string     `json:"inventory_id,omitempty"`
	FinishedAt       time.Time  `json:"finished_at"`
	RegistryRevision uint64     `json:"registry_revision"`
	Complete         bool       `json:"complete"`
	Stale            bool       `json:"stale"`
	Components       []Evidence `json:"components"`
	Models           []Evidence `json:"models"`
	Diagnostics      []string   `json:"diagnostics"`
}

type Dashboard struct {
	Schema                int             `json:"schema"`
	InstanceID            string          `json:"instance_id"`
	Build                 string          `json:"build"`
	State                 json.RawMessage `json:"state"`
	Inventory             json.RawMessage `json:"inventory"`
	LastCompleteInventory json.RawMessage `json:"last_complete_inventory"`
	Components            json.RawMessage `json:"components"`
}

type Report struct {
	Dashboard
	ObservedAt       time.Time       `json:"observed_at"`
	InventoryID      string          `json:"inventory_id"`
	RegistryRevision uint64          `json:"registry_revision"`
	Complete         bool            `json:"complete"`
	Stale            bool            `json:"stale"`
	Summary          ModelSummary    `json:"summary"`
	Models           json.RawMessage `json:"models"`
}

type ModelSummary struct {
	Total               int `json:"total"`
	Present             int `json:"present"`
	Missing             int `json:"missing"`
	Unknown             int `json:"unknown"`
	Recognized          int `json:"recognized"`
	CompositionComplete int `json:"composition_complete"`
}

type FilesPage struct {
	Schema     int             `json:"schema"`
	InstanceID string          `json:"instance_id"`
	Build      string          `json:"build"`
	Files      json.RawMessage `json:"files"`
	NextCursor string          `json:"next_cursor"`
}

func randomID() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}
func digestValid(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}
func decode(r io.Reader, limit int64, v any) error {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e != nil {
		return e
	}
	if int64(len(b)) > limit {
		return errors.New("protocol_too_large")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("protocol_trailing_data")
	}
	return nil
}
func canonical(address string) (string, error) {
	host, p, e := net.SplitHostPort(address)
	if e != nil {
		return "", errors.New("listen_not_loopback")
	}
	ip := net.ParseIP(host)
	port, e := strconv.Atoi(p)
	if e != nil || port < 1 || port > 65535 || ip == nil || !ip.IsLoopback() || strings.Contains(host, "%") {
		return "", errors.New("listen_not_loopback")
	}
	a := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	if a != address {
		return "", errors.New("listen_not_canonical")
	}
	return "http://" + a + "/", nil
}
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: timeout}).DialContext, DisableKeepAlives: true, ResponseHeaderTimeout: timeout, MaxResponseHeaderBytes: 8192}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("center_redirect_denied") }}
}
func safeCode(s string) bool {
	if len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == '/') {
			return false
		}
	}
	return true
}
func validateSnapshot(s *DependencySnapshot, now time.Time) error {
	if s.Schema != 1 || len(s.Components) > 64 || len(s.Models) > 64 || len(s.Diagnostics) > 256 {
		return errors.New("dependencies_invalid")
	}
	for _, d := range s.Diagnostics {
		if d == "" || !safeCode(d) {
			return errors.New("dependencies_diagnostic_invalid")
		}
	}
	if s.InventoryID != "" && !safeCode(s.InventoryID) {
		return errors.New("dependencies_inventory_invalid")
	}
	for groupIndex, group := range [][]Evidence{s.Components, s.Models} {
		seen := map[string]bool{}
		for _, e := range group {
			if seen[e.ID] {
				return errors.New("dependencies_duplicate_id")
			}
			seen[e.ID] = true
			if e.Dependency != "" && e.Dependency != "unknown" && e.Dependency != "not_checked" && e.Dependency != "not_applicable" && e.Dependency != "files_found" && e.Dependency != "incomplete" {
				return errors.New("dependencies_dependency_invalid")
			}
			if groupIndex == 0 && (e.Dependency != "" || e.NonEmpty) || groupIndex == 1 && e.StartupBuildBound {
				return errors.New("dependencies_evidence_kind_invalid")
			}
			if e.NonEmpty && (e.Discovery != "found" || e.Composition != "complete") {
				return errors.New("dependencies_nonempty_invalid")
			}
			if e.StartupBuildBound && (e.StartupTest != "passed" || e.Integrity != "matched" || e.Platform != "matched" || e.Files != "complete" || (e.Packaging != "go" && e.Packaging != "native" && e.Packaging != "pyinstaller-onedir")) {
				return errors.New("dependencies_startup_binding_invalid")
			}
			// The current build report carries declarations, not protocol execution.
			if e.ProtocolEvidence != "" && e.ProtocolEvidence != "not_declared" && e.ProtocolEvidence != "declared_not_tested" && e.ProtocolEvidence != "missing_required_declaration" {
				return errors.New("dependencies_protocol_invalid")
			}
			if e.ID == "" {
				return errors.New("dependencies_id_invalid")
			}
			for _, v := range []string{e.ID, e.Packaging, e.Platform, e.Files, e.Integrity, e.StartupTest, e.ProtocolEvidence, e.Discovery, e.Composition} {
				if !safeCode(v) {
					return errors.New("dependencies_evidence_invalid")
				}
			}
			if e.Inference != "not_tested" {
				return errors.New("dependencies_inference_denied")
			}
		}
	}
	if s.FinishedAt.IsZero() || now.Sub(s.FinishedAt) > 5*time.Minute || s.FinishedAt.After(now) {
		s.Stale = true
	}
	return nil
}
func identityError(e error) error {
	return fmt.Errorf("center_identity_mismatch: 中心身份确认失败: %w", e)
}
