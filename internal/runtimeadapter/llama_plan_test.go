package runtimeadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/runtimeprocess"
)

// Test-only legacy helpers: production always routes llama through the shim.
func llamaPlan(r Resolved, vision bool) (runtimeprocess.VerifiedBinary, []string, error) {
	return llamaPlanWithCPU(r, vision, llamaHostAVX2)
}

func llamaPlanWithCPU(r Resolved, vision bool, avx2 func() bool) (runtimeprocess.VerifiedBinary, []string, error) {
	b, model, projector, err := llamaBindingPlan(r, vision, avx2)
	if err != nil {
		return runtimeprocess.VerifiedBinary{}, nil, err
	}
	args := []string{"--host", "127.0.0.1", "--offline", "--no-webui", "--no-warmup", "--parallel", "1", "--ctx-size", "4096", "--n-gpu-layers", "0", "--threads", "4", "--no-mmproj-offload", "--model", model}
	if vision {
		args = append(args, "--mmproj", projector)
	} else {
		args = append(args, "--no-mmproj")
	}
	return b, args, nil
}

func startLlama(ctx context.Context, s *runtimeprocess.Supervisor, b runtimeprocess.VerifiedBinary, args []string) (*runtimeprocess.Process, error) {
	return s.Start(ctx, b, runtimeprocess.Options{Args: args, MaxStdinBytes: 1, MaxStdoutBytes: 16 << 20, MaxStderrBytes: 16 << 20, BufferBytes: 4 << 20})
}

func llamaHTTP() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, DisableKeepAlives: true, MaxResponseHeaderBytes: 32 << 10}, Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func llamaReady(ctx context.Context, p *runtimeprocess.Process, b runtimeprocess.VerifiedBinary, base string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	c := llamaHTTP()
	defer c.CloseIdleConnections()
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	for {
		if err := p.CheckIdentity(b); err != nil {
			return err
		}
		hc, stop := context.WithTimeout(ctx, time.Second)
		req, _ := http.NewRequestWithContext(hc, http.MethodGet, base+"/health", nil)
		resp, err := c.Do(req)
		ready := false
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
			resp.Body.Close()
			var health struct {
				Status string `json:"status"`
			}
			ready = readErr == nil && len(data) <= 4096 && resp.StatusCode == 200 && json.Unmarshal(data, &health) == nil && health.Status == "ok"
		}
		stop()
		if ready {
			return p.CheckIdentity(b)
		}
		select {
		case <-ctx.Done():
			return errors.New("center_llama_load_failed")
		case <-timer.C:
		}
	}
}
