package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/notify"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

const approvalTestToken = "simulated-approval-http-token"

func TestApprovalPageRedirectsIntoConversation(t *testing.T) {
	h := (&Server{Token: approvalTestToken}).Handler()
	for _, id := range []string{"", "run_fixture", "unknown run <not HTML>&session=other"} {
		target := "/ui"
		path := "/approval"
		if id != "" {
			path += "?run=" + url.QueryEscape(id)
			target += "?run=" + url.QueryEscape(id)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != target || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("legacy link did not redirect safely: status=%d location=%q", rec.Code, rec.Header().Get("Location"))
		}
		if strings.Contains(rec.Body.String(), approvalTestToken) {
			t.Fatal("redirect leaked bearer token")
		}
	}
}

func TestApprovalPageAuthExemptionIsExact(t *testing.T) {
	h := (&Server{Token: approvalTestToken}).Handler()
	const runPath = "/v1/runs/run_fixture"
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/approval/"},
		{http.MethodGet, "/approval/child"},
		{http.MethodGet, "/approval-preview"},
		{http.MethodGet, "/v1/approval"},
		{http.MethodGet, "/v1/approvals"},
		{http.MethodGet, runPath},
		{http.MethodGet, runPath + "/events"},
		{http.MethodPost, runPath + "/responses"},
		{http.MethodPost, runPath + "/answer"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != `Bearer realm="cloud-harness"` {
			t.Errorf("%s %s: expected 401 and Bearer challenge, got %d %v", tc.method, tc.path, rec.Code, rec.Header())
		}
	}
}

