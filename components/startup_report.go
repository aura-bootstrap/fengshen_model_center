package components

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/safefs"
)

const MaxStartupReportBytes = 256 * 1024

func (m *Manager) readStartupReport(s *safefs.Scope, prefix string, manifest Manifest, e *Evidence, entries *[]safefs.Entry) {
	key := manifest.ComponentID + "/" + manifest.Version
	previous, had := m.reports[key]
	stale := func() {
		e.StartupTest = "stale"
		e.CleanEnvironment = "not_verified"
		e.StartupReport = nil
	}
	if manifest.StartupReportRef == "" {
		if had {
			stale()
		}
		return
	}
	f, a, err := open(s, prefix+manifest.StartupReportRef)
	if err != nil {
		if had {
			stale()
		} else if !os.IsNotExist(err) {
			e.StartupTest = "invalid_report"
		}
		e.Diagnostics = append(e.Diagnostics, "component_startup_report_unavailable: "+err.Error())
		return
	}
	*entries = append(*entries, a)
	report, parseErr := ParseStartupReport(f, manifest, e.ManifestSHA256)
	st, statErr := f.Stat()
	identity, idErr := safefs.Identity(f)
	closeErr := f.Close()
	g, after, reopenErr := open(s, prefix+manifest.StartupReportRef)
	if g != nil {
		g.Close()
	}
	if statErr != nil || idErr != nil || closeErr != nil || reopenErr != nil || !same(a, after) || st.Size() != a.Size || !st.ModTime().Equal(a.Modified) || identity != a.Identity {
		stale()
		e.Diagnostics = append(e.Diagnostics, "component_startup_report_changed")
		return
	}
	if had {
		changed := previous.manifest != e.ManifestSHA256 || len(previous.entries) != len(*entries)
		if !changed {
			for i := range *entries {
				if !same(previous.entries[i], (*entries)[i]) {
					changed = true
					break
				}
			}
		}
		if changed {
			stale()
			e.Diagnostics = append(e.Diagnostics, "component_startup_report_stale: 构建或文件观测已改变，旧启动证据失效")
			return
		}
	}
	if parseErr != nil {
		e.StartupTest = "invalid_report"
		e.Diagnostics = append(e.Diagnostics, "component_startup_report_invalid: "+parseErr.Error())
		return
	}
	e.StartupTest = report.StartupTest
	e.StartupReport = &report
	// Even a validated build record is not independently authenticated here.
	// Keep the effective clean environment state unverified; expose the original
	// claim only inside StartupReport, with an explicit provenance diagnostic.
	if report.CleanEnvironment == "validated" {
		e.Diagnostics = append(e.Diagnostics, "构建报告记录了干净环境测试；中心未独立验证其真实性")
	}
	m.reports[key] = cached{manifest: e.ManifestSHA256, entries: append([]safefs.Entry(nil), (*entries)...)}
}

// StartupReport is the public build-script contract (schema 1). The SHA-256 is
// over the exact manifest.json bytes, not re-encoded JSON. Write this report only
// after the manifest; exclude both files from Manifest.Files. A final delivery
// manifest must bind both. Reading a report never executes or authenticates it.
// ProtocolEvidence may only repeat declarations, never claim runtime success.
type StartupReport struct {
	Schema                   int                       `json:"schema"`
	ComponentID              string                    `json:"component_id"`
	Version                  string                    `json:"version"`
	ManifestSHA256           string                    `json:"manifest_sha256"`
	StartupTest              string                    `json:"startup_test"`
	MetadataOnly             *MetadataOnlyEvidence     `json:"metadata_only,omitempty"`
	CleanEnvironment         string                    `json:"clean_environment"`
	CleanEnvironmentEvidence *CleanEnvironmentEvidence `json:"clean_environment_evidence,omitempty"`
	ProtocolEvidence         string                    `json:"protocol_evidence"`
	Inference                string                    `json:"inference"`
}

