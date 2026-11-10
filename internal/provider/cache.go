package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

// PrefixCache records prompt-cache fields from the stable prefix.
// ReportHits is the measured arm. The baseline leaves it false and always
// reports zero cached tokens, even when the prefix is unchanged.
type PrefixCache struct {
	Inner      Provider
	ReportHits bool

	mu   sync.Mutex
	prev string
}

func (p *PrefixCache) Name() string {
	if p.Inner != nil {
		return p.Inner.Name()
	}
	return "prefix-cache"
}

func (p *PrefixCache) Complete(ctx context.Context, req Request) (domain.ModelResponse, error) {
	resp, err := p.Inner.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	in := estimateTokens(req.Messages)
	cached := 0
	sum := hashPrefix(req.Prefix)
	p.mu.Lock()
	if p.ReportHits && p.prev != "" && p.prev == sum && sum != "" {
		cached = estimateTokens(req.Prefix)
	}
	p.prev = sum
	p.mu.Unlock()
	return attachUsage(resp, domain.Usage{
		InputTokens: in, OutputTokens: 1, CachedInputTokens: cached, LatencyMS: 1,
	}), nil
}

func estimateTokens(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += (len(m.Content)+3)/4 + 4
	}
	if n < 1 {
		return 1
	}
	return n
}

func hashPrefix(msgs []Message) string {
	raw, err := json.Marshal(msgs)
	if err != nil || len(msgs) == 0 {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func attachUsage(resp domain.ModelResponse, u domain.Usage) domain.ModelResponse {
	items := domain.CloneItems(resp.Items)
	found := false
	for i := range items {
		if items[i].Kind == domain.ItemUsage && items[i].Usage != nil {
			items[i].Usage.InputTokens += u.InputTokens
			items[i].Usage.OutputTokens += u.OutputTokens
			items[i].Usage.CachedInputTokens += u.CachedInputTokens
			if items[i].Usage.LatencyMS == 0 {
				items[i].Usage.LatencyMS = u.LatencyMS
			}
			found = true
		}
	}
	if !found {
		items = append(items, domain.ResponseItem{Ordinal: len(items), Kind: domain.ItemUsage, Usage: &u})
	}
	resp.Items = items
	return resp
}
