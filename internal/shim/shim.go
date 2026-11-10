// Package shim adapts legacy local component processes to a bounded loopback
// HTTP/WS contract. It is the only place that still speaks stdio/CLI to them.
package shim

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
	"github.com/aura-bootstrap/fengshen_model_center/internal/upstream/facehaar"
)

const (
	ModeWhisperStream = "whisper-stream"
	ModeWhisperBatch  = "whisper-batch"
	ModeTimbre        = "timbre"
	ModeHaar          = "haar"
	ModeLlama         = "llama"
)

type Config struct {
	Mode       string
	Token      string
	Target     string
	WorkRoot   string
	FrontalXML string
	ProfileXML string
	StartChild StartChild // test injection only

	// TestLlamaControl proxies /fixture/* to the fake upstream. Tests only.
	TestLlamaControl bool
}

type Server struct {
	cfg        Config
	startChild StartChild
	detector   *facehaar.Detector

	mu     sync.Mutex
	jobs   map[string]*job
	active *job
	closed bool

	llamaMu      sync.Mutex
	llamaChild   Child
	llamaCancel  context.CancelFunc
	llamaBase    string
	llamaKey     string
	llamaStarted bool
}

func New(cfg Config) (*Server, error) {
	switch cfg.Mode {
	case ModeWhisperStream, ModeWhisperBatch, ModeTimbre, ModeHaar, ModeLlama:
	default:
		return nil, errors.New("shim_mode_invalid")
	}
	if len(cfg.Token) < 32 {
		return nil, errors.New("shim_token_invalid")
	}
	if !filepath.IsAbs(cfg.WorkRoot) {
		return nil, errors.New("shim_workroot_invalid")
	}
	if err := os.MkdirAll(cfg.WorkRoot, 0700); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, startChild: cfg.StartChild, jobs: map[string]*job{}}
	if s.startChild == nil {
		s.startChild = processLauncher()
	}
	if cfg.Mode != ModeHaar {
		if !ordinaryFile(cfg.Target) || !strings.EqualFold(filepath.Ext(cfg.Target), ".exe") {
			return nil, errors.New("shim_target_invalid")
		}
	}
	if cfg.Mode == ModeHaar {
		frontal, err := readBounded(cfg.FrontalXML, facehaar.MaxImageBytes)
		if err != nil {
			return nil, fmt.Errorf("shim_haar_invalid: %w", err)
		}
		profile, err := readBounded(cfg.ProfileXML, facehaar.MaxImageBytes)
		if err != nil {
			return nil, fmt.Errorf("shim_haar_invalid: %w", err)
		}
		detector, err := facehaar.New(frontal, profile)
		if err != nil {
			return nil, fmt.Errorf("shim_haar_invalid: %w", err)
		}
		s.detector = detector
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/stream", s.stream)
	mux.HandleFunc("POST /v1/jobs", s.submitJob)
	mux.HandleFunc("GET /v1/jobs/{id}", s.jobSnapshot)
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", s.cancelJob)
	mux.HandleFunc("GET /v1/jobs/{id}/events", s.jobEvents)
	mux.HandleFunc("GET /v1/jobs/{id}/artifacts/{name}", s.jobArtifact)
	mux.HandleFunc("POST /v1/face/classify", s.classify)
	mux.HandleFunc("POST /v1/llama/start", s.startLlama)
	mux.HandleFunc("POST /v1/llama/stop", s.stopLlama)
	mux.HandleFunc("GET /health", s.llamaProxyHealth)
	mux.HandleFunc("POST /v1/chat/completions", s.llamaProxyChat)
	if s.cfg.TestLlamaControl {
		mux.HandleFunc("GET /fixture/", s.llamaProxyFixture)
		mux.HandleFunc("POST /fixture/", s.llamaProxyFixture)
	}
	return s.authorize(mux)
}

func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	active := s.active
	jobs := make([]*job, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	s.mu.Unlock()
	if active != nil {
		active.cancel()
	}
	s.closeLlama()
	for _, j := range jobs {
		<-j.done
		_ = os.RemoveAll(j.dir)
	}
	return nil
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "mode": s.cfg.Mode})
}

func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopback(r.RemoteAddr) {
			writeError(w, http.StatusForbidden, componentprotocol.Fail("component_auth"))
			return
		}
		for _, h := range []string{"Proxy-Authorization", "Cookie", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP"} {
			if len(r.Header.Values(h)) != 0 {
				writeError(w, http.StatusForbidden, componentprotocol.Fail("component_auth"))
				return
			}
		}
		values := r.Header.Values("Authorization")
		want := "Bearer " + s.cfg.Token
		if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte(want)) != 1 {
			writeError(w, http.StatusForbidden, componentprotocol.Fail("component_auth"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) requireMode(w http.ResponseWriter, modes ...string) bool {
	for _, mode := range modes {
		if s.cfg.Mode == mode {
			return true
		}
	}
	writeError(w, http.StatusNotFound, componentprotocol.Fail("component_not_found"))
	return false
}

func ordinaryFile(p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	st, err := os.Lstat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&os.ModeSymlink == 0 && st.Size() > 0
}

func ordinaryDir(p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	st, err := os.Lstat(p)
	return err == nil && st.IsDir() && st.Mode()&os.ModeSymlink == 0
}

func readBounded(p string, max int64) ([]byte, error) {
	if !ordinaryFile(p) {
		return nil, errors.New("not an ordinary file")
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, errors.New("file exceeds limit")
	}
	return b, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, e *componentprotocol.Error) {
	writeJSON(w, status, e)
}
