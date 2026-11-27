// Package modelconfig discovers read-only catalogs without running inference.
package modelconfig

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
)

const MaxCatalogBytes = 1 << 20
const MaxCatalogModels = 1000

type Model struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
}
type Detection struct {
	Protocol  string    `json:"protocol"`
	Models    []Model   `json:"models"`
	FetchedAt time.Time `json:"fetched_at"`
}
type CatalogError struct {
	Message        string
	UpstreamStatus int
}

func (e *CatalogError) Error() string { return e.Message }
func ValidProtocol(p string) bool     { return p == "openai" || p == "anthropic" || p == "gemini" }

// NormalizeBaseURL is shared by save and discovery. No credentials in URLs.
func NormalizeBaseURL(base string) (string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	u, err := url.Parse(base)
	if err != nil {
		return "", errors.New("服务地址必须使用 HTTPS，仅本机回环地址允许 HTTP")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback()
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(base, "#") || !(u.Scheme == "https" || u.Scheme == "http" && loopback) {
		return "", errors.New("服务地址必须使用 HTTPS，且不能包含用户信息、查询参数或片段；仅本机回环地址允许 HTTP")
	}
	return base, nil
}

// ThinkingSupport is conservative: unknown models may try the parameter.
func ThinkingSupport(protocol, model string) string {
	m := strings.ToLower(strings.TrimPrefix(model, "models/"))
	for _, prefix := range []string{"gpt-3.5", "gpt-4", "chatgpt-4o", "claude-2", "claude-instant", "claude-3-haiku", "claude-3-sonnet", "claude-3-opus", "claude-3-5", "gemini-1.", "gemini-2.0"} {
		if strings.HasPrefix(m, prefix) {
			return "unsupported"
		}
	}
	for _, prefix := range []string{"o1", "o3", "o4", "gpt-5", "gpt-6", "claude-3-7", "claude-sonnet-4", "claude-opus-4", "claude-haiku-4", "gemini-2.5", "gemini-3"} {
		if strings.HasPrefix(m, prefix) {
			return "supported"
		}
	}
	return "unknown"
}
func parseObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(raw) > MaxCatalogBytes {
		return nil, errors.New("模型列表响应超过 1MB")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, errors.New("服务未返回有效的模型列表")
	}
	return obj, nil
}
func parseData(raw []byte, protocol string) ([]Model, error) {
	obj, err := parseObject(raw)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID          string
		DisplayName string `json:"display_name"`
		Object      string
		Type        string
		Thinking    string
	}
	if data, ok := obj["data"]; !ok || string(data) == "null" || json.Unmarshal(data, &rows) != nil {
		return nil, errors.New("服务未返回有效的模型列表")
	}
	out := make([]Model, 0, min(len(rows), MaxCatalogModels))
	seen := map[string]bool{}
	for _, row := range rows {
		if row.ID == "" || len(row.ID) > 200 || seen[row.ID] {
			continue
		}
		seen[row.ID] = true
		thinking := ThinkingSupport(protocol, row.ID)
		if row.Thinking == "supported" || row.Thinking == "unsupported" || row.Thinking == "unknown" {
			thinking = row.Thinking
		}
		out = append(out, Model{ID: row.ID, DisplayName: row.DisplayName, Thinking: thinking})
		if len(out) == MaxCatalogModels {
			break
		}
	}
	return out, nil
}
func ParseOpenAIModels(raw []byte) ([]Model, error)    { return parseData(raw, "openai") }
func ParseAnthropicModels(raw []byte) ([]Model, error) { return parseData(raw, "anthropic") }
func ParseGeminiModels(raw []byte) ([]Model, error) {
	obj, err := parseObject(raw)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Name        string
		DisplayName string
		Methods     []string `json:"supportedGenerationMethods"`
	}
	if data, ok := obj["models"]; !ok || string(data) == "null" || json.Unmarshal(data, &rows) != nil {
		return nil, errors.New("服务未返回有效的 Gemini 模型列表")
	}
	out := make([]Model, 0, min(len(rows), MaxCatalogModels))
	seen := map[string]bool{}
	for _, row := range rows {
		if !strings.HasPrefix(row.Name, "models/") {
			continue
		}
		generates := false
		for _, method := range row.Methods {
			if method == "generateContent" {
				generates = true
			}
		}
		id := strings.TrimPrefix(row.Name, "models/")
		if !generates || id == "" || len(id) > 200 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, Model{ID: id, DisplayName: row.DisplayName, Thinking: ThinkingSupport("gemini", id)})
		if len(out) == MaxCatalogModels {
			break
		}
	}
	return out, nil
}

// DetectProtocol ranks protocol-specific shapes above generic OpenAI data arrays.
func DetectProtocol(raw []byte) (string, []Model, error) {
	obj, err := parseObject(raw)
	if err != nil {
		return "", nil, err
	}
	if _, ok := obj["models"]; ok {
		var rows []struct{ Name string }
		if json.Unmarshal(obj["models"], &rows) == nil && string(obj["models"]) != "null" {
			valid := len(rows) == 0
			for _, row := range rows {
				if strings.HasPrefix(row.Name, "models/") {
					valid = true
				}
			}
			if valid {
				if models, err := ParseGeminiModels(raw); err == nil {
					return "gemini", models, nil
				}
			}
		}
	}
	var rows []map[string]json.RawMessage
	if json.Unmarshal(obj["data"], &rows) != nil || string(obj["data"]) == "null" || obj["data"] == nil {
		return "", nil, errors.New("无法识别模型列表协议")
	}
	_, anthropic := obj["has_more"]
	for _, row := range rows {
		if string(row["type"]) == `"model"` || row["display_name"] != nil {
			anthropic = true
		}
	}
	if anthropic {
		models, err := ParseAnthropicModels(raw)
		return "anthropic", models, err
	}
	if len(rows) > 0 {
		valid := false
		for _, row := range rows {
			if string(row["object"]) == `"model"` || row["id"] != nil {
				valid = true
			}
		}
		if !valid {
			return "", nil, errors.New("无法识别模型列表协议")
		}
	}
	models, err := ParseOpenAIModels(raw)
	return "openai", models, err
}
