//go:build windows

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRuntimeClientDownloadWAVTransport(t *testing.T) {
	c, s := runtimeClientFixture(t, "timbre.convert")
	binding := s.Grant().Bindings[0]
	body := RuntimeBody{Component: ExecutionComponentRef{ID: "seedvc-runner", Version: "v1", ManifestSHA256: strings.Repeat("b", 64), Protocol: "seed/v1"}, Convert: &RuntimeConvertInput{AssetID: "asset", ReferenceAssetID: "reference"}}
	frozen, _ := json.Marshal(struct {
		Binding ExecutionBindingRef
		Body    RuntimeBody
	}{binding, body})
	hash := sha256.Sum256(frozen)
	snap := RuntimeSnapshot{ID: "job", SessionID: s.Grant().SessionID, TaskID: s.Grant().TaskID, RequestID: "request", Binding: binding, BodySHA256: hex.EncodeToString(hash[:]), State: "succeeded", CreatedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(), Cursor: 4}
	job, e := c.Resume(s, snap, body)
	if e != nil {
		t.Fatal(e)
	}
	wav := runtimeTestWAV()
	sum := sha256.Sum256(wav)
	media := RuntimeMediaResult{ID: "media", Bytes: strconv.Itoa(len(wav)), SHA256: hex.EncodeToString(sum[:]), SampleRate: 8000, Channels: 1, DurationMS: 1000}
	mode := "good"
	downloads := 0
	c.http.Transport = runtimeTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get(RuntimeHeader) == "" {
			t.Fatal("download not authenticated")
		}
		switch r.URL.Path {
		case RuntimeJobsPath + "/job":
			return runtimeTestJSON(200, snap), nil
		case RuntimeJobsPath + "/job/results/job":
			return runtimeTestJSON(200, RuntimeResult{Component: body.Component, Media: []RuntimeMediaResult{media}}), nil
		case RuntimeJobsPath + "/job/results/media":
			downloads++
			data := append([]byte(nil), wav...)
			header := media.SHA256
			n := int64(len(data))
			switch mode {
			case "hash":
				data[len(data)-1] = 1
			case "header":
				header = strings.Repeat("0", 64)
			case "length":
				n--
			case "truncate":
				data = data[:len(data)-1]
			case "format":
				data[20] = 3
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"audio/wav"}, "X-Content-Sha256": []string{header}}, ContentLength: n, Body: io.NopCloser(bytes.NewReader(data))}, nil
		}
		t.Fatal("unexpected request", r.URL.Path)
		return nil, errors.New("bad route")
	})
	for _, m := range []string{"good", "hash", "header", "length", "truncate", "format"} {
		mode = m
		var dst bytes.Buffer
		e := c.DownloadWAV(context.Background(), s, job, "media", &dst)
		if (e == nil) != (m == "good") {
			t.Fatal(m, e)
		}
		if m == "good" && !bytes.Equal(dst.Bytes(), wav) {
			t.Fatal("download differs")
		}
	}
	before := downloads
	if e = c.DownloadWAV(context.Background(), s, job, "other", io.Discard); e == nil || downloads != before {
		t.Fatal("unknown result fetched")
	}
}

type runtimeCancelBody struct {
	ctx    context.Context
	closed bool
}

func (b *runtimeCancelBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b *runtimeCancelBody) Close() error             { b.closed = true; return nil }
func TestRuntimeClientEventsContextOnlyDisconnects(t *testing.T) {
	c, s := runtimeClientFixture(t)
	body := runtimeTestBody()
	binding := s.Grant().Bindings[0]
	frozen, _ := json.Marshal(struct {
		Binding ExecutionBindingRef
		Body    RuntimeBody
	}{binding, body})
	sum := sha256.Sum256(frozen)
	snap := RuntimeSnapshot{ID: "job", SessionID: s.Grant().SessionID, TaskID: s.Grant().TaskID, RequestID: "request", Binding: binding, BodySHA256: hex.EncodeToString(sum[:]), State: "queued", CreatedAt: time.Now().UTC(), Cursor: 1}
	job, e := c.Resume(s, snap, body)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &runtimeCancelBody{ctx: ctx}
	calls := 0
	c.http.Transport = runtimeTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.URL.Path != RuntimeJobsPath+"/job/events" || r.Header.Get("Last-Event-ID") != "0" || r.Header.Get(RuntimeHeader) == "" {
			t.Fatal("unexpected event request")
		}
		cancel()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, ContentLength: -1, Body: reader}, nil
	})
	if e = c.Events(ctx, s, job, 0, func(RuntimeEvent) error { return nil }); !errors.Is(e, context.Canceled) || !reader.closed || calls != 1 {
		t.Fatal("cancelled job or leaked connection", calls, e)
	}
	c.http.Transport = runtimeTestTransport(func(*http.Request) (*http.Response, error) {
		return runtimeTestJSON(410, struct {
			Code string `json:"code"`
		}{"center_event_cursor_expired"}), nil
	})
	if e = c.Events(context.Background(), s, job, 0, func(RuntimeEvent) error { return nil }); !errors.Is(e, ErrRuntimeEventCursorExpired) {
		t.Fatal(e)
	}
}
