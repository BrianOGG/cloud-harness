package transport

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/provider"
)

func installSkill(t *testing.T, root, name, description, body string) string {
	t.Helper()
	dir := filepath.Join(root, ".agents", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWebSkillCatalog(t *testing.T) {
	global := t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(global, "config.json"))
	m, _ := newServer(t)
	m.Workspace = t.TempDir()
	h := (&Server{Manager: m, Token: approvalTestToken}).Handler()
	const path = "/v1/skills"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 401 {
		t.Fatalf("unauthenticated status = %d", rec.Code)
	}
	read := func() []map[string]string {
		t.Helper()
		rec := modelConfigRequest(t, h, "GET", path, nil, 200, "")
		var got []map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(); got == nil || len(got) != 0 {
		t.Fatalf("empty catalog = %#v", got)
	}
	installSkill(t, global, "shared", "global", "global body")
	installSkill(t, global, "alpha", "global only", "alpha body")
	installSkill(t, m.Workspace, "invalid", "", "ignored")
	installSkill(t, m.Workspace, "shared", "workspace", "workspace body")
	want := []map[string]string{{"name": "alpha", "description": "global only"}, {"name": "shared", "description": "workspace"}}
	if got := read(); !reflect.DeepEqual(want, got) {
		t.Fatalf("catalog = %#v", got)
	}
}

func TestWebExplicitSkillRun(t *testing.T) {
	global := t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(global, "config.json"))
	m, h := newServer(t)
	m.Workspace, m.SkipPlanner = t.TempDir(), true
	installSkill(t, global, "review", "global", "unused global skill body")
	selected := installSkill(t, m.Workspace, "review", "local", "workspace instructions @file:skill-example.txt")
	mock := m.Provider.(*provider.Mock)
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/sessions/" + session.ID + "/runs?wait=1"
	for _, name := range []string{"../review", "removed"} {
		modelConfigRequest(t, h, "POST", path, map[string]string{"skill": name, "message": "task"}, 400, "")
	}
	if runs, err := m.Store.ListRuns(t.Context(), session.ID); err != nil || len(runs) != 0 {
		t.Fatalf("invalid skill created run: %v %v", runs, err)
	}
	// The API opts in using a field. Existing clients can send literal slash text.
	modelConfigRequest(t, h, "POST", path, map[string]string{"message": "/skill review example"}, 201, "")
	first := mock.Requests[0].Messages
	if got := first[len(first)-1].Content; got != "/skill review example" {
		t.Fatalf("plain message = %q", got)
	}
	const task = "检查下面的代码\n  return 42"
	modelConfigRequest(t, h, "POST", path, map[string]string{"skill": "review", "message": task}, 201, "")
	last := mock.Requests[len(mock.Requests)-1].Messages
	user := last[len(last)-1]
	if user.Role != "user" || !strings.Contains(user.Content, selected) || !strings.Contains(user.Content, task) || strings.Contains(user.Content, "unused global skill body") {
		t.Fatalf("explicit skill or arguments missing from user context: %+v", user)
	}
	page, err := m.Store.ListMessages(t.Context(), session.ID, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	var inputs []string
	for _, msg := range page.Messages {
		if msg.Kind == "input" {
			inputs = append(inputs, msg.Content)
		}
	}
	if !reflect.DeepEqual(inputs, []string{"/skill review example", "/skill review " + task}) {
		t.Fatalf("turn inputs = %#v", inputs)
	}
	modelConfigRequest(t, h, "POST", path, map[string]string{"skill": "review"}, 201, "")
	last = mock.Requests[len(mock.Requests)-1].Messages
	if !strings.Contains(last[len(last)-1].Content, selected) {
		t.Fatal("skill-only run did not load the skill")
	}
}
