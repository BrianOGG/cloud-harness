package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

const messagesUnitCursor = "eyJ2IjoxLCJzZXNzaW9uX2lkIjoic2VzX2RlbW8iLCJjcmVhdGVkX2F0IjoiMjAyNi0xMS0yNVQwOTowMDowMS4wMDAwMDAwMDBaIiwiaWQiOiJtc2dfcmVzcG9uc2VfcnNwX2RlbW8ifQ"
const messagesUnitJSON = `{"v":1,"session_id":"ses_demo","created_at":"2026-11-25T09:00:01.000000000Z","id":"msg_response_rsp_demo"}`

// Embedding the interface makes any unexpected store call fail immediately.
// Startup recovery is an existing Handler behavior, separate from GET reads.
type messagesUnitStore struct {
	storage.Store
	calls      []string
	sessionErr error
	pageErr    error
	page       storage.MessagePage
	after      *storage.MessageCursor
	limit      int
}

func (s *messagesUnitStore) ListExpiredRunningRuns(context.Context, time.Time) ([]storage.Run, error) {
	return nil, nil
}

func (s *messagesUnitStore) GetSession(_ context.Context, id string) (storage.Session, error) {
	s.calls = append(s.calls, "session:"+id)
	return storage.Session{ID: id}, s.sessionErr
}

func (s *messagesUnitStore) ListMessages(_ context.Context, id string, after *storage.MessageCursor, limit int) (storage.MessagePage, error) {
	s.calls = append(s.calls, "messages:"+id)
	s.after, s.limit = after, limit
	return s.page, s.pageErr
}

func messagesUnitRequest(h http.Handler, session, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+session+"/messages", nil)
	req.URL.RawQuery = query
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func messagesUnitError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, message, ref string) {
	t.Helper()
	want := map[string]any{"error": map[string]any{"code": code, "message": message}}
	if ref != "" {
		want["error"].(map[string]any)["ref"] = ref
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != status || !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %d %s, want %d %+v (decode: %v)", rec.Code, rec.Body, status, want, err)
	}
}

func TestMessagesUnitCursorCanonical(t *testing.T) {
	at := time.Date(2026, 11, 25, 17, 0, 1, 0, time.FixedZone("Shanghai", 8*60*60))
	cursor := storage.MessageCursor{CreatedAt: at, ID: "msg_response_rsp_demo"}
	encoded, err := encodeMessageCursor("ses_demo", cursor)
	if err != nil || encoded != messagesUnitCursor {
		t.Fatalf("encoding = %q, %v", encoded, err)
	}
	for _, wire := range []string{
		messagesUnitCursor,
		base64.RawURLEncoding.EncodeToString([]byte(" \n{ \"id\": \"msg_response_rsp_demo\", \"created_at\": \"2026-11-25T09:00:01.000000000Z\", \"session_id\": \"ses_demo\", \"\\u0076\": 1 }\t")),
	} {
		got, err := decodeMessageCursor(wire, "ses_demo")
		if err != nil || got == nil || got.ID != cursor.ID || !got.CreatedAt.Equal(at) || got.CreatedAt.Location() != time.UTC {
			t.Fatalf("decoding = %+v, %v", got, err)
		}
	}
	// Session IDs are UTF-8; only the message ID is restricted to ASCII.
	wire := base64.RawURLEncoding.EncodeToString([]byte(strings.ReplaceAll(messagesUnitJSON, "ses_demo", "会话")))
	if _, err := decodeMessageCursor(wire, "会话"); err != nil {
		t.Fatal(err)
	}
}