func TestApprovalPageNotificationToResume(t *testing.T) {
	for _, tc := range []struct {
		name, channel, event, tool, argument, summary string
		reason                                        domain.WaitReason
		queueReason, endpoint, payload                string
		invocationStatus                              domain.InvStatus
		resultText                                    string
	}{
		{
			name: "approve", channel: "bark", event: notify.EventWaitingApproval,
			tool: "run_command", argument: "command", summary: "echo approved-for-m10-3-3",
			reason: domain.WaitApproval, queueReason: "approval", endpoint: "/responses?wait=1", payload: `{"decision":"approve"}`,
			invocationStatus: domain.InvSucceeded, resultText: "approved-for-m10-3-3",
		},
		{
			name: "deny", channel: "ntfy", event: notify.EventWaitingApproval,
			tool: "run_command", argument: "command", summary: "echo denied-for-m10-3-3",
			reason: domain.WaitApproval, queueReason: "approval", endpoint: "/responses?wait=1", payload: `{"decision":"deny"}`,
			invocationStatus: domain.InvDenied,
		},
		{
			name: "answer", channel: "ntfy", event: notify.EventAskUser,
			tool: domain.ToolAskUser, argument: "question", summary: "Which region should I use?",
			reason: domain.WaitUser, queueReason: "ask_user", endpoint: "/answer?wait=1", payload: `{"answer":"eu-central-1"}`,
			invocationStatus: domain.InvSucceeded, resultText: `"answer":"eu-central-1"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newServer(t)
			m.Workspace = t.TempDir()
			m.DisableSnapshots = true
			m.Broker = tools.NewBroker(m.Store, m.Store)
			if err := tools.RegisterLocal(m.Broker, m.Workspace); err != nil {
				t.Fatal(err)
			}
			args, err := json.Marshal(map[string]string{tc.argument: tc.summary})
			if err != nil {
				t.Fatal(err)
			}
			m.Provider = &provider.Mock{Responses: []domain.ModelResponse{
				{Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{
					ProviderCallID: "waiting-tool", Name: tc.tool, Arguments: args,
				}}}},
				{Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemText, Text: "resumed and finished"}}},
			}}

			type push struct {
				notify.Event
				URL string `json:"url"` // Bark's tap action.
			}
			pushes := make(chan push, 1)
			pushPath := "/push"
			channel := notify.ChannelConfig{Type: tc.channel, Events: []string{tc.event}}
			if tc.channel == "bark" {
				channel.Credential = "test-device"
				pushPath += "/test-device"
			}
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != pushPath {
					t.Errorf("unexpected notification request: %s %s", r.Method, r.URL.Path)
				}
				var notice push
				if err := json.NewDecoder(r.Body).Decode(&notice); err != nil {
					t.Errorf("decode notification: %v", err)
				}
				w.WriteHeader(http.StatusNoContent)
				select {
				case pushes <- notice:
				default:
					t.Error("unexpected duplicate notification")
				}
			}))
			t.Cleanup(receiver.Close)
			channel.URL = receiver.URL + "/push"
			m.Notifier, err = notify.New(notify.Config{Channels: []notify.ChannelConfig{channel}})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer((&Server{Manager: m, Token: approvalTestToken}).Handler())
			t.Cleanup(server.Close)
			m.ApprovalBaseURL = server.URL
			client := server.Client()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			request := func(ctx context.Context, method, path, token, body string) *http.Response {
				t.Helper()
				req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				if body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				response, err := client.Do(req)
				if err != nil {
					t.Fatalf("%s %s: %v", method, path, err)
				}
				return response
			}
			read := func(response *http.Response, status int, dest any) {
				t.Helper()
				defer func() { _ = response.Body.Close() }()
				body, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != status {
					t.Fatalf("%s %s: got %d %s (%v), want %d", response.Request.Method, response.Request.URL.Path, response.StatusCode, body, err, status)
				}
				if dest != nil {
					if err := json.Unmarshal(body, dest); err != nil {
						t.Fatalf("decode %s: %v", body, err)
					}
				}
			}

			var session struct{ ID string }
			read(request(ctx, http.MethodPost, "/v1/sessions", approvalTestToken, ""), http.StatusCreated, &session)
			var waiting view
			read(request(ctx, http.MethodPost, "/v1/sessions/"+session.ID+"/runs?wait=1", approvalTestToken, `{"message":"trigger a wait"}`), http.StatusCreated, &waiting)
			if waiting.ID == "" || waiting.Status != domain.RunWaiting || waiting.WaitReason != tc.reason {
				t.Fatalf("waiting run: %+v", waiting)
			}
			var notice push
			select {
			case notice = <-pushes:
			case <-ctx.Done():
				t.Fatal("waiting notification did not arrive")
			}
			path := "/ui?run=" + url.QueryEscape(waiting.ID)
			if notice.ApprovalURL != server.URL+path || notice.RunID != waiting.ID || notice.EventType != tc.event || notice.Summary != tc.summary {
				t.Fatalf("waiting notification: %+v", notice)
			}
			if tc.channel == "bark" && notice.URL != notice.ApprovalURL {
				t.Fatalf("Bark tap action: %q, want %q", notice.URL, notice.ApprovalURL)
			}

			response := request(ctx, http.MethodGet, strings.TrimPrefix(notice.ApprovalURL, server.URL), "", "")
			read(response, http.StatusOK, nil)
			if got := response.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Fatalf("notification link returned %q", got)
			}
			read(request(ctx, http.MethodGet, "/v1/approvals", "", ""), http.StatusUnauthorized, nil)

			var queue []approvalView
			read(request(ctx, http.MethodGet, "/v1/approvals", approvalTestToken, ""), http.StatusOK, &queue)
			if len(queue) != 1 || queue[0].RunID != waiting.ID || queue[0].WaitReason != tc.queueReason || queue[0].Summary != tc.summary {
				t.Fatalf("waiting queue: %+v", queue)
			}
			runPath := "/v1/runs/" + waiting.ID
			var detail view
			read(request(ctx, http.MethodGet, runPath, approvalTestToken, ""), http.StatusOK, &detail)
			if detail.ID != waiting.ID || detail.Status != domain.RunWaiting || detail.WaitReason != tc.reason {
				t.Fatalf("run detail: %+v", detail)
			}
			if tc.reason == domain.WaitUser && detail.Question != tc.summary ||
				tc.reason == domain.WaitApproval && (detail.PendingInvocation == nil || detail.PendingInvocation.Tool != tc.tool) {
				t.Fatalf("missing wait context in detail: %+v", detail)
			}

			stream := request(ctx, http.MethodGet, runPath+"/events", approvalTestToken, "")
			defer func() { _ = stream.Body.Close() }()
			if stream.StatusCode != http.StatusOK || stream.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("event stream: %d %v", stream.StatusCode, stream.Header)
			}
			started := time.Now()
			resumeCtx, stopResume := context.WithDeadline(ctx, started.Add(time.Second))
			defer stopResume()
			var completed view
			read(request(resumeCtx, http.MethodPost, runPath+tc.endpoint, approvalTestToken, tc.payload), http.StatusOK, &completed)
			elapsed := time.Since(started)
			if elapsed >= time.Second || completed.ID != waiting.ID || completed.Status != domain.RunCompleted || completed.FinalAnswer != "resumed and finished" {
				t.Fatalf("resume after %s: %+v", elapsed, completed)
			}
			t.Logf("%s notification link resumed and completed the same run in %s", tc.channel, elapsed.Round(time.Millisecond))

			body, err := io.ReadAll(stream.Body)
			if err != nil {
				t.Fatal(err)
			}
			resumedAt := strings.Index(string(body), "event: run.resumed\n")
			completedAt := strings.Index(string(body), "event: run.completed\ndata: {\"status\":\"COMPLETED\"}")
			if resumedAt < 0 || completedAt <= resumedAt {
				t.Fatalf("event stream did not show resume and completion in order:\n%s", body)
			}
			batches, err := m.Store.LoadBatches(ctx, waiting.ID)
			if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 1 {
				t.Fatalf("resumed run batches: %+v (%v)", batches, err)
			}
			inv := batches[0].Invocations[0]
			if inv.Status != tc.invocationStatus || (tc.resultText != "" && (inv.Result == nil || !inv.Result.OK || !strings.Contains(string(inv.Result.Data), tc.resultText))) {
				t.Fatalf("response did not apply to the waiting invocation: %+v", inv)
			}
		})
	}
}
