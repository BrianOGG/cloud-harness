package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/modelconfig"
)

// FetchModelCatalog has no persistent side effects. Each concurrent request is bounded
// to ten seconds and one megabyte, and redirects are never followed.
func FetchModelCatalog(ctx context.Context, base, key, protocol string) (modelconfig.Detection, error) {
	base, err := modelconfig.NormalizeBaseURL(base)
	if err != nil {
		return modelconfig.Detection{}, err
	}
	if protocol != "" && !modelconfig.ValidProtocol(protocol) {
		return modelconfig.Detection{}, errors.New("无效的模型服务协议")
	}
	u, _ := url.Parse(base)
	if protocol == "" {
		switch strings.ToLower(u.Hostname()) {
		case "generativelanguage.googleapis.com":
			protocol = "gemini"
		case "api.anthropic.com":
			protocol = "anthropic"
		}
	}
	protocols := []string{protocol}
	if protocol == "" {
		protocols = []string{"openai", "anthropic", "gemini"}
	}
	type result struct {
		detection modelconfig.Detection
		status    int
		err       error
	}
	results := make([]result, len(protocols))
	var wg sync.WaitGroup
	for i, candidate := range protocols {
		wg.Add(1)
		go func() {
			defer wg.Done()
			endpoint := base + "/models"
			if candidate == "anthropic" && !strings.HasSuffix(base, "/v1") {
				endpoint = base + "/v1/models"
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if err != nil {
				results[i].err = err
				return
			}
			switch candidate {
			case "openai":
				if key != "" {
					req.Header.Set("Authorization", "Bearer "+key)
				}
			case "anthropic":
				req.Header.Set("x-api-key", key)
				req.Header.Set("anthropic-version", "2023-06-01")
			case "gemini":
				req.Header.Set("x-goog-api-key", key)
			}
			req.Header.Set("Accept", "application/json")
			client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			res, err := client.Do(req)
			if err != nil {
				results[i].err = errors.New("模型列表请求失败，请检查服务地址、网络与凭据")
				return
			}
			defer func() { _ = res.Body.Close() }()
			results[i].status = res.StatusCode
			if res.StatusCode != http.StatusOK {
				results[i].err = errors.New("模型列表请求失败，请检查服务地址与凭据")
				return
			}
			raw, err := io.ReadAll(io.LimitReader(res.Body, modelconfig.MaxCatalogBytes+1))
			if err != nil || len(raw) > modelconfig.MaxCatalogBytes || key != "" && strings.Contains(string(raw), key) {
				results[i].err = errors.New("服务未返回有效的模型列表（最大 1MB）")
				return
			}
			selected := candidate
			var models []modelconfig.Model
			if protocol == "" {
				selected, models, err = modelconfig.DetectProtocol(raw)
			} else {
				switch candidate {
				case "openai":
					models, err = modelconfig.ParseOpenAIModels(raw)
				case "anthropic":
					models, err = modelconfig.ParseAnthropicModels(raw)
				case "gemini":
					models, err = modelconfig.ParseGeminiModels(raw)
				}
			}
			if err == nil && key != "" {
				for _, model := range models {
					if strings.Contains(model.ID, key) || strings.Contains(model.DisplayName, key) {
						err = errors.New("服务未返回有效的模型列表")
						break
					}
				}
			}
			results[i].detection = modelconfig.Detection{Protocol: selected, Models: models, FetchedAt: time.Now().UTC()}
			results[i].err = err
		}()
	}
	wg.Wait()
	rank := map[string]int{"openai": 1, "anthropic": 2, "gemini": 3}
	var best modelconfig.Detection
	status := 0
	for _, result := range results {
		if result.err == nil && rank[result.detection.Protocol] > rank[best.Protocol] {
			best = result.detection
		}
		if status == 0 || result.status == 401 {
			status = result.status
		}
	}
	if best.Protocol != "" {
		return best, nil
	}
	return modelconfig.Detection{}, &modelconfig.CatalogError{Message: "模型列表获取失败；请检查地址、凭据和协议，也可手动填写模型 ID", UpstreamStatus: status}
}