func TestMessagesUnitCursorRejects(t *testing.T) {
	badJSON := map[string]string{
		"empty object":       `{}`,
		"null":               `null`,
		"array":              `[]`,
		"string":             `"cursor"`,
		"missing member":     strings.Replace(messagesUnitJSON, `,"id":"msg_response_rsp_demo"`, "", 1),
		"extra member":       strings.Replace(messagesUnitJSON, `"v":1`, `"extra":true,"v":1`, 1),
		"duplicate version":  strings.Replace(messagesUnitJSON, `"v":1`, `"v":1,"v":1`, 1),
		"escaped duplicate":  strings.Replace(messagesUnitJSON, `"v":1`, `"v":1,"\u0076":1`, 1),
		"duplicate session":  strings.Replace(messagesUnitJSON, `"v":1`, `"session_id":"ses_demo","v":1`, 1),
		"duplicate time":     strings.Replace(messagesUnitJSON, `"v":1`, `"created_at":"2026-11-25T09:00:01.000000000Z","v":1`, 1),
		"duplicate id":       strings.Replace(messagesUnitJSON, `"v":1`, `"id":"msg_response_rsp_demo","v":1`, 1),
		"version two":        strings.Replace(messagesUnitJSON, `"v":1`, `"v":2`, 1),
		"version float":      strings.Replace(messagesUnitJSON, `"v":1`, `"v":1.0`, 1),
		"version exponent":   strings.Replace(messagesUnitJSON, `"v":1`, `"v":1e0`, 1),
		"version string":     strings.Replace(messagesUnitJSON, `"v":1`, `"v":"1"`, 1),
		"version bool":       strings.Replace(messagesUnitJSON, `"v":1`, `"v":true`, 1),
		"version null":       strings.Replace(messagesUnitJSON, `"v":1`, `"v":null`, 1),
		"empty session":      strings.Replace(messagesUnitJSON, `"ses_demo"`, `""`, 1),
		"wrong session":      strings.Replace(messagesUnitJSON, `"ses_demo"`, `"ses_other"`, 1),
		"session case":       strings.Replace(messagesUnitJSON, `"ses_demo"`, `"SES_DEMO"`, 1),
		"session null":       strings.Replace(messagesUnitJSON, `"ses_demo"`, `null`, 1),
		"session object":     strings.Replace(messagesUnitJSON, `"ses_demo"`, `{}`, 1),
		"time number":        strings.Replace(messagesUnitJSON, `"2026-11-25T09:00:01.000000000Z"`, `123`, 1),
		"time null":          strings.Replace(messagesUnitJSON, `"2026-11-25T09:00:01.000000000Z"`, `null`, 1),
		"time array":         strings.Replace(messagesUnitJSON, `"2026-11-25T09:00:01.000000000Z"`, `[]`, 1),
		"no fraction":        strings.Replace(messagesUnitJSON, `.000000000Z`, `Z`, 1),
		"short fraction":     strings.Replace(messagesUnitJSON, `.000000000Z`, `.000Z`, 1),
		"long fraction":      strings.Replace(messagesUnitJSON, `.000000000Z`, `.0000000000Z`, 1),
		"comma fraction":     strings.Replace(messagesUnitJSON, `.000000000Z`, `,000000000Z`, 1),
		"offset":             strings.Replace(messagesUnitJSON, `.000000000Z`, `.000000000+00:00`, 1),
		"invalid date":       strings.Replace(messagesUnitJSON, `2026-11-25`, `2026-02-30`, 1),
		"leap second":        strings.Replace(messagesUnitJSON, `09:00:01`, `09:00:60`, 1),
		"short hour":         strings.Replace(messagesUnitJSON, `T09:`, `T9:`, 1),
		"empty id":           strings.Replace(messagesUnitJSON, `"msg_response_rsp_demo"`, `""`, 1),
		"id null":            strings.Replace(messagesUnitJSON, `"msg_response_rsp_demo"`, `null`, 1),
		"id bool":            strings.Replace(messagesUnitJSON, `"msg_response_rsp_demo"`, `true`, 1),
		"id number":          strings.Replace(messagesUnitJSON, `"msg_response_rsp_demo"`, `12`, 1),
		"id array":           strings.Replace(messagesUnitJSON, `"msg_response_rsp_demo"`, `[]`, 1),
		"unicode id":         strings.Replace(messagesUnitJSON, `msg_response_rsp_demo`, `msg_会话`, 1),
		"unpaired surrogate": strings.Replace(messagesUnitJSON, `msg_response_rsp_demo`, `msg_\ud800`, 1),
		"invalid UTF8":       strings.Replace(messagesUnitJSON, `ses_demo`, "ses_\xff", 1),
		"trailing object":    messagesUnitJSON + `{}`,
		"trailing null":      messagesUnitJSON + ` null`,
		"trailing comma":     strings.TrimSuffix(messagesUnitJSON, "}") + ",}",
		"truncated":          strings.TrimSuffix(messagesUnitJSON, "}"),
		"invalid escape":     strings.Replace(messagesUnitJSON, `msg_response_rsp_demo`, `msg_\x20`, 1),
	}
	for name, data := range badJSON {
		t.Run(name, func(t *testing.T) {
			if got, err := decodeMessageCursor(base64.RawURLEncoding.EncodeToString([]byte(data)), "ses_demo"); err == nil {
				t.Fatalf("accepted %q: %+v", data, got)
			}
		})
	}
	for name, wire := range map[string]string{
		"empty": "", "padding": messagesUnitCursor + "==", "newline": messagesUnitCursor + "\n",
		"space": " " + messagesUnitCursor, "tab": messagesUnitCursor + "\t",
		"plus": "+" + messagesUnitCursor, "slash": "/" + messagesUnitCursor,
		"impossible length": "A", "nonzero trailing bits": strings.TrimSuffix(messagesUnitCursor, "Q") + "R",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := decodeMessageCursor(wire, "ses_demo"); err == nil {
				t.Fatalf("accepted wire %q: %+v", wire, got)
			}
		})
	}
}

