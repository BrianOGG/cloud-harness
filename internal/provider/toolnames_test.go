package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestOpenAIEncodesDottedToolNamesOnTheWireAndRestoresThem(t *testing.T) {
	for _, mode := range []string{"json", "sse"} {
		t.Run(mode, func(t *testing.T) {
			var body struct {
				Tools []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tools"`
				Messages []struct {
					ToolCalls []struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"messages"`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Error(err)
				}
				if mode == "json" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"c1","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"mcp_files_echo","arguments":"{\"msg\":\"hi\"}"}},{"id":"call_2","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]}}]}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, `data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"mcp_files_echo","arguments":"{\"msg\":\"hi\"}"}}]}}]}`+"\n\n"+
					`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]}}]}`+"\n\n"+
					`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
			}))
			defer srv.Close()
			p := &OpenAICompatible{BaseURL: srv.URL, APIKey: "k", Model: "m", Client: srv.Client()}
			resp, err := p.Complete(context.Background(), Request{
				Messages: []Message{
					{Role: "user", Content: "go"},
					{Role: "assistant", ToolCalls: []domain.ToolCall{{ProviderCallID: "call_0", Name: "mcp.files.echo", Arguments: json.RawMessage(`{}`)}}},
					{Role: "tool", ToolCallID: "call_0", Content: "{}"},
				},
				Tools: []ToolDefinition{{Name: "mcp.files.echo"}, {Name: "read_file"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(body.Tools) != 2 || body.Tools[0].Function.Name != "mcp_files_echo" || body.Tools[1].Function.Name != "read_file" {
				t.Fatalf("wire tools = %+v", body.Tools)
			}
			if len(body.Messages) != 3 || len(body.Messages[1].ToolCalls) != 1 || body.Messages[1].ToolCalls[0].Function.Name != "mcp_files_echo" {
				t.Fatalf("wire history = %+v", body.Messages)
			}
			var got []string
			for _, item := range resp.Items {
				if item.ToolCall != nil {
					got = append(got, item.ToolCall.Name)
				}
			}
			if strings.Join(got, ",") != "mcp.files.echo,read_file" {
				t.Fatalf("restored tool calls = %v", got)
			}
		})
	}
}

func TestWireToolNamesAreValidUniqueAndStable(t *testing.T) {
	long := "mcp.server." + strings.Repeat("x", 80)
	defs := []ToolDefinition{
		{Name: "mcp.a.b"}, {Name: "mcp_a_b"}, {Name: "mcp.a_b"}, {Name: "read_file"}, {Name: long},
	}
	names := newWireToolNames(defs)
	seen := map[string]string{}
	for _, def := range defs {
		wire := names.wire(def.Name)
		if !wireToolNameRE.MatchString(wire) {
			t.Fatalf("%q encoded to invalid wire name %q", def.Name, wire)
		}
		if prev, dup := seen[wire]; dup {
			t.Fatalf("%q and %q share wire name %q", prev, def.Name, wire)
		}
		seen[wire] = def.Name
		resp, _ := names.restore(domain.ModelResponse{Items: []domain.ResponseItem{{Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{Name: wire}}}}, nil)
		if resp.Items[0].ToolCall.Name != def.Name {
			t.Fatalf("wire %q restored to %q, want %q", wire, resp.Items[0].ToolCall.Name, def.Name)
		}
		if again := newWireToolNames(defs).wire(def.Name); again != wire {
			t.Fatalf("encoding of %q is not stable: %q then %q", def.Name, wire, again)
		}
	}
	if names.wire("read_file") != "read_file" || names.wire("mcp_a_b") != "mcp_a_b" {
		t.Fatal("valid names must be sent unchanged")
	}
	if got := names.wire("mcp.gone.tool"); got != "mcp_gone_tool" {
		t.Fatalf("stale history name encoded to %q", got)
	}
}
