//go:build windows

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Keep the real Windows process, delivery and named-pipe checks from the session
// fixture; only redirect its authenticated read endpoint to a controlled server.
func capabilitiesClientFixture(t *testing.T, handler func(http.ResponseWriter, *http.Request, RuntimeCapabilities)) (*Client, *atomic.Int32, RuntimeCapabilities) {
	t.Helper()
	c := sessionFixture(t)
	h := c.cache.Session.Response
	v := RuntimeCapabilities{Schema: 1, InstanceID: h.Descriptor.InstanceID, Build: h.Build, ObservedAt: time.Now().UTC()}
	for _, scene := range RuntimeScenes() {
		v.Capabilities = append(v.Capabilities, RuntimeCapability{Scene: scene.ID, State: CapabilityMissingBinding})
	}
	calls := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != RuntimeCapabilitiesPath || r.URL.RawQuery != "" || r.Header.Get(ReadOnlyHeader) != h.Token || r.Header.Get(RuntimeHeader) != "" || r.Header.Get(ManagementHeader) != "" {
			t.Error("unexpected capabilities request or credentials")
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		handler(w, r, v)
	}))
	t.Cleanup(srv.Close)
	h.Descriptor.LoopbackAddress = strings.TrimPrefix(srv.URL, "http://")
	raw, err := json.Marshal(h.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	writeDeliveryFile(t, filepath.Join(c.cfg.DataDir, DescriptorName), raw)
	c.cache.Session.Response = h
	return c, calls, v
}

func TestWindowsRuntimeCapabilitiesClientTrustedRead(t *testing.T) {
	c, calls, want := capabilitiesClientFixture(t, func(w http.ResponseWriter, _ *http.Request, v RuntimeCapabilities) {
		_ = json.NewEncoder(w).Encode(v)
	})
	first := c.cache.Session
	for i := 0; i < 2; i++ {
		got, err := c.RuntimeCapabilities(context.Background())
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("snapshot changed: %+v err=%v", got, err)
		}
		if c.cache.Session != first {
			t.Fatal("read renewed credentials")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected request count", calls.Load())
	}
}

func TestWindowsRuntimeCapabilitiesClientFailures(t *testing.T) {
	for _, mode := range []string{"instance", "build", "schema", "observed", "partial", "duplicate", "state", "binding", "component", "inference", "reason", "malformed", "trailing", "oversize", "401", "cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, calls, _ := capabilitiesClientFixture(t, func(w http.ResponseWriter, r *http.Request, v RuntimeCapabilities) {
				switch mode {
				case "instance":
					v.InstanceID = "other"
				case "build":
					v.Build = "other"
				case "schema":
					v.Schema++
				case "observed":
					v.ObservedAt = time.Time{}
				case "partial":
					v.Capabilities = v.Capabilities[1:]
				case "duplicate":
					v.Capabilities[1] = v.Capabilities[0]
				case "state":
					v.Capabilities[0].State = "ready"
				case "binding":
					v.Capabilities[0].Binding = &ExecutionBindingRef{Scene: v.Capabilities[0].Scene}
				case "component":
					v.Capabilities[0].Component = &ExecutionComponentRef{ID: "unknown"}
				case "inference":
					v.Capabilities[0].Inference = &InferenceEvidence{State: "passed"}
				case "reason":
					v.Capabilities[0].Reason = `C:\private\models`
				case "malformed":
					_, _ = w.Write([]byte(`{"schema":`))
					return
				case "oversize":
					// Valid JSON followed by whitespace must still obey MaxResponse.
					_ = json.NewEncoder(w).Encode(v)
					_, _ = w.Write([]byte(strings.Repeat(" ", MaxResponse)))
					return
				case "401":
					http.Error(w, "denied", http.StatusUnauthorized)
					return
				case "cancel", "timeout":
					if mode == "cancel" {
						cancel()
					}
					<-r.Context().Done()
					return
				}
				_ = json.NewEncoder(w).Encode(v)
				if mode == "trailing" {
					_, _ = w.Write([]byte(`{}`))
				}
			})
			started := time.Now()
			got, err := c.RuntimeCapabilities(ctx)
			if err == nil || !reflect.DeepEqual(got, RuntimeCapabilities{}) || !reflect.DeepEqual(c.cache, cacheEntry{}) {
				t.Fatalf("failure retained data/cache: %+v err=%v", got, err)
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost", err)
			}
			if mode == "timeout" && (!errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 6*time.Second) {
				t.Fatal("three-second deadline not enforced", err)
			}
			if _, err = c.RuntimeCapabilities(context.Background()); !errors.Is(err, ErrUnconfirmed) {
				t.Fatal("passive retry revived trust", err)
			}
			if calls.Load() != 1 {
				t.Fatal("read retried HTTP", calls.Load())
			}
		})
	}
}

func TestWindowsRuntimeCapabilitiesClientPassive(t *testing.T) {
	c, calls, _ := capabilitiesClientFixture(t, func(http.ResponseWriter, *http.Request, RuntimeCapabilities) {
		t.Error("unconfirmed read reached HTTP")
	})
	// An invalid manifest distinguishes passive metadata checks from first hash /
	// delivery verification, which would fail with a different error.
	writeDeliveryFile(t, c.delivery(), []byte("not a delivery manifest"))
	c.cache = cacheEntry{}
	for i := 0; i < 2; i++ {
		got, err := c.RuntimeCapabilities(context.Background())
		if !errors.Is(err, ErrUnconfirmed) || !reflect.DeepEqual(got, RuntimeCapabilities{}) || !reflect.DeepEqual(c.cache, cacheEntry{}) {
			t.Fatal("passive read verified a new build", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unconfirmed HTTP requests", calls.Load())
	}
	// No descriptor: a passive read must not enter the startup path or create data.
	data := filepath.Join(t.TempDir(), "absent")
	c.cfg.DataDir = data
	if _, err := c.RuntimeCapabilities(context.Background()); err == nil {
		t.Fatal("missing center accepted")
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("passive read created integration data", err)
	}
}
