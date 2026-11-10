// Package componentclient speaks the shim's loopback HTTP/WS contract. It
// enforces bearer auth, bounded bodies, no redirects and no proxy use.
package componentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
	"github.com/coder/websocket"
)

type Client struct {
	base  string
	token string
	http  *http.Client
}

func New(base, token string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.Path != "" || u.RawQuery != "" || u.Host == "" {
		return nil, errors.New("component_base_invalid")
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || len(token) < 32 {
		return nil, errors.New("component_base_invalid")
	}
	return &Client{base: base, token: token, http: &http.Client{
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: false, ForceAttemptHTTP2: false},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 0,
	}}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

func (c *Client) Health(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/healthz", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("component_health_%d", resp.StatusCode)
	}
	return nil
}

func (c *Client) WaitHealthy(ctx context.Context) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		if err := c.Health(ctx); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("component_unavailable: %w", last)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Client) SubmitJob(ctx context.Context, req componentprotocol.JobRequest) (componentprotocol.JobSnapshot, error) {
	var snap componentprotocol.JobSnapshot
	raw, err := json.Marshal(req)
	if err != nil {
		return snap, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/jobs", raw)
	if err != nil {
		return snap, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return snap, responseError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, componentprotocol.MaxControlJSON)).Decode(&snap); err != nil {
		return snap, errors.New("component_response_invalid")
	}
	return snap, nil
}

func (c *Client) Job(ctx context.Context, id string) (componentprotocol.JobSnapshot, error) {
	var snap componentprotocol.JobSnapshot
	resp, err := c.do(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id), nil)
	if err != nil {
		return snap, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return snap, responseError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, componentprotocol.MaxControlJSON)).Decode(&snap); err != nil {
		return snap, errors.New("component_response_invalid")
	}
	return snap, nil
}

func (c *Client) WaitJob(ctx context.Context, id string) (componentprotocol.JobSnapshot, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		snap, err := c.Job(ctx, id)
		if err != nil {
			return snap, err
		}
		if snap.State.Terminal() {
			return snap, nil
		}
		select {
		case <-ctx.Done():
			return snap, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Client) CancelJob(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/cancel", []byte{})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError(resp)
	}
	return nil
}

func (c *Client) DownloadArtifact(ctx context.Context, id, name string, max int64) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id)+"/artifacts/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("component_artifact_too_large")
	}
	return data, nil
}

func (c *Client) Classify(ctx context.Context, contentType string, image []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/face/classify", bytes.NewReader(image))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", responseError(resp)
	}
	var out struct {
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, componentprotocol.MaxControlJSON)).Decode(&out); err != nil {
		return "", errors.New("component_response_invalid")
	}
	switch out.Kind {
	case "frontal", "profile", "no_face":
		return out.Kind, nil
	}
	return "", errors.New("component_response_invalid")
}

func responseError(resp *http.Response) error {
	var e componentprotocol.Error
	_ = json.NewDecoder(io.LimitReader(resp.Body, componentprotocol.MaxControlJSON)).Decode(&e)
	if e.Code == "" {
		return fmt.Errorf("component_http_%d", resp.StatusCode)
	}
	return &e
}

// Stream is one authenticated WebSocket session. One connection carries exactly
// one center-side stream request.
type Stream struct {
	c *websocket.Conn
}

func (c *Client) DialStream(ctx context.Context) (*Stream, error) {
	wsURL := "ws" + strings.TrimPrefix(c.base, "http") + "/v1/stream"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + c.token}}})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(componentprotocol.MaxWSFrame + 1024)
	return &Stream{c: conn}, nil
}

func (s *Stream) Send(ctx context.Context, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(raw) > componentprotocol.MaxWSFrame {
		return errors.New("component_frame_too_large")
	}
	return s.c.Write(ctx, websocket.MessageText, raw)
}

func (s *Stream) ReadRaw(ctx context.Context) ([]byte, error) {
	_, data, err := s.c.Read(ctx)
	if err != nil {
		return nil, err
	}
	if len(data) > componentprotocol.MaxWSFrame {
		return nil, errors.New("component_frame_too_large")
	}
	return data, nil
}

// ExpectNormalClose verifies the shim closed the session cleanly after the
// terminal frame. Any extra data frame is rejected.
func (s *Stream) ExpectNormalClose(ctx context.Context) error {
	_, data, err := s.c.Read(ctx)
	if err == nil {
		_ = data
		return errors.New("component_frame_extra")
	}
	if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
		return nil
	}
	return errors.New("component_stream_failed")
}

func (s *Stream) Close() {
	_ = s.c.Close(websocket.StatusNormalClosure, "")
}
