package transport

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	contextpkg "github.com/BrianOGG/cloud-harness/internal/context"
)

func TestWebSkillDefinitions(t *testing.T) {
	global := t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(global, "config.json"))
	m, _ := newServer(t)
	m.Workspace = t.TempDir()
	h := (&Server{Manager: m, Token: approvalTestToken}).Handler()
	const defs = "/v1/skill-definitions"
	const file = defs + "/project/review"
	const content = "---\r\nname: review\r\ndescription: 'Review: API'\r\nlicense: MIT\r\n---\r\n# Review\r\n"
	body := map[string]string{"content": content}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", defs, nil))
	if rec.Code != 401 {
		t.Fatalf("unauthenticated status %d", rec.Code)
	}
	call := func(method, path string, input any, status int) string {
		t.Helper()
		return modelConfigRequest(t, h, method, path, input, status, "").Body.String()
	}
	if got := strings.TrimSpace(call("GET", defs, nil, 200)); got != "[]" {
		t.Fatal(got)
	}
	call("GET", file, nil, 404)
	call("POST", defs+"/other", body, 400)
	call("POST", defs+"/project", map[string]string{"path": "/tmp/test"}, 400)
	call("POST", defs+"/project", map[string]string{"content": strings.Replace(content, "name: review", "name: ../escape", 1)}, 400)
	globalDoc := installSkill(t, global, "review", "global", "global body")
	dir := filepath.Join(m.Workspace, ".agents", "skills", "review")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	companion := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(companion, []byte("extra content"), 0o600); err != nil {
		t.Fatal(err)
	}
	decode := func(raw string) contextpkg.SkillDocument {
		t.Helper()
		var doc contextpkg.SkillDocument
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	created := decode(call("POST", defs+"/project", body, 201))
	if created != decode(call("GET", file, nil, 200)) || created.Content != content || created.Scope != "project" || created.Revision == "" {
		t.Fatalf("round trip failed: %+v", created)
	}
	call("POST", defs+"/project", body, 409)
	var rows []contextpkg.ScopedSkill
	if err := json.Unmarshal([]byte(call("GET", defs, nil, 200)), &rows); err != nil {
		t.Fatal(err)
	}
	want := []contextpkg.ScopedSkill{
		{Skill: contextpkg.Skill{Name: "review", Description: "Review: API"}, Scope: "project", Effective: true},
		{Skill: contextpkg.Skill{Name: "review", Description: "global"}, Scope: "global"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("catalog = %+v", rows)
	}
	call("PUT", file, body, 400)
	call("PUT", file, map[string]string{"revision": created.Revision, "content": strings.Replace(content, "name: review", "name: another", 1)}, 400)
	next := decode(call("PUT", file, map[string]string{"revision": created.Revision, "content": content + "update"}, 200))
	call("DELETE", file, map[string]string{"revision": created.Revision}, 409)
	if data, err := os.ReadFile(filepath.Join(dir, "SKILL.md")); err != nil || string(data) != content+"update" {
		t.Fatalf("wrong bytes on disk: %q %v", data, err)
	}
	call("DELETE", file, map[string]string{"revision": next.Revision}, 204)
	if data, err := os.ReadFile(companion); err != nil || string(data) != "extra content" {
		t.Fatal("lost supporting file")
	}
	got, err := contextpkg.LoadSkill(m.Workspace, "review")
	if err != nil || got.Content != globalDoc || !strings.Contains(call("GET", "/v1/skills", nil, 200), `"description":"global"`) {
		t.Fatal("CLI loader and picker did not fall back to the global file")
	}
	globalRow := decode(call("GET", defs+"/global/review", nil, 200))
	call("DELETE", defs+"/global/review", map[string]string{"revision": globalRow.Revision}, 204)
	call("POST", defs+"/global", body, 201)
	if data, err := os.ReadFile(filepath.Join(global, ".agents", "skills", "review", "SKILL.md")); err != nil || string(data) != content {
		t.Fatal("global create used the wrong layer")
	}
}
