package provider

import (
	"strings"
	"sync"

	"github.com/BrianOGG/cloud-harness/internal/modelconfig"
)

type AnthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
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

// Agnes documents this field at /en/docs/agnes-30-flash (Thinking mode).
type ChatTemplateKwargs struct {
	EnableThinking bool `json:"enable_thinking"`
}

type ReasoningParameters struct {
	Effort             string
	Thinking           *AnthropicThinking
	ExtraBody          *GeminiExtraBody
	ChatTemplateKwargs *ChatTemplateKwargs
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
	for _, parameter := range []string{"reasoning_effort", "chat_template_kwargs", "enable_thinking", "thinking", "thinking_config", "unsupported parameter"} {
		if strings.Contains(text, parameter) {
			return true
		}
	}
	return false
}

// ModelReasoningSupport shares the runtime cache with the built-in model menu.
func ModelReasoningSupport(p Provider, model string) string {
	return ModelMetadata(p, model).Thinking
}

// MapModelReasoning uses one catalog record, never the union of third-party
// capabilities. Unsupported legacy preferences preserve the service default.
func MapModelReasoning(protocol, effort string, m modelconfig.Model) ReasoningParameters {
	if m.Thinking == "unsupported" {
		return ReasoningParameters{}
	}
	if m.ReasoningType == "toggle" {
		if protocol == "anthropic" {
			if effort == "off" {
				return ReasoningParameters{Thinking: &AnthropicThinking{Type: "disabled"}}
			}
			return MapReasoningEffort(protocol, "low")
		}
		if protocol == "gemini" {
			if effort == "off" {
				return MapReasoningEffort(protocol, "off")
			}
			return ReasoningParameters{}
		}
		// OpenAI-compatible Agnes uses a boolean, not a synthetic effort level.
		return ReasoningParameters{ChatTemplateKwargs: &ChatTemplateKwargs{EnableThinking: effort != "off"}}
	}
	if effort == "" || effort == "default" {
		return ReasoningParameters{}
	}
	if m.ReasoningType == "effort" {
		value := effort
		values := m.ReasoningValues
		contains := func(v string) bool {
			for _, item := range values {
				if item == v {
					return true
				}
			}
			return false
		}
		if effort == "off" {
			if contains("none") {
				value = "none"
			} else {
				value = "off"
			}
		}
		if effort == "low" && !contains("low") {
			value = "minimal"
		}
		if !contains(value) {
			return ReasoningParameters{}
		}
		if protocol == "" || protocol == "openai" {
			return ReasoningParameters{Effort: value}
		}
	}
	return MapReasoningEffort(protocol, effort)
}

// SetModelCatalog shares a cache between the settings boundary and inference.
// Inference only reads it; it never makes a models.dev request.
func SetModelCatalog(p Provider, catalog *ModelsDev) {
	switch p := p.(type) {
	case *OpenAICompatible:
		p.ModelsDev = catalog
	case *Anthropic:
		p.ModelsDev = catalog
	}
}
func ModelCatalog(p Provider) *ModelsDev {
	switch p := p.(type) {
	case *OpenAICompatible:
		return p.ModelsDev
	case *Anthropic:
		return p.ModelsDev
	}
	return nil
}
func reasoningMetadata(c *ModelsDev, base, protocol, identity, model string, metadata map[string]modelconfig.Model, thinking map[string]string) modelconfig.Model {
	m, ok := metadata[model]
	if !ok {
		m = modelconfig.Model{ID: model, Thinking: thinking[model]}
	}
	m = c.Enrich(base, protocol, []modelconfig.Model{m})[0]
	m.Thinking = ReasoningSupport(protocol, identity, model, m.Thinking)
	return m
}
func ModelMetadata(p Provider, model string) modelconfig.Model {
	switch p := p.(type) {
	case *OpenAICompatible:
		if model == "" {
			model = p.Model
		}
		protocol := p.Protocol
		if protocol == "" {
			protocol = "openai"
		}
		identity := p.CapabilityID
		if identity == "" {
			identity = p.BaseURL
		}
		return reasoningMetadata(p.ModelsDev, p.BaseURL, protocol, identity, model, p.ModelMetadata, p.Thinking)
	case *Anthropic:
		if model == "" {
			model = p.Model
		}
		identity := p.CapabilityID
		if identity == "" {
			identity = p.BaseURL
		}
		return reasoningMetadata(p.ModelsDev, p.BaseURL, "anthropic", identity, model, p.ModelMetadata, p.Thinking)
	}
	return modelconfig.Model{ID: model, Thinking: modelconfig.ThinkingSupport("", model)}
}
