package provider

import (
	"encoding/json"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// ParseGeminiUsage parses usageMetadata without introducing a Gemini transport.
func ParseGeminiUsage(raw []byte) (domain.Usage, error) {
	var envelope struct {
		UsageMetadata *struct {
			PromptTokenCount        int  `json:"promptTokenCount"`
			CandidatesTokenCount    int  `json:"candidatesTokenCount"`
			CachedContentTokenCount *int `json:"cachedContentTokenCount"`
			ThoughtsTokenCount      *int `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return domain.Usage{}, domain.E(domain.CodeProtocol, "gemini usage metadata was not JSON")
	}
	if envelope.UsageMetadata == nil {
		return domain.Usage{}, nil
	}
	metadata := envelope.UsageMetadata
	usage := domain.Usage{
		InputTokens:       metadata.PromptTokenCount,
		OutputTokens:      metadata.CandidatesTokenCount,
		CacheReported:     metadata.CachedContentTokenCount != nil,
		ReasoningReported: metadata.ThoughtsTokenCount != nil,
	}
	if metadata.CachedContentTokenCount != nil {
		usage.CachedInputTokens = *metadata.CachedContentTokenCount
	}
	if metadata.ThoughtsTokenCount != nil {
		usage.ReasoningTokens = *metadata.ThoughtsTokenCount
		usage.OutputTokens += *metadata.ThoughtsTokenCount
	}
	return usage, nil
}