func TestMessagesUnitQueryValidation(t *testing.T) {
	cases := []struct {
		name, query, message string
		limit                int
		cursor               bool
	}{
		{name: "default", limit: 50},
		{name: "empty cursor", query: "cursor=", limit: 50},
		{name: "bare cursor", query: "cursor", limit: 50},
		{name: "minimum", query: "limit=1", limit: 1},
		{name: "maximum", query: "limit=200", limit: 200},
		{name: "escaped once", query: "l%69mit=%32&ignored=%25&run_id=other", limit: 2},
		{name: "logical boundary", query: "cursor=" + messagesUnitCursor + "&limit=3", limit: 3, cursor: true},
		{name: "double escaping", query: "limit=%2531", message: "invalid message limit"},
		{name: "bad query beats limit", query: "limit=0&cursor=bad&unused=%GG", message: "invalid message query"},
		{name: "bad query key", query: "%GG=value", message: "invalid message query"},
		{name: "semicolon", query: "unused=a;b", message: "invalid message query"},
		{name: "bad limit beats cursor", query: "cursor=bad&limit=0", message: "invalid message limit"},
		{name: "reverse priority", query: "limit=0&cursor=bad", message: "invalid message limit"},
		{name: "duplicate limit", query: "limit=1&limit=1", message: "invalid message limit"},
		{name: "escaped duplicate", query: "limit=1&%6cimit=1", message: "invalid message limit"},
		{name: "duplicate cursor", query: "cursor=&cursor=", message: "invalid message cursor"},
		{name: "duplicate nonempty cursor", query: "cursor=" + messagesUnitCursor + "&cursor=" + messagesUnitCursor, message: "invalid message cursor"},
		{name: "cursor whitespace", query: "cursor=+", message: "invalid message cursor"},
		{name: "cursor invalid", query: "cursor=bad", message: "invalid message cursor"},
	}
	for _, value := range []string{"", "0", "00", "01", "201", "1000", "9999999999999999999999", "-1", "%2B1", "1+", "+1", "1.0", "1e1", "%EF%BC%91", "%D9%A1", "%09%31", "1%00", "%FF"} {
		cases = append(cases, struct {
			name, query, message string
			limit                int
			cursor               bool
		}{name: "limit " + value, query: "limit=" + value, message: "invalid message limit"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &messagesUnitStore{}
			h := (&Server{Manager: &agent.Manager{Store: store}}).Handler()
			rec := messagesUnitRequest(h, "ses_demo", tc.query)
			if tc.message != "" {
				messagesUnitError(t, rec, 400, "invalid_argument", tc.message, "")
				if len(store.calls) != 0 {
					t.Fatalf("invalid query reached store: %v", store.calls)
				}
				return
			}
			if rec.Code != 200 || rec.Body.String() != "{\"messages\":[],\"next_cursor\":\"\"}\n" || store.limit != tc.limit || (store.after != nil) != tc.cursor {
				t.Fatalf("response=%d %s limit=%d after=%+v", rec.Code, rec.Body, store.limit, store.after)
			}
			if !reflect.DeepEqual(store.calls, []string{"session:ses_demo", "messages:ses_demo"}) {
				t.Fatalf("calls = %v", store.calls)
			}
			if tc.cursor && (store.after.ID != "msg_response_rsp_demo" || store.after.CreatedAt.Format(messageTimeLayout) != "2026-11-25T09:00:01.000000000Z") {
				t.Fatalf("boundary = %+v", store.after)
			}
		})
	}
}

