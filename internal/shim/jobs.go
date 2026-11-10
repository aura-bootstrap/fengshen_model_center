package shim

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aura-bootstrap/fengshen_model_center/internal/componentprotocol"
	"github.com/coder/websocket"
)

type job struct {
	id     string
	kind   string
	dir    string
	cancel context.CancelFunc

	mu      sync.Mutex
	snap    componentprotocol.JobSnapshot
	changed chan struct{}
	done    chan struct{}
}

func newJob(id, kind, dir string, cancel context.CancelFunc) *job {
	return &job{
		id: id, kind: kind, dir: dir, cancel: cancel,
		snap:    componentprotocol.JobSnapshot{ID: id, State: componentprotocol.JobRunning},
		changed: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

func (j *job) update(fn func(*componentprotocol.JobSnapshot)) {
	j.mu.Lock()
	fn(&j.snap)
	j.snap.Seq++
	j.mu.Unlock()
	select {
	case j.changed <- struct{}{}:
	default:
	}
}

func (j *job) snapshot() componentprotocol.JobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snap
}

func (s *Server) submitJob(w http.ResponseWriter, r *http.Request) {
	if !s.requireMode(w, ModeWhisperBatch, ModeTimbre) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, componentprotocol.MaxControlJSON))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, componentprotocol.Fail("component_too_large"))
		return
	}
	req, err := componentprotocol.DecodeJobRequest(body)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, componentprotocol.Fail("component_request"))
		return
	}
	if s.cfg.Mode == ModeWhisperBatch && req.Kind != "whisper-batch" || s.cfg.Mode == ModeTimbre && req.Kind != "demucs" && req.Kind != "seedvc" {
		writeError(w, http.StatusUnprocessableEntity, componentprotocol.Fail("component_request"))
		return
	}
	if err := validateBindings(req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, componentprotocol.Fail("component_request"))
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, componentprotocol.Fail("component_failed"))
		return
	}
	if s.active != nil {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, componentprotocol.Fail("component_conflict"))
		return
	}
	id, err := newJobID()
	if err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusInternalServerError, componentprotocol.Fail("component_failed"))
		return
	}
	dir, err := os.MkdirTemp(s.cfg.WorkRoot, "job-")
	if err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusInternalServerError, componentprotocol.Fail("component_failed"))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := newJob(id, req.Kind, dir, cancel)
	s.jobs[id] = j
	s.active = j
	s.mu.Unlock()

	go s.runJob(ctx, j, req)
	writeJSON(w, http.StatusAccepted, j.snapshot())
}

func (s *Server) jobSnapshot(w http.ResponseWriter, r *http.Request) {
	j := s.findJob(r.PathValue("id"))
	if j == nil {
		writeError(w, http.StatusNotFound, componentprotocol.Fail("component_not_found"))
		return
	}
	writeJSON(w, http.StatusOK, j.snapshot())
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	j := s.findJob(r.PathValue("id"))
	if j == nil {
		writeError(w, http.StatusNotFound, componentprotocol.Fail("component_not_found"))
		return
	}
	j.cancel()
	select {
	case <-j.done:
	case <-time.After(10 * time.Second):
	}
	writeJSON(w, http.StatusOK, j.snapshot())
}

