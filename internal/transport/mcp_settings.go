package transport

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/config"
)

const mcpCallbackPath = config.MCPOAuthCallbackPath
const mcpCookie = "cloud_harness_mcp_state"

func (s *Server) mcpRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/mcp/servers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		status := s.MCP.Status()
		for i := range status {
			// Stdio initialization errors can include arbitrary subprocess output.
			if status[i].Transport == "stdio" && status[i].Error != "" {
				status[i].Error = "请检查本地命令、参数和启动超时。"
			}
		}
		writeJSON(w, 200, status)
	})
	mux.HandleFunc("POST /v1/mcp/servers/{name}/connect", func(w http.ResponseWriter, r *http.Request) {
		if err := s.MCP.Connect(r.Context(), r.PathValue("name")); err != nil {
			writeJSON(w, 400, map[string]string{"error": "连接失败，请查看服务诊断。"})
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /v1/mcp/servers/{name}/disconnect", func(w http.ResponseWriter, r *http.Request) {
		if err := s.MCP.Disconnect(r.PathValue("name")); err != nil {
			writeJSON(w, 400, map[string]string{"error": "无法断开服务。"})
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /v1/mcp/servers/{name}/authorize", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		location, state, secure, err := s.MCP.BeginOAuth(r.Context(), r.PathValue("name"), r.Host)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: mcpCookie, Value: state, Path: mcpCallbackPath, HttpOnly: true, Secure: secure || r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: 300})
		writeJSON(w, 200, map[string]string{"url": location})
	})
}

func (s *Server) mcpCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	cookie, err := r.Cookie(mcpCookie)
	state := r.URL.Query().Get("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(cookie.Value)) != 1 {
		http.Error(w, "授权状态不匹配，请从设置重新授权。", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: mcpCookie, Path: mcpCallbackPath, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	code := r.URL.Query().Get("code")
	if r.URL.Query().Get("error") != "" {
		code = ""
	}
	if err := s.MCP.FinishOAuth(r.Context(), state, code, r.URL.Query().Get("iss"), r.Host); err != nil {
		http.Error(w, "授权或连接失败，请返回设置检查服务状态后重试。", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = strings.NewReader(`<!doctype html><html lang="zh-CN"><meta charset="utf-8"><title>MCP 授权完成</title><p>授权完成，服务已连接。</p><a href="/ui/">返回会话</a></html>`).WriteTo(w)
}