func TestMessagesUnitGuardPriority(t *testing.T) {
	const token = "messages-unit-token"
	cases := []struct {
		name, host, origin, auth, message string
		status                            int
	}{
		{"host first", "untrusted.test", "http://other.test", "", "Host header is not a loopback address or an allowed host", 403},
		{"origin before bearer", "localhost", "http://other.test", "", "cross-origin request refused", 403},
		{"null origin", "localhost", "null", "Bearer " + token, "cross-origin request refused", 403},
		{"bearer before query", "localhost", "http://localhost", "Bearer wrong-token", "missing or invalid bearer token", 401},
		{"missing bearer", "localhost", "", "", "missing or invalid bearer token", 401},
		{"authenticated query", "localhost", "http://localhost", "Bearer " + token, "invalid message query", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &messagesUnitStore{}
			h := (&Server{Manager: &agent.Manager{Store: store}, Token: token, LoopbackBind: true}).Handler()
			req := httptest.NewRequest(http.MethodGet, "/v1/sessions/ses_demo/messages", nil)
			req.URL.RawQuery = "limit=0&cursor=bad&unused=%GG"
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			req.Header.Set("Authorization", tc.auth)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			code := "auth_denied"
			if tc.status == 400 {
				code = "invalid_argument"
			}
			messagesUnitError(t, rec, tc.status, code, tc.message, "")
			if len(store.calls) != 0 {
				t.Fatalf("guard/query failure reached store: %v", store.calls)
			}
			challenge := rec.Header().Get("WWW-Authenticate")
			if tc.status == 401 && challenge != `Bearer realm="cloud-harness"` || tc.status != 401 && challenge != "" {
				t.Fatalf("challenge = %q", challenge)
			}
		})
	}
}

func TestMessagesUnitPageWire(t *testing.T) {
	at := time.Date(2026, 11, 25, 17, 0, 1, 123456789, time.FixedZone("Shanghai", 8*60*60))
	store := &messagesUnitStore{page: storage.MessagePage{
		Messages: []storage.Message{
			{ID: "msg_user_turn", SessionID: "ses_demo", TurnID: "turn", Role: "user", Kind: "input", Status: "recorded", Content: "原文\n<a>&", Source: "hidden", CreatedAt: at},
			{ID: "msg_status_run", SessionID: "ses_demo", RunID: "run", Role: "system", Kind: "run_status", Status: "failed", Content: "stopped", Event: "run.FAILED", CreatedAt: at},
		},
	}}
	h := (&Server{Manager: &agent.Manager{Store: store}}).Handler()
	rec := messagesUnitRequest(h, "ses_demo", "limit=2")
	want := `{"messages":[{"id":"msg_user_turn","session_id":"ses_demo","run_id":null,"turn_id":"turn","role":"user","kind":"input","status":"recorded","content":"原文\n<a>&","event":"","created_at":"2026-11-25T09:00:01.123456789Z"},{"id":"msg_status_run","session_id":"ses_demo","run_id":"run","turn_id":null,"role":"system","kind":"run_status","status":"failed","content":"stopped","event":"run.FAILED","created_at":"2026-11-25T09:00:01.123456789Z"}],"next_cursor":""}` + "\n"
	if rec.Code != 200 || rec.Body.String() != want || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response=%d %s, want %s", rec.Code, rec.Body, want)
	}
}

