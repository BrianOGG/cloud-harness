package provider

import (
	"strings"
	"sync"

	"github.com/BrianOGG/cloud-harness/internal/modelconfig"
)

type AnthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}
type GeminiThinking struct {
	ThinkingBudget  int  `json:"thinking_budget"`
	IncludeThoughts bool `json:"include_thoughts"`
}
type GeminiGoogle struct {
	ThinkingConfig GeminiThinking `json:"thinking_config"`
}
type GeminiExtraBody struct {
	Google GeminiGoogle `json:"google"`
}
type ReasoningParameters struct {
	Effort    string
	Thinking  *AnthropicThinking
	ExtraBody *GeminiExtraBody
}

func ValidReasoningEffort(effort string) bool {
	switch effort {
	case "", "default", "off", "low", "medium", "high":
		return true
	}
	return false
}

// MapReasoningEffort leaves service defaults untouched unless explicitly set.
func MapReasoningEffort(protocol, effort string) ReasoningParameters {
	if effort == "" || effort == "default" {
		return ReasoningParameters{}
	}
	switch protocol {
	case "anthropic":
		budget := map[string]int{"low": 4000, "medium": 10000, "high": 24000}[effort]
		if budget > 0 {
			return ReasoningParameters{Thinking: &AnthropicThinking{Type: "enabled", BudgetTokens: budget}}
		}
	case "gemini":
		budget, ok := map[string]int{"off": 0, "low": 1024, "medium": 8192, "high": 24576}[effort]
		if ok {
			return ReasoningParameters{ExtraBody: &GeminiExtraBody{Google: GeminiGoogle{ThinkingConfig: GeminiThinking{ThinkingBudget: budget, IncludeThoughts: false}}}}
		}
	default:
		if effort == "low" || effort == "medium" || effort == "high" {
			return ReasoningParameters{Effort: effort}
		}
	}
	return ReasoningParameters{}
}

const ReasoningFallbackEventType = "provider.reasoning_unsupported"

type ReasoningFallbackEvent struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

var unsupportedReasoning sync.Map

type reasoningKey struct{ protocol, provider, model string }

func ReasoningSupport(protocol, identity, model, declared string) string {
	if _, ok := unsupportedReasoning.Load(reasoningKey{protocol, identity, model}); ok {
		return "unsupported"
	}
	if modelconfig.ThinkingSupport(protocol, model) == "unsupported" {
		return "unsupported"
	}
	if declared == "supported" || declared == "unsupported" {
		return declared
	}
	return modelconfig.ThinkingSupport(protocol, model)
}
func rememberReasoningUnsupported(protocol, identity, model string, req Request) {
	unsupportedReasoning.Store(reasoningKey{protocol, identity, model}, true)
	if req.OnReasoningFallback != nil {
		req.OnReasoningFallback(ReasoningFallbackEvent{Provider: protocol, Model: model})
	}
}
func reasoningRejected(status int, message string) bool {
	if status != 400 {
		return false
	}
	text := strings.ToLower(message)
	for _, parameter := range []string{"reasoning_effort", "thinking", "thinking_config", "unsupported parameter"} {
		if strings.Contains(text, parameter) {
			return true
		}
	}
	return false
}

// ModelReasoningSupport shares the runtime cache with the built-in model menu.
func ModelReasoningSupport(p Provider, model string) string {
	switch p := p.(type) {
	case *OpenAICompatible:
		protocol := p.Protocol
		if protocol == "" {
			protocol = "openai"
		}
		identity := p.CapabilityID
		if identity == "" {
			identity = p.BaseURL
		}
		if model == "" {
			model = p.Model
		}
		return ReasoningSupport(protocol, identity, model, p.Thinking[model])
	case *Anthropic:
		identity := p.CapabilityID
		if identity == "" {
			identity = p.BaseURL
		}
		if model == "" {
			model = p.Model
		}
		return ReasoningSupport("anthropic", identity, model, p.Thinking[model])
	default:
		return modelconfig.ThinkingSupport("", model)
	}
}
