package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
)

const (
	llamaMaxRequest  = 4 << 20
	llamaMaxResponse = 8 << 20
)

func (s *Server) startLlama(w http.ResponseWriter, r *http.Request) {
	if !s.requireMode(w, ModeLlama) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, componentprotocol.MaxControlJSON))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, componentprotocol.Fail("component_too_large"))
		return
	}
	req, err := componentprotocol.DecodeLlamaStart(body)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, componentprotocol.Fail("component_request"))
		return
	}
	if !ordinaryFile(req.Model) || req.Vision && !ordinaryFile(req.MMProj) {
		writeError(w, http.StatusUnprocessableEntity, componentprotocol.Fail("component_request"))
		return
	}
	s.llamaMu.Lock()
	defer s.llamaMu.Unlock()
	if s.llamaStarted {
		writeError(w, http.StatusConflict, componentprotocol.Fail("component_conflict"))
		return
	}
	child, cancel, base, err := s.launchLlama(req, 999)
	if err != nil && !errors.Is(err, errLlamaPort) {
		// Full GPU offload did not become healthy; retry once entirely on CPU.
		child, cancel, base, err = s.launchLlama(req, 0)
	}
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errLlamaPort) {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, componentprotocol.Fail("component_failed"))
		return
	}
	s.llamaChild, s.llamaCancel, s.llamaBase, s.llamaStarted = child, cancel, base, true
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "protocol": componentprotocol.LlamaHTTPv1})
}

var errLlamaPort = errors.New("shim: no llama port")

// launchLlama starts one llama-server with the requested GPU layer count and
// waits for health. The caller owns the returned child on success; on failure
// the child is already closed and cancelled.
func (s *Server) launchLlama(req componentprotocol.LlamaStartRequest, gpuLayers int) (Child, context.CancelFunc, string, error) {
	port, err := freeLlamaPort()
	if err != nil {
		return nil, nil, "", errLlamaPort
	}
	args := []string{
		"--host", "127.0.0.1", "--offline", "--no-webui", "--no-warmup",
		// llama-server divides --ctx-size across --parallel slots (n_ctx_slot =
		// ctx/parallel), so ctx must scale with slots to keep 32768 tokens per
		// candidate; total KV stays ctx-sized regardless of slot count.
		"--parallel", "4", "--ctx-size", "131072", "--n-gpu-layers", strconv.Itoa(gpuLayers),
		"--threads", strconv.Itoa(llamaThreads()), "--no-mmproj-offload", "--model", req.Model,
	}
	if req.Vision {
		args = append(args, "--mmproj", req.MMProj)
	} else {
		args = append(args, "--no-mmproj")
	}
	args = append(args, "--port", strconv.Itoa(port))
	ctx, cancel := context.WithCancel(context.Background())
	child, err := s.startChild(ctx, s.cfg.Target, args, Limits{In: 1, Out: 16 << 20, Err: 16 << 20, Buffer: 4 << 20})
	if err != nil {
		cancel()
		return nil, nil, "", err
	}
	go func() { _, _ = io.Copy(io.Discard, child.Stdout()) }()
	go func() { _, _ = io.Copy(io.Discard, child.Stderr()) }()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	if err := waitLlamaHealth(ctx, base); err != nil {
		_ = child.Close()
		cancel()
		return nil, nil, "", err
	}
	return child, cancel, base, nil
}

func (s *Server) stopLlama(w http.ResponseWriter, r *http.Request) {
	if !s.requireMode(w, ModeLlama) {
		return
	}
	s.closeLlama()
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func llamaThreads() int {
	n := runtime.NumCPU()
	if n > 16 {
		n = 16
	}
	if n < 4 {
		n = 4
	}
	return n
}

func (s *Server) closeLlama() {
	s.llamaMu.Lock()
	defer s.llamaMu.Unlock()
	if s.llamaCancel != nil {
		s.llamaCancel()
		s.llamaCancel = nil
	}
	if s.llamaChild != nil {
		_ = s.llamaChild.Close()
		s.llamaChild = nil
	}
	s.llamaBase, s.llamaStarted = "", false
}

func (s *Server) llamaProxyHealth(w http.ResponseWriter, r *http.Request) {
	if !s.requireMode(w, ModeLlama) {
		return
	}
	s.llamaMu.Lock()
	base, started := s.llamaBase, s.llamaStarted
	s.llamaMu.Unlock()
	if !started {
		writeError(w, http.StatusServiceUnavailable, componentprotocol.Fail("component_failed"))
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, base+"/health", nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, componentprotocol.Fail("component_failed"))
		return
	}
	resp, err := llamaUpstreamClient().Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, componentprotocol.Fail("component_failed"))
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(body) > 4096 {
		writeError(w, http.StatusBadGateway, componentprotocol.Fail("component_failed"))
		return
	}
	writeJSON(w, resp.StatusCode, json.RawMessage(body))
}

func (s *Server) llamaProxyChat(w http.ResponseWriter, r *http.Request) {
	if !s.requireMode(w, ModeLlama) {
		return
	}
	s.llamaMu.Lock()
	base, started := s.llamaBase, s.llamaStarted
	s.llamaMu.Unlock()
	if !started {
		writeError(w, http.StatusServiceUnavailable, componentprotocol.Fail("component_failed"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, llamaMaxRequest))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, componentprotocol.Fail("component_too_large"))
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, componentprotocol.Fail("component_failed"))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := llamaUpstreamClient().Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, componentprotocol.Fail("component_failed"))
		return
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, llamaMaxResponse+1))
	if err != nil || len(payload) > llamaMaxResponse {
		writeError(w, http.StatusBadGateway, componentprotocol.Fail("component_failed"))
		return
	}
	if contentType := resp.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(payload)
}

// llamaProxyFixture is compiled in production but only registered when the
// test-only Config flag is set. It gives adapter tests a side channel to the
// fake upstream without exposing arbitrary paths.
func (s *Server) llamaProxyFixture(w http.ResponseWriter, r *http.Request) {
	s.llamaMu.Lock()
	base, started := s.llamaBase, s.llamaStarted
	s.llamaMu.Unlock()
	if !started {
		writeError(w, http.StatusServiceUnavailable, componentprotocol.Fail("component_failed"))
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, base+r.URL.Path, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, componentprotocol.Fail("component_failed"))
		return
	}
	resp, err := llamaUpstreamClient().Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, componentprotocol.Fail("component_failed"))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func freeLlamaPort() (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	if port == 11434 {
		return 0, errors.New("shim: reserved llama port")
	}
	return port, nil
}

func waitLlamaHealth(ctx context.Context, base string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		hc, stop := context.WithTimeout(ctx, time.Second)
		req, err := http.NewRequestWithContext(hc, http.MethodGet, base+"/health", nil)
		if err == nil {
			resp, err := llamaUpstreamClient().Do(req)
			ready := false
			if err == nil {
				data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
				resp.Body.Close()
				var health struct {
					Status string `json:"status"`
				}
				ready = readErr == nil && len(data) <= 4096 && resp.StatusCode == http.StatusOK && json.Unmarshal(data, &health) == nil && health.Status == "ok"
			}
			stop()
			if ready {
				return nil
			}
		} else {
			stop()
		}
		select {
		case <-ctx.Done():
			return errors.New("shim: llama load failed")
		case <-ticker.C:
		}
	}
}

var llamaUpstream = &http.Client{
	Transport: &http.Transport{
		Proxy:                  nil,
		DisableKeepAlives:      false,
		ForceAttemptHTTP2:      false,
		MaxResponseHeaderBytes: 32 << 10,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func llamaUpstreamClient() *http.Client { return llamaUpstream }