func TestMessagesUnitFailures(t *testing.T) {
	cases := []struct {
		name, query string
		store       messagesUnitStore
		status      int
		message     string
		calls       []string
	}{
		{"cursor before missing session", "cursor=" + messagesUnitCursor, messagesUnitStore{sessionErr: domain.E(domain.CodeNotFound, "private")}, 400, "invalid message cursor", nil},
		{"missing session", "", messagesUnitStore{sessionErr: domain.E(domain.CodeNotFound, "private").WithRef("wrong-ref")}, 404, "record not found", []string{"session:unknown"}},
		{"valid beyond end missing session", "cursor=" + base64.RawURLEncoding.EncodeToString([]byte(strings.ReplaceAll(messagesUnitJSON, "ses_demo", "unknown"))), messagesUnitStore{sessionErr: domain.E(domain.CodeNotFound, "private")}, 404, "record not found", []string{"session:unknown"}},
		{"session failure", "", messagesUnitStore{sessionErr: errors.New("private database failure")}, 500, "request failed", []string{"session:unknown"}},
		{"domain session failure", "", messagesUnitStore{sessionErr: domain.E(domain.CodeCorrupt, "private database failure")}, 500, "request failed", []string{"session:unknown"}},
		{"page failure", "", messagesUnitStore{pageErr: errors.New("private page failure")}, 500, "request failed", []string{"session:unknown", "messages:unknown"}},
		{"domain page failure", "", messagesUnitStore{pageErr: domain.E(domain.CodeInvalidArgument, "private storage failure")}, 500, "request failed", []string{"session:unknown", "messages:unknown"}},
		{"unencodable time", "", messagesUnitStore{page: storage.MessagePage{Messages: []storage.Message{{CreatedAt: time.Now()}, {CreatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}}}, 500, "request failed", []string{"session:unknown", "messages:unknown"}},
		{"unencodable cursor", "", messagesUnitStore{page: storage.MessagePage{Messages: []storage.Message{{CreatedAt: time.Now()}}, NextCursor: &storage.MessageCursor{ID: "非ASCII", CreatedAt: time.Now()}}}, 500, "request failed", []string{"session:unknown", "messages:unknown"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := (&Server{Manager: &agent.Manager{Store: &tc.store}}).Handler()
			rec := messagesUnitRequest(h, "unknown", tc.query)
			code, ref := "internal", ""
			if tc.status == 400 {
				code = "invalid_argument"
			}
			if tc.status == 404 {
				code, ref = "not_found", "unknown"
			}
			messagesUnitError(t, rec, tc.status, code, tc.message, ref)
			if !reflect.DeepEqual(tc.store.calls, tc.calls) {
				t.Fatalf("calls = %v, want %v", tc.store.calls, tc.calls)
			}
		})
	}
}

func TestMessagesUnitSQLitePagesReadOnly(t *testing.T) {
	m, h := newServer(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 25, 9, 0, 1, 123456789, time.UTC)
	for _, id := range []string{"ses_demo", "ses_empty", "ses_other"} {
		if err := m.Store.CreateSession(ctx, storage.Session{ID: id, CreatedAt: at, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	for _, turn := range []storage.Turn{
		{ID: "b", SessionID: "ses_demo", UserInput: "same", CreatedAt: at},
		{ID: "a", SessionID: "ses_demo", UserInput: "same", CreatedAt: at},
		{ID: "c", SessionID: "ses_demo", UserInput: "later\n原文", CreatedAt: at.Add(time.Nanosecond)},
		{ID: "other", SessionID: "ses_other", UserInput: "exclude", CreatedAt: at},
	} {
		if err := m.Store.CreateTurn(ctx, turn); err != nil {
			t.Fatal(err)
		}
	}
	read := func(session, query string, ids []string, more bool) string {
		t.Helper()
		rec := messagesUnitRequest(h, session, query)
		var page struct {
			Messages []struct {
				ID        string `json:"id"`
				SessionID string `json:"session_id"`
				RunID     any    `json:"run_id"`
			} `json:"messages"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || rec.Code != 200 || page.Messages == nil || len(page.Messages) != len(ids) || (page.NextCursor != "") != more {
			t.Fatalf("response = %d %s, error=%v", rec.Code, rec.Body, err)
		}
		for i, message := range page.Messages {
			if message.ID != ids[i] || message.SessionID != session || message.RunID != nil {
				t.Fatalf("message = %+v, expected ID %s", message, ids[i])
			}
		}
		return page.NextCursor
	}
	first := read("ses_demo", "limit=1", []string{"msg_user_a"}, true)
	data, err := base64.RawURLEncoding.DecodeString(first)
	if err != nil || string(data) != `{"v":1,"session_id":"ses_demo","created_at":"2026-11-25T09:00:01.123456789Z","id":"msg_user_a"}` {
		t.Fatalf("first boundary = %s, %v", data, err)
	}
	read("ses_demo", "cursor="+first+"&limit=2", []string{"msg_user_b", "msg_user_c"}, false)
	read("ses_demo", "limit=200", []string{"msg_user_a", "msg_user_b", "msg_user_c"}, false)
	read("ses_empty", "", nil, false)
	logical := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"session_id":"ses_demo","created_at":"2026-11-25T09:00:01.123456789Z","id":"msg_user_a0"}`))
	read("ses_demo", "cursor="+url.QueryEscape(logical), []string{"msg_user_b", "msg_user_c"}, false)
	beyond := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"session_id":"ses_demo","created_at":"9999-12-31T23:59:59.999999999Z","id":"missing_anchor"}`))
	read("ses_demo", "cursor="+beyond, nil, false)
	read("ses_demo", "limit=1", []string{"msg_user_a"}, true)
	runs, err := m.Store.ListRuns(ctx, "ses_demo")
	if err != nil || len(runs) != 0 {
		t.Fatalf("GET created runs: %+v, %v", runs, err)
	}
}