func (s *Server) jobEvents(w http.ResponseWriter, r *http.Request) {
	j := s.findJob(r.PathValue("id"))
	if j == nil {
		writeError(w, http.StatusNotFound, componentprotocol.Fail("component_not_found"))
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	send := func() error {
		raw, _ := json.Marshal(j.snapshot())
		return c.Write(ctx, websocket.MessageText, raw)
	}
	if err := send(); err != nil {
		return
	}
	for {
		if j.snapshot().State.Terminal() {
			return
		}
		select {
		case <-j.changed:
			if err := send(); err != nil {
				return
			}
		case <-j.done:
			_ = send()
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) jobArtifact(w http.ResponseWriter, r *http.Request) {
	j := s.findJob(r.PathValue("id"))
	if j == nil {
		writeError(w, http.StatusNotFound, componentprotocol.Fail("component_not_found"))
		return
	}
	name := r.PathValue("name")
	if filepath.Base(name) != name || !componentprotocol.ArtifactAllowed(j.kind, name) {
		writeError(w, http.StatusNotFound, componentprotocol.Fail("component_not_found"))
		return
	}
	path := filepath.Join(j.dir, name)
	if !ordinaryFile(path) {
		writeError(w, http.StatusNotFound, componentprotocol.Fail("component_not_found"))
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, componentprotocol.Fail("component_not_found"))
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, componentprotocol.Fail("component_not_found"))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (s *Server) findJob(id string) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

func (s *Server) runJob(ctx context.Context, j *job, req componentprotocol.JobRequest) {
	defer close(j.done)
	defer func() {
		s.mu.Lock()
		if s.active == j {
			s.active = nil
		}
		s.mu.Unlock()
	}()
	args, err := jobArgs(req, j.dir)
	if err != nil {
		j.update(func(sn *componentprotocol.JobSnapshot) {
			sn.State = componentprotocol.JobFailed
			sn.Error = componentprotocol.Fail("component_request")
		})
		return
	}
	child, err := s.startChild(ctx, s.cfg.Target, args, Limits{In: 1, Out: 4 << 20, Err: 4 << 20, Buffer: 4 << 20})
	if err != nil {
		j.update(func(sn *componentprotocol.JobSnapshot) {
			sn.State = componentprotocol.JobFailed
			sn.Error = componentprotocol.Fail("component_failed")
		})
		return
	}
	defer child.Close()
	go func() { _, _ = io.Copy(io.Discard, child.Stdout()) }()
	go func() { _, _ = io.Copy(io.Discard, child.Stderr()) }()
	err = child.Wait()
	if ctx.Err() != nil {
		j.update(func(sn *componentprotocol.JobSnapshot) {
			sn.State = componentprotocol.JobCancelled
			sn.Error = componentprotocol.Fail("component_cancelled")
		})
		return
	}
	if err != nil {
		j.update(func(sn *componentprotocol.JobSnapshot) {
			sn.State = componentprotocol.JobFailed
			sn.Error = componentprotocol.Fail("component_failed")
		})
		return
	}
	artifacts := componentprotocol.Artifacts(j.kind)
	for _, name := range artifacts {
		path := filepath.Join(j.dir, name)
		if !ordinaryFile(path) {
			j.update(func(sn *componentprotocol.JobSnapshot) {
				sn.State = componentprotocol.JobFailed
				sn.Error = componentprotocol.Fail("component_failed")
			})
			return
		}
		st, _ := os.Stat(path)
		if st.Size() > componentprotocol.MaxArtifactSize {
			j.update(func(sn *componentprotocol.JobSnapshot) {
				sn.State = componentprotocol.JobFailed
				sn.Error = componentprotocol.Fail("component_failed")
			})
			return
		}
	}
	j.update(func(sn *componentprotocol.JobSnapshot) {
		sn.State = componentprotocol.JobSucceeded
		sn.Progress = 100
		sn.Artifacts = artifacts
	})
}

func validateBindings(req componentprotocol.JobRequest) error {
	if !ordinaryFile(req.Input.AudioPath) {
		return componentprotocol.ErrInvalid
	}
	for key, paths := range req.Models {
		dir := key == "seed-whisper" || key == "bigvgan"
		for _, p := range paths {
			if dir && !ordinaryDir(p) || !dir && !ordinaryFile(p) {
				return componentprotocol.ErrInvalid
			}
		}
	}
	if req.Kind == "seedvc" {
		if !ordinaryFile(req.Input.ReferencePath) {
			return componentprotocol.ErrInvalid
		}
	}
	if req.Kind == "demucs" && !ordinaryFile(req.Bag) {
		return componentprotocol.ErrInvalid
	}
	return nil
}

func jobArgs(req componentprotocol.JobRequest, dir string) ([]string, error) {
	switch req.Kind {
	case "whisper-batch":
		asr := req.Models["whisper-asr"][0]
		args := []string{"-m", asr, "-f", req.Input.AudioPath, "-l", "zh", "-t", "4", "-ml", "1", "-sow", "-ojf", "-of", filepath.Join(dir, "transcript"), "-np", "-ng"}
		if req.Options.VAD {
			args = append(args, "--vad", "--vad-model", req.Models["whisper-vad"][0])
		}
		return args, nil
	case "demucs":
		model := "htdemucs"
		weights := req.Models["demucs"]
		if len(req.Models["demucs-ft"]) != 0 {
			model, weights = "htdemucs_ft", req.Models["demucs-ft"]
		}
		repo := filepath.Dir(weights[0])
		format := ""
		for _, w := range weights {
			if filepath.Dir(w) != repo {
				return nil, componentprotocol.ErrInvalid
			}
			ext := strings.ToLower(filepath.Ext(w))
			current := "hf-safetensors"
			if ext == ".th" {
				current = "th"
			} else if ext != ".safetensors" {
				return nil, componentprotocol.ErrInvalid
			}
			if format != "" && format != current {
				return nil, componentprotocol.ErrInvalid
			}
			format = current
		}
		args := []string{"--model", model, "--repo", repo, "--bag", req.Bag, "--weight-format", format, "--device", req.Options.Device}
		for _, w := range weights {
			args = append(args, "--weight", w)
		}
		return append(args, "--in", req.Input.AudioPath, "--outdir", dir), nil
	case "seedvc":
		var checkpoint, config string
		for _, p := range req.Models["seed-vc"] {
			switch strings.ToLower(filepath.Ext(p)) {
			case ".yml", ".yaml":
				config = p
			case ".pth", ".pt":
				checkpoint = p
			default:
				return nil, componentprotocol.ErrInvalid
			}
		}
		if checkpoint == "" || config == "" {
			return nil, componentprotocol.ErrInvalid
		}
		return []string{
			"--checkpoint", checkpoint, "--config", config,
			"--seed-whisper", req.Models["seed-whisper"][0],
			"--campplus", req.Models["campplus"][0],
			"--bigvgan", req.Models["bigvgan"][0],
			"--steps", "25", "--pitch", strconv.Itoa(req.Options.Pitch), "--device", req.Options.Device,
			"--source", req.Input.AudioPath, "--target", req.Input.ReferencePath,
			"--out", filepath.Join(dir, "converted.wav"),
		}, nil
	}
	return nil, componentprotocol.ErrInvalid
}

func newJobID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
