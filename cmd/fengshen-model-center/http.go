package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/aura-bootstrap/fengshen_model_center/internal/bindings"
	"github.com/aura-bootstrap/fengshen_model_center/internal/components"
	"github.com/aura-bootstrap/fengshen_model_center/internal/integration"
	"github.com/aura-bootstrap/fengshen_model_center/internal/registry"
)

func newHandler(manager *Manager, host string, integrationHost *integration.Host) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/report", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			writeAPIError(w, failure("request_invalid", "汇总报告不接受路径或查询参数", 400))
			return
		}
		value, err := manager.reportSnapshot()
		respond(w, http.StatusOK, value, err)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		state := manager.State()
		state.StorageError = state.Registry.Error
		writeJSON(w, 200, state)
	})
	mux.HandleFunc("GET /api/inventories/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := manager.Inventory(r.PathValue("id"))
		respond(w, 200, v, err)
	})
	mux.HandleFunc("GET /api/inventories/{id}/models/{model}/files", func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				writeAPIError(w, failure("page_invalid", "分页数量无效", 422))
				return
			}
			limit = n
		}
		files, next, err := manager.Files(r.PathValue("id"), r.PathValue("model"), r.URL.Query().Get("cursor"), limit)
		respond(w, 200, map[string]any{"files": files, "next_cursor": next}, err)
	})
	mux.HandleFunc("POST /api/operations/scan", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Revision *uint64 `json:"registry_revision"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		if in.Revision == nil {
			writeAPIError(w, failure("request_invalid", "缺少登记版本", 422))
			return
		}
		v, err := manager.StartScan(r.Context(), *in.Revision)
		respond(w, 202, v, err)
	})
	mux.HandleFunc("POST /api/operations/verify", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Inventory string   `json:"inventory_id"`
			Model     string   `json:"model_id"`
			Files     []string `json:"file_ids"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		v, err := manager.StartVerify(r.Context(), in.Inventory, in.Model, in.Files)
		respond(w, 202, v, err)
	})
	mux.HandleFunc("GET /api/components", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, manager.Components()) })
	mux.HandleFunc("POST /api/operations/verify-component", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID      string `json:"component_id"`
			Version string `json:"version"`
		}
		if !decodeBody(w, r, &in) {
			return
		}
		v, err := manager.StartVerifyComponent(r.Context(), in.ID, in.Version)
		respond(w, 202, v, err)
	})
	mux.HandleFunc("GET /api/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := manager.Operation(r.PathValue("id"))
		respond(w, 200, v, err)
	})
	mux.HandleFunc("POST /api/operations/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		var in struct{}
		if !decodeBody(w, r, &in) {
			return
		}
		v, err := manager.Cancel(r.PathValue("id"))
		respond(w, 200, v, err)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; object-src 'none'; frame-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if r.Host != host {
			writeAPIError(w, failure("host_denied", "拒绝非规范本机入口", 403))
			return
		}
		if origin := r.Header.Get("Origin"); len(r.Header.Values("Origin")) > 1 || origin != "" && origin != "http://"+host {
			writeAPIError(w, failure("origin_denied", "拒绝跨站请求", 403))
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); len(r.Header.Values("Sec-Fetch-Site")) > 1 || site != "" && site != "same-origin" && site != "none" {
			writeAPIError(w, failure("origin_denied", "拒绝跨站请求", 403))
			return
		}
		if strings.Contains(r.URL.EscapedPath(), "%") || strings.Contains(r.URL.Path, "\\") {
			writeAPIError(w, failure("path_invalid", "资源路径无效", 404))
			return
		}
		manager.mu.Lock()
		closed := manager.closed
		manager.mu.Unlock()
		if closed {
			writeAPIError(w, failure("service_closed", "服务已关闭", 503))
			return
		}
		if r.URL.Path == integration.BindingsPath || strings.HasPrefix(r.URL.Path, integration.BindingsPath+"/") {
			serveBindingHTTP(manager, w, r)
			return
		}
		if r.URL.Path == integration.RuntimeAssetsPath || r.URL.Path == integration.RuntimeJobsPath || strings.HasPrefix(r.URL.Path, integration.RuntimeJobsPath+"/") {
			if manager.runtimeHandler == nil {
				writeAPIError(w, failure("center_runtime_unavailable", "运行宿主尚未配置", 503))
				return
			}
			manager.runtimeHandler.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/integration/") {
			if integrationHost == nil {
				writeAPIError(w, failure("integration_unavailable", "只读集成未启用", 503))
				return
			}
			integrationHost.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Method == http.MethodHead {
				w.Header().Set("Allow", "GET")
				writeAPIError(w, failure("method_not_allowed", "接口不支持该方法", 405))
				return
			}
			mux.ServeHTTP(w, r)
			return
		}
		writeAPIError(w, failure("resource_unknown", "资源不存在", 404))
	})
}
func decodeBody(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeAPIError(w, failure("request_too_large", "请求内容超过8192字节", 413))
		} else {
			writeAPIError(w, failure("request_invalid", "无法读取请求", 400))
		}
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") {
		writeAPIError(w, failure("request_invalid", "请求必须为JSON对象", 400))
		return false
	}
	if components.StrictJSON(raw, out) != nil {
		writeAPIError(w, failure("request_invalid", "请求字段、类型、重复键或JSON格式无效", 400))
		return false
	}
	return true
}
func respond(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, status, v)
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeAPIError(w http.ResponseWriter, err error) {
	var e *apiError
	if errors.Is(err, bindings.ErrConflict) {
		e = &apiError{Code: "center_binding_stale", Message: "绑定版本已变化，请刷新后重试", Status: 409}
	} else if errors.Is(err, bindings.ErrReadOnly) || errors.Is(err, bindings.ErrCorrupt) {
		e = &apiError{Code: "center_binding_store_unavailable", Message: "绑定存储不可写，请检查存储诊断", Status: 503}
	} else if errors.Is(err, bindings.ErrInvalid) {
		e = &apiError{Code: "center_binding_invalid", Message: "绑定字段或模型组成无效", Status: 422}
	} else if errors.Is(err, registry.ErrConflict) {
		e = &apiError{Code: "registry_conflict", Message: "登记版本已变化，请刷新状态", Status: 409}
	} else if errors.Is(err, registry.ErrReadOnly) {
		e = &apiError{Code: "registry_read_only", Message: "登记存储只读，请检查存储诊断", Status: 503}
	} else if !errors.As(err, &e) {
		e = &apiError{Code: "operation_failed", Message: "操作未完成，请检查状态后重试", Status: 422}
		message := err.Error()
		for _, code := range []string{"registry_conflict", "registry_corrupt", "registry_commit_uncertain", "registry_write_failed"} {
			if strings.Contains(message, code) {
				e.Code = code
				e.Message = "登记数据不可写或版本已变化，请刷新状态"
				e.Status = 503
				if code == "registry_conflict" {
					e.Status = 409
				}
				break
			}
		}
	}
	writeJSON(w, e.Status, map[string]any{"error": e})
}
