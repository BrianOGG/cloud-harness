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
		protocol = modelconfig.OfficialProtocol(u.Hostname())
	}
	auto := protocol == ""
	protocols := []string{protocol}
	if auto {
		protocols = []string{"openai", "anthropic", "gemini"}
	}
	type result struct {
		detection modelconfig.Detection
		raw       []byte
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
			var models []modelconfig.Model
			ok := !auto
			switch candidate {
			case "openai":
				ok = ok || modelconfig.LooksLikeOpenAI(raw)
				models, err = modelconfig.ParseOpenAIModels(raw)
			case "anthropic":
				ok = ok || modelconfig.LooksLikeAnthropic(raw)
				models, err = modelconfig.ParseAnthropicModels(raw)
			case "gemini":
				ok = ok || modelconfig.LooksLikeGemini(raw)
				models, err = modelconfig.ParseGeminiModels(raw)
			}
			if err == nil && !ok {
				err = errors.New("服务未返回有效的模型列表")
			}
			if err == nil && key != "" {
				for _, model := range models {
					if strings.Contains(model.ID, key) || strings.Contains(model.DisplayName, key) {
						err = errors.New("服务未返回有效的模型列表")
						break
					}
				}
			}
			results[i].raw = raw
			results[i].detection = modelconfig.Detection{Protocol: candidate, Models: models, FetchedAt: time.Now().UTC()}
			results[i].err = err
		}()
	}
	wg.Wait()
	successful := map[string][]modelconfig.Model{}
	var openAIRaw []byte
	var fetchedAt time.Time
	status := 0
	for i, result := range results {
		if result.err == nil {
			successful[result.detection.Protocol] = result.detection.Models
			if fetchedAt.IsZero() || result.detection.FetchedAt.After(fetchedAt) {
				fetchedAt = result.detection.FetchedAt
			}
		}
		if protocols[i] == "openai" {
			openAIRaw = result.raw
		}
		if status == 0 || result.status == http.StatusUnauthorized {
			status = result.status
		}
	}
	if auto {
		protocol, alternatives, models := modelconfig.SelectAutoProtocol(successful, openAIRaw)
		if protocol != "" {
			if fetchedAt.IsZero() {
				fetchedAt = time.Now().UTC()
			}
			return modelconfig.Detection{Protocol: protocol, Models: models, FetchedAt: fetchedAt, Alternatives: alternatives}, nil
		}
	} else if models, ok := successful[protocol]; ok {
		if fetchedAt.IsZero() {
			fetchedAt = time.Now().UTC()
		}
		return modelconfig.Detection{Protocol: protocol, Models: models, FetchedAt: fetchedAt}, nil
	}
	return modelconfig.Detection{}, &modelconfig.CatalogError{Message: "模型列表获取失败；请检查地址、凭据和协议，也可手动填写模型 ID", UpstreamStatus: status}
}