// MetadataOnlyEvidence records an explicit build-time metadata-only run.
// ExitCode is required even when zero. No model, network or compilation is allowed.
type MetadataOnlyEvidence struct {
	Arguments     []string  `json:"arguments"`
	ExitCode      *int      `json:"exit_code"`
	CheckedAt     time.Time `json:"checked_at"`
	NoModelLoad   bool      `json:"no_model_load"`
	NoNetwork     bool      `json:"no_network"`
	NoCompilation bool      `json:"no_compilation"`
}

// CleanEnvironmentEvidence describes a build operator's isolated VM/container
// test, not verification by the center. PATH-only changes are insufficient.
// EnvironmentRef and EvidenceRef identify retained external test records; they
// are opaque descriptions, never paths/URLs to open by this package.
type CleanEnvironmentEvidence struct {
	Kind                string `json:"kind"`
	EnvironmentRef      string `json:"environment_ref"`
	EvidenceRef         string `json:"evidence_ref"`
	NoHostPython        bool   `json:"no_host_python"`
	NoHostCompiler      bool   `json:"no_host_compiler"`
	NoDevelopmentMounts bool   `json:"no_development_mounts"`
}

// ParseStartupReport strictly validates a report against the exact build identity.
// It validates recorded claims only, and does not establish clean-environment trust.
func ParseStartupReport(r io.Reader, m Manifest, manifestSHA256 string) (StartupReport, error) {
	var p StartupReport
	b, err := io.ReadAll(io.LimitReader(r, MaxStartupReportBytes+1))
	if err != nil {
		return p, err
	}
	if len(b) > MaxStartupReportBytes {
		return p, fmt.Errorf("startup report exceeds 256 KiB")
	}
	if err = StrictJSON(b, &p); err != nil {
		return p, err
	}
	h, err := hex.DecodeString(p.ManifestSHA256)
	if p.Schema != 1 || p.ComponentID != m.ComponentID || p.Version != m.Version || err != nil || len(h) != 32 || !strings.EqualFold(p.ManifestSHA256, manifestSHA256) {
		return p, fmt.Errorf("startup report build identity mismatch")
	}
	if p.Inference != "not_tested" || (p.ProtocolEvidence != "not_declared" && p.ProtocolEvidence != "declared_not_tested") || (len(m.Protocols) == 0) != (p.ProtocolEvidence == "not_declared") {
		return p, fmt.Errorf("report cannot upgrade protocol or inference evidence")
	}
	switch p.StartupTest {
	case "not_tested":
		if p.MetadataOnly != nil {
			return p, fmt.Errorf("untested report contains execution evidence")
		}
	case "passed", "failed":
		a := p.MetadataOnly
		if a == nil || len(a.Arguments) != 1 || (a.Arguments[0] != "--component-info" && !(m.ComponentID == "whisper-stream" && a.Arguments[0] == "--metadata") && !(m.ComponentID == "haar-consumer" && a.Arguments[0] == "--haar-component-info")) || a.ExitCode == nil || a.CheckedAt.IsZero() || !a.NoModelLoad || !a.NoNetwork || !a.NoCompilation || (p.StartupTest == "passed") != (*a.ExitCode == 0) {
			return p, fmt.Errorf("invalid metadata-only startup evidence")
		}
	default:
		return p, fmt.Errorf("invalid startup_test")
	}
	switch p.CleanEnvironment {
	case "not_verified":
		if p.CleanEnvironmentEvidence != nil {
			return p, fmt.Errorf("unverified environment contains validation evidence")
		}
	case "validated":
		a := p.CleanEnvironmentEvidence
		if p.StartupTest != "passed" || a == nil || (a.Kind != "vm" && a.Kind != "container") || strings.TrimSpace(a.EnvironmentRef) == "" || strings.TrimSpace(a.EvidenceRef) == "" || !a.NoHostPython || !a.NoHostCompiler || !a.NoDevelopmentMounts {
			return p, fmt.Errorf("invalid clean environment evidence; PATH-only is insufficient")
		}
	default:
		return p, fmt.Errorf("invalid clean_environment")
	}
	return p, nil
}
