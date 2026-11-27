package main

import (
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/modelconfig"
	"github.com/BrianOGG/cloud-harness/internal/provider"
)

func TestCustomModelProviderProtocolAndBaseNormalization(t *testing.T) {
	for _, tc := range []struct{ protocol, base, want string }{
		{"openai", "https://example.invalid/v1", "https://example.invalid/v1"},
		{"anthropic", "https://api.anthropic.com/v1", "https://api.anthropic.com"},
		{"anthropic", "https://example.invalid/proxy", "https://example.invalid/proxy"},
		{"gemini", "https://generativelanguage.googleapis.com/v1beta", "https://generativelanguage.googleapis.com/v1beta/openai"},
	} {
		t.Run(tc.protocol+tc.base, func(t *testing.T) {
			p := customModelProvider(config.CustomProvider{ID: "p", BaseURL: tc.base, Protocol: tc.protocol, Model: "one", APIKey: "constructor-fixture", Catalog: []modelconfig.Model{{ID: "one", Thinking: "unsupported"}}})
			switch p := p.(type) {
			case *provider.Anthropic:
				if tc.protocol != "anthropic" || p.BaseURL != tc.want || p.Thinking["one"] != "unsupported" {
					t.Fatal("incorrect Anthropic routing")
				}
			case *provider.OpenAICompatible:
				if tc.protocol == "anthropic" || p.BaseURL != tc.want || p.Protocol != tc.protocol || p.Thinking["one"] != "unsupported" {
					t.Fatal("incorrect compatible routing")
				}
			default:
				t.Fatal("unexpected provider")
			}
		})
	}
}
