package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestWorkspaceToolsDenyEscapeAndDanger(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	charge := &memCharge{b: domain.Budget{MaxToolCalls: 10}}
	grants := &memGrants{grants: []domain.Grant{{
		ID: "g", Tool: "read_file", Op: "call", State: "active", RunID: "run",
		ResourceScope: root,
	}}}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}
	ok, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "read_file", Args: json.RawMessage(`{"path":"note.txt"}`)})
	if err != nil || !ok.OK || !ok.Dispatched {
		t.Fatalf("read %+v %v", ok, err)
	}
	escaped, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "read_file", Args: json.RawMessage(`{"path":"../secret"}`)})
	if err != nil || escaped.Dispatched || escaped.Error == nil || escaped.Error.Code != domain.CodeAuthDenied {
		t.Fatalf("escape %+v %v", escaped, err)
	}
	write, err := b.Authorize(context.Background(), Request{
		RunID: "run", Tool: "write_file", ApprovalPossible: true,
		Args: json.RawMessage(`{"path":"out.txt","content":"x"}`),
	})
	if err != nil || !write.NeedsApproval || write.Dispatched {
		t.Fatalf("write should wait for approval: %+v %v", write, err)
	}
	if _, err := os.Stat(filepath.Join(root, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("write ran without a grant")
	}
	danger, err := b.Authorize(context.Background(), Request{
		RunID: "run", Tool: "run_command", ApprovalPossible: true,
		Args: json.RawMessage(`{"command":"rm -rf sentinel"}`), Now: time.Now(),
	})
	if err != nil || danger.Dispatched || danger.NeedsApproval && danger.OK {
		t.Fatalf("danger %+v %v", danger, err)
	}
	if danger.Error == nil {
		t.Fatal("dangerous command was allowed")
	}
	// A direct invoke still must not start a dangerous command, even if a grant exists.
	grants.grants = append(grants.grants, domain.Grant{
		ID: "g2", Tool: "run_command", Op: "call", State: "active", RunID: "run", ResourceScope: root,
	})
	ran, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "run_command", Args: json.RawMessage(`{"command":"rm -rf sentinel"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ran.OK {
		t.Fatal("dangerous command succeeded")
	}
	body, err := os.ReadFile(sentinel)
	if err != nil || string(body) != "keep" {
		t.Fatalf("sentinel changed: %q %v", body, err)
	}
}

func TestWriteProbeConfirmsWithoutWriting(t *testing.T) {
	root := t.TempDir()
	b := NewBroker(&memCharge{b: domain.Budget{MaxToolCalls: 1}}, &memGrants{})
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"path":"out.txt","content":"x"}`)
	missing, err := b.Probe(context.Background(), Request{Tool: "write_file", Args: args})
	if err != nil || missing.Known {
		t.Fatalf("missing file %+v %v", missing, err)
	}
	if _, err := os.Stat(filepath.Join(root, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("probe created the file")
	}
	if err := os.WriteFile(filepath.Join(root, "out.txt"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	other, err := b.Probe(context.Background(), Request{Tool: "write_file", Args: args})
	if err != nil || other.Known {
		t.Fatalf("mismatched content confirmed %+v %v", other, err)
	}
	if err := os.WriteFile(filepath.Join(root, "out.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	hit, err := b.Probe(context.Background(), Request{Tool: "write_file", Args: args})
	if err != nil || !hit.Known || !hit.OK || !bytes.Contains(hit.Data, []byte(`"probed":true`)) {
		t.Fatalf("probe %+v %v", hit, err)
	}
	body, err := os.ReadFile(filepath.Join(root, "out.txt"))
	if err != nil || string(body) != "x" {
		t.Fatalf("probe changed the file: %q %v", body, err)
	}
	cmd, err := b.Probe(context.Background(), Request{Tool: "run_command", Args: json.RawMessage(`{"command":"ls"}`)})
	if err != nil || cmd.Known {
		t.Fatalf("command probe %+v %v", cmd, err)
	}
}

func TestLocalToolSchemasAreStrict(t *testing.T) {
	root := t.TempDir()
	b := NewBroker(&memCharge{b: domain.Budget{MaxToolCalls: 1}}, &memGrants{})
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}
	specs := map[string]PublicSpec{}
	for _, s := range b.List() {
		specs[s.Name] = s
	}
	checkSchema := func(name string, wantProps []string, forbidProps []string, wantReq []string) {
		t.Helper()
		spec, ok := specs[name]
		if !ok {
			t.Fatalf("missing tool %s", name)
		}
		var schema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
			t.Fatalf("%s schema unmarshal: %v", name, err)
		}
		if schema.Type != "object" {
			t.Fatalf("%s type=%q", name, schema.Type)
		}
		for _, p := range wantProps {
			if _, ok := schema.Properties[p]; !ok {
				t.Fatalf("%s missing property %q in %s", name, p, spec.Parameters)
			}
		}
		for _, p := range forbidProps {
			if _, ok := schema.Properties[p]; ok {
				t.Fatalf("%s must not declare property %q in %s", name, p, spec.Parameters)
			}
		}
		if len(schema.Required) != len(wantReq) {
			t.Fatalf("%s required=%v, want %v", name, schema.Required, wantReq)
		}
		reqSet := map[string]bool{}
		for _, r := range schema.Required {
			reqSet[r] = true
		}
		for _, r := range wantReq {
			if !reqSet[r] {
				t.Fatalf("%s required=%v missing %q", name, schema.Required, r)
			}
		}
	}
	checkSchema("read_file", []string{"path", "start_line", "end_line"}, []string{"content", "command"}, []string{"path"})
	checkSchema("write_file", []string{"path", "content"}, []string{"command"}, []string{"path", "content"})
	checkSchema("edit_file", []string{"path", "old_string", "new_string"}, []string{"content", "command"}, []string{"path", "old_string", "new_string"})
	checkSchema("list_dir", []string{"path"}, []string{"content", "command", "pattern"}, nil)
	checkSchema("list_files", []string{"pattern"}, []string{"content", "command", "path"}, nil)
	checkSchema("grep", []string{"pattern", "glob", "literal", "max_hits"}, []string{"content", "command"}, []string{"pattern"})
	checkSchema("git_status", nil, []string{"content", "command", "path"}, nil)
	checkSchema("git_diff", []string{"staged", "path"}, []string{"content", "command"}, nil)
	checkSchema("git_log", []string{"limit"}, []string{"content", "command", "path"}, nil)
	checkSchema("run_command", []string{"command", "cwd"}, []string{"content"}, []string{"command"})
	for _, s := range b.List() {
		assertOnlyStandardSchemaKeys(t, s.Name, s.Parameters)
	}
}

func assertOnlyStandardSchemaKeys(t *testing.T, toolName string, raw json.RawMessage) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s schema invalid JSON: %v (%s)", toolName, err, raw)
	}
	checkNodeKeys(t, toolName, "$", m)
}

func checkNodeKeys(t *testing.T, toolName, loc string, node map[string]any) {
	t.Helper()
	for k := range node {
		if !standardJSONSchemaKeys[k] {
			t.Fatalf("%s at %s contains non-standard schema key %q in %v", toolName, loc, k, node)
		}
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for propName, propVal := range props {
			propMap, ok := propVal.(map[string]any)
			if !ok {
				t.Fatalf("%s property %s.%s is not a schema object: %#v", toolName, loc, propName, propVal)
			}
			checkNodeKeys(t, toolName, loc+".properties."+propName, propMap)
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		checkNodeKeys(t, toolName, loc+".items", items)
	}
}

func TestProviderSchemasContainOnlyStandardKeys(t *testing.T) {
	root := t.TempDir()
	b := NewBroker(&memCharge{b: domain.Budget{MaxToolCalls: 10}}, &memGrants{})
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}
	// Register a tool whose raw Parameters embeds non-standard internal metadata keys.
	b.Register(Spec{
		Name:            "custom_polluted",
		Description:     "Tool with polluted raw schema",
		Readonly:        true,
		Idempotent:      true,
		Pollable:        true,
		SideEffectClass: "reversible",
		Parameters: json.RawMessage(`{
			"type":"object",
			"readOnly":true,
			"readonly":true,
			"sideEffect":"reversible",
			"sideEffectClass":"reversible",
			"reversible":true,
			"idempotent":true,
			"pollable":true,
			"properties":{
				"query":{"type":"string","description":"Search query","readOnly":true,"sideEffect":"none","reversible":false}
			},
			"required":["query"],
			"additionalProperties":false
		}`),
	})

	// Internal metadata stays intact on Broker.Lookup.
	fact, ok := b.Lookup("custom_polluted")
	if !ok || !fact.Readonly || !fact.Idempotent || !fact.Pollable || fact.SideEffect != "reversible" {
		t.Fatalf("internal tool metadata lost: %+v (ok=%v)", fact, ok)
	}

	// Provider-facing schemas from List() and ToMCPTool must have all non-standard keys stripped.
	for _, pub := range b.List() {
		assertOnlyStandardSchemaKeys(t, pub.Name, pub.Parameters)
		for _, forbidden := range []string{"readOnly", "readonly", "sideEffect", "sideEffectClass", "reversible", "idempotent", "pollable"} {
			if bytes.Contains(pub.Parameters, []byte(`"`+forbidden+`"`)) {
				t.Fatalf("%s provider schema leaked %q: %s", pub.Name, forbidden, pub.Parameters)
			}
		}
	}
	mcpTool := ToMCPTool(Spec{
		Name:       "mcp_polluted",
		Parameters: json.RawMessage(`{"type":"object","readOnly":true,"sideEffect":"none","reversible":true,"properties":{"x":{"type":"string","readOnly":true}}}`),
	})
	assertOnlyStandardSchemaKeys(t, mcpTool.Name, mcpTool.InputSchema)
}

func TestSymlinkEscapeDenied(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	outsideSecret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("outside-secret-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideSecret, filepath.Join(root, "link_secret.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link_dir")); err != nil {
		t.Fatal(err)
	}
	outsideMissing := filepath.Join(outside, "created_outside.txt")
	if err := os.Symlink(outsideMissing, filepath.Join(root, "dangling_out.txt")); err != nil {
		t.Fatal(err)
	}

	charge := &memCharge{b: domain.Budget{MaxToolCalls: 50}}
	grants := &memGrants{grants: []domain.Grant{
		{ID: "g1", Tool: "read_file", Op: "call", State: "active", RunID: "run", ResourceScope: root},
		{ID: "g2", Tool: "write_file", Op: "call", State: "active", RunID: "run", ResourceScope: root},
		{ID: "g3", Tool: "edit_file", Op: "call", State: "active", RunID: "run", ResourceScope: root},
		{ID: "g4", Tool: "list_files", Op: "call", State: "active", RunID: "run", ResourceScope: root},
		{ID: "g5", Tool: "grep", Op: "call", State: "active", RunID: "run", ResourceScope: root},
	}}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}

	// read_file via file symlink and dir symlink must be denied.
	for _, p := range []string{"link_secret.txt", "link_dir/secret.txt"} {
		res, err := b.Invoke(context.Background(), Request{
			RunID: "run", Tool: "read_file",
			Args: json.RawMessage(fmt.Sprintf(`{"path":%q}`, p)),
		})
		if err != nil || res.OK || res.Dispatched || res.Error == nil || res.Error.Code != domain.CodeAuthDenied {
			t.Fatalf("read_file(%s) should be denied: %+v %v", p, res, err)
		}
	}

	// write_file via file symlink, dir symlink (not-yet-existing child), and dangling symlink must be denied.
	for _, p := range []string{"link_secret.txt", "link_dir/new_escape.txt", "dangling_out.txt"} {
		res, err := b.Invoke(context.Background(), Request{
			RunID: "run", Tool: "write_file",
			Args: json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"pwned"}`, p)),
		})
		if err != nil || res.OK || res.Dispatched || res.Error == nil || res.Error.Code != domain.CodeAuthDenied {
			t.Fatalf("write_file(%s) should be denied: %+v %v", p, res, err)
		}
		authRes, err := b.Authorize(context.Background(), Request{
			RunID: "run", Tool: "write_file", ApprovalPossible: true,
			Args: json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"pwned"}`, p)),
		})
		if err != nil || authRes.OK || authRes.NeedsApproval || authRes.Error == nil || authRes.Error.Code != domain.CodeAuthDenied {
			t.Fatalf("write_file Authorize(%s) should be denied: %+v %v", p, authRes, err)
		}
	}

	// edit_file via file symlink and dir symlink must be denied.
	for _, p := range []string{"link_secret.txt", "link_dir/secret.txt"} {
		res, err := b.Invoke(context.Background(), Request{
			RunID: "run", Tool: "edit_file",
			Args: json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":"outside-secret-content","new_string":"pwned"}`, p)),
		})
		if err != nil || res.OK || res.Dispatched || res.Error == nil || res.Error.Code != domain.CodeAuthDenied {
			t.Fatalf("edit_file(%s) should be denied: %+v %v", p, res, err)
		}
		authRes, err := b.Authorize(context.Background(), Request{
			RunID: "run", Tool: "edit_file", ApprovalPossible: true,
			Args: json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":"outside-secret-content","new_string":"pwned"}`, p)),
		})
		if err != nil || authRes.OK || authRes.NeedsApproval || authRes.Error == nil || authRes.Error.Code != domain.CodeAuthDenied {
			t.Fatalf("edit_file Authorize(%s) should be denied: %+v %v", p, authRes, err)
		}
	}

	// Verify external files were never modified or created.
	gotSecret, err := os.ReadFile(outsideSecret)
	if err != nil || string(gotSecret) != "outside-secret-content" {
		t.Fatalf("outside secret modified: %q %v", gotSecret, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new_escape.txt")); !os.IsNotExist(err) {
		t.Fatal("write_file created new_escape.txt outside workspace")
	}
	if _, err := os.Stat(outsideMissing); !os.IsNotExist(err) {
		t.Fatal("write_file created dangling_out.txt target outside workspace")
	}
}

func TestEditFileUniqueMatchRequired(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "code.go")
	initial := "package main\n\nfunc alpha() {\n\tprintln(\"hello\")\n}\n\nfunc beta() {\n\tprintln(\"hello\")\n}\n"
	if err := os.WriteFile(target, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	charge := &memCharge{b: domain.Budget{MaxToolCalls: 20}}
	grants := &memGrants{}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}

	// Without a grant, edit_file requires approval just like write_file.
	auth, err := b.Authorize(context.Background(), Request{
		RunID: "run", Tool: "edit_file", ApprovalPossible: true,
		Args: json.RawMessage(`{"path":"code.go","old_string":"func alpha()","new_string":"func alphaRenamed()"}`),
	})
	if err != nil || !auth.NeedsApproval || auth.Dispatched {
		t.Fatalf("edit_file should wait for approval without grant: %+v %v", auth, err)
	}

	// Grant edit_file on root.
	grants.grants = append(grants.grants, domain.Grant{
		ID: "g_edit", Tool: "edit_file", Op: "call", State: "active", RunID: "run", ResourceScope: root,
	})

	// 1. Non-unique old_string (appears twice) must be rejected and leave file unchanged.
	dupRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "edit_file",
		Args: json.RawMessage(`{"path":"code.go","old_string":"println(\"hello\")","new_string":"println(\"world\")"}`),
	})
	if err != nil || dupRes.OK || dupRes.Error == nil {
		t.Fatalf("expected non-unique old_string error, got %+v %v", dupRes, err)
	}
	afterDup, err := os.ReadFile(target)
	if err != nil || string(afterDup) != initial {
		t.Fatalf("file changed after non-unique edit: %q %v", afterDup, err)
	}

	// 2. Zero-occurrence old_string must be rejected and leave file unchanged.
	zeroRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "edit_file",
		Args: json.RawMessage(`{"path":"code.go","old_string":"nonexistent_symbol","new_string":"replacement"}`),
	})
	if err != nil || zeroRes.OK || zeroRes.Error == nil {
		t.Fatalf("expected zero-match old_string error, got %+v %v", zeroRes, err)
	}
	afterZero, err := os.ReadFile(target)
	if err != nil || string(afterZero) != initial {
		t.Fatalf("file changed after zero-match edit: %q %v", afterZero, err)
	}

	// StatusProbe before edit lands must report Known=false.
	editArgs := json.RawMessage(`{"path":"code.go","old_string":"func alpha()","new_string":"func alphaRenamed()"}`)
	probeBefore, err := b.Probe(context.Background(), Request{Tool: "edit_file", Args: editArgs})
	if err != nil || probeBefore.Known {
		t.Fatalf("probe before edit should be Known=false: %+v %v", probeBefore, err)
	}

	// 3. Unique old_string succeeds and updates only that match.
	okRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "edit_file", Args: editArgs,
	})
	if err != nil || !okRes.OK || !okRes.Dispatched {
		t.Fatalf("unique edit failed: %+v %v", okRes, err)
	}
	afterOK, err := os.ReadFile(target)
	want := strings.Replace(initial, "func alpha()", "func alphaRenamed()", 1)
	if err != nil || string(afterOK) != want {
		t.Fatalf("file after unique edit = %q, want %q (err=%v)", afterOK, want, err)
	}

	// StatusProbe after edit lands must report Known=true, OK=true with probed=true.
	probeAfter, err := b.Probe(context.Background(), Request{Tool: "edit_file", Args: editArgs})
	if err != nil || !probeAfter.Known || !probeAfter.OK || !bytes.Contains(probeAfter.Data, []byte(`"probed":true`)) {
		t.Fatalf("probe after edit: %+v %v", probeAfter, err)
	}
}

func TestReadFileLineRange(t *testing.T) {
	root := t.TempDir()
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line-%d", i))
	}
	filePath := filepath.Join(root, "ten.txt")
	if err := os.WriteFile(filePath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	charge := &memCharge{b: domain.Budget{MaxToolCalls: 20}}
	grants := &memGrants{grants: []domain.Grant{
		{ID: "g", Tool: "read_file", Op: "call", State: "active", RunID: "run", ResourceScope: root},
	}}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}

	// 1. Read a line range (lines 3..5 inclusive).
	rangeRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "read_file",
		Args: json.RawMessage(`{"path":"ten.txt","start_line":3,"end_line":5}`),
	})
	if err != nil || !rangeRes.OK {
		t.Fatalf("range read: %+v %v", rangeRes, err)
	}
	var gotRange struct {
		Path       string `json:"path"`
		Content    string `json:"content"`
		TotalLines int    `json:"total_lines"`
		StartLine  int    `json:"start_line"`
		EndLine    int    `json:"end_line"`
		Truncated  bool   `json:"truncated"`
		Notice     string `json:"notice"`
	}
	if err := json.Unmarshal(rangeRes.Data, &gotRange); err != nil {
		t.Fatal(err)
	}
	if gotRange.Content != "line-3\nline-4\nline-5" {
		t.Fatalf("range content = %q, want %q", gotRange.Content, "line-3\nline-4\nline-5")
	}
	if gotRange.TotalLines != 10 || gotRange.StartLine != 3 || gotRange.EndLine != 5 {
		t.Fatalf("range metadata = %+v", gotRange)
	}
	if !gotRange.Truncated || !strings.Contains(gotRange.Notice, "3-5") || !strings.Contains(gotRange.Notice, "10") {
		t.Fatalf("expected range truncation notice, got truncated=%v notice=%q", gotRange.Truncated, gotRange.Notice)
	}

	// 2. Read full file without range: total_lines=10, truncated=false, notice="".
	fullRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "read_file",
		Args: json.RawMessage(`{"path":"ten.txt"}`),
	})
	if err != nil || !fullRes.OK {
		t.Fatalf("full read: %+v %v", fullRes, err)
	}
	var gotFull struct {
		Content    string `json:"content"`
		TotalLines int    `json:"total_lines"`
		Truncated  bool   `json:"truncated"`
		Notice     string `json:"notice"`
	}
	if err := json.Unmarshal(fullRes.Data, &gotFull); err != nil {
		t.Fatal(err)
	}
	if gotFull.TotalLines != 10 || gotFull.Truncated || gotFull.Notice != "" {
		t.Fatalf("unexpected full read metadata: %+v", gotFull)
	}

	// 3. Read file over 1MiB cap: must report total_lines and explicit truncation notice.
	bigContent := strings.Repeat("abcdefghij\n", 100_000) // ~1.1MB, 100,000 lines
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(bigContent), 0o644); err != nil {
		t.Fatal(err)
	}
	bigRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "read_file",
		Args: json.RawMessage(`{"path":"big.txt"}`),
	})
	if err != nil || !bigRes.OK {
		t.Fatalf("big read: %+v %v", bigRes, err)
	}
	var gotBig struct {
		Content    string `json:"content"`
		TotalLines int    `json:"total_lines"`
		Truncated  bool   `json:"truncated"`
		Notice     string `json:"notice"`
	}
	if err := json.Unmarshal(bigRes.Data, &gotBig); err != nil {
		t.Fatal(err)
	}
	if len(gotBig.Content) != maxFileBytes {
		t.Fatalf("big content len = %d, want %d", len(gotBig.Content), maxFileBytes)
	}
	if gotBig.TotalLines != 100_000 || !gotBig.Truncated || !strings.Contains(gotBig.Notice, "truncated") {
		t.Fatalf("expected size truncation notice, got total_lines=%d truncated=%v notice=%q", gotBig.TotalLines, gotBig.Truncated, gotBig.Notice)
	}
}

func TestGrepFindsAndCaps(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".cloud-harness"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "a.go"), []byte("package pkg\n// FIND_ME in go\nfunc call(x int) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("first line\nFIND_ME in txt with [literal](test)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("FIND_ME in git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".cloud-harness", "state"), []byte("FIND_ME in cloud-harness\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	charge := &memCharge{b: domain.Budget{MaxToolCalls: 20}}
	grants := &memGrants{grants: []domain.Grant{
		{ID: "g", Tool: "grep", Op: "call", State: "active", RunID: "run", ResourceScope: root},
	}}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}

	// 1. Regex search finds matches across workspace and skips .git and .cloud-harness.
	res, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "grep",
		Args: json.RawMessage(`{"pattern":"FIND_ME.*"}`),
	})
	if err != nil || !res.OK {
		t.Fatalf("grep: %+v %v", res, err)
	}
	var got struct {
		Matches   []string `json:"matches"`
		Count     int      `json:"count"`
		Total     int      `json:"total"`
		Truncated bool     `json:"truncated"`
		Notice    string   `json:"notice"`
	}
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Matches) != 2 || got.Truncated {
		t.Fatalf("expected 2 matches without truncation, got %+v", got)
	}
	if got.Matches[0] != "notes.txt:2:FIND_ME in txt with [literal](test)" || got.Matches[1] != "pkg/a.go:2:// FIND_ME in go" {
		t.Fatalf("unexpected matches: %v", got.Matches)
	}

	// 2. Glob filter restricts search to *.go.
	globRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "grep",
		Args: json.RawMessage(`{"pattern":"FIND_ME","glob":"*.go"}`),
	})
	if err != nil || !globRes.OK {
		t.Fatalf("grep glob: %+v %v", globRes, err)
	}
	if err := json.Unmarshal(globRes.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Matches) != 1 || got.Matches[0] != "pkg/a.go:2:// FIND_ME in go" {
		t.Fatalf("unexpected glob matches: %v", got.Matches)
	}

	// 3. Literal search treats regex metacharacters as plain text.
	litRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "grep",
		Args: json.RawMessage(`{"pattern":"[literal](test)","literal":true}`),
	})
	if err != nil || !litRes.OK {
		t.Fatalf("grep literal: %+v %v", litRes, err)
	}
	if err := json.Unmarshal(litRes.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Matches) != 1 || got.Matches[0] != "notes.txt:2:FIND_ME in txt with [literal](test)" {
		t.Fatalf("unexpected literal matches: %v", got.Matches)
	}

	// 4. Max-hits cap and truncation notice.
	var manyLines strings.Builder
	for i := 1; i <= 80; i++ {
		fmt.Fprintf(&manyLines, "REPEAT_HIT line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(root, "many.txt"), []byte(manyLines.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	capRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "grep",
		Args: json.RawMessage(`{"pattern":"REPEAT_HIT"}`),
	})
	if err != nil || !capRes.OK {
		t.Fatalf("grep cap: %+v %v", capRes, err)
	}
	if err := json.Unmarshal(capRes.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Matches) != defaultGrepMaxHits || got.Total != 80 || !got.Truncated || !strings.Contains(got.Notice, "truncated") {
		t.Fatalf("expected capped grep result (%d of 80), got %+v", defaultGrepMaxHits, got)
	}
}

func TestListFilesSkipsGitDirs(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("README.md")
	mustWrite("internal/app.go")
	mustWrite("internal/sub/util.go")
	mustWrite(".git/HEAD")
	mustWrite(".git/objects/pack/pack-1.idx")
	mustWrite(".cloud-harness/harness.db")

	charge := &memCharge{b: domain.Budget{MaxToolCalls: 20}}
	grants := &memGrants{grants: []domain.Grant{
		{ID: "g", Tool: "list_files", Op: "call", State: "active", RunID: "run", ResourceScope: root},
	}}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}

	res, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "list_files",
		Args: json.RawMessage(`{}`),
	})
	if err != nil || !res.OK {
		t.Fatalf("list_files: %+v %v", res, err)
	}
	var got struct {
		Files     []string `json:"files"`
		Count     int      `json:"count"`
		Total     int      `json:"total"`
		Truncated bool     `json:"truncated"`
		Notice    string   `json:"notice"`
	}
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"README.md", "internal/app.go", "internal/sub/util.go"}
	if strings.Join(got.Files, ",") != strings.Join(want, ",") || got.Truncated {
		t.Fatalf("list_files = %v (truncated=%v), want %v", got.Files, got.Truncated, want)
	}
	for _, f := range got.Files {
		if strings.Contains(f, ".git") || strings.Contains(f, ".cloud-harness") {
			t.Fatalf("list_files leaked ignored dir: %s", f)
		}
	}

	// Test glob pattern filtering.
	globRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "list_files",
		Args: json.RawMessage(`{"pattern":"*.go"}`),
	})
	if err != nil || !globRes.OK {
		t.Fatalf("list_files glob: %+v %v", globRes, err)
	}
	if err := json.Unmarshal(globRes.Data, &got); err != nil {
		t.Fatal(err)
	}
	wantGo := []string{"internal/app.go", "internal/sub/util.go"}
	if strings.Join(got.Files, ",") != strings.Join(wantGo, ",") {
		t.Fatalf("list_files(*.go) = %v, want %v", got.Files, wantGo)
	}

	// Test result cap and truncation notice.
	for i := 0; i < defaultListFiles+10; i++ {
		mustWrite(fmt.Sprintf("bulk/file_%03d.txt", i))
	}
	capRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "list_files",
		Args: json.RawMessage(`{"pattern":"bulk/*.txt"}`),
	})
	if err != nil || !capRes.OK {
		t.Fatalf("list_files cap: %+v %v", capRes, err)
	}
	if err := json.Unmarshal(capRes.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != defaultListFiles || got.Total != defaultListFiles+10 || !got.Truncated || !strings.Contains(got.Notice, "truncated") {
		t.Fatalf("expected capped list_files (%d of %d), got %+v", defaultListFiles, defaultListFiles+10, got)
	}
}

func TestListDirEntries(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "hello")
	write("sub/child.go", "package sub")
	write(".git/config", "git")
	write(".cloud-harness/policy.json", "{}")

	charge := &memCharge{b: domain.Budget{MaxToolCalls: 20}}
	grants := &memGrants{grants: []domain.Grant{
		{ID: "g", Tool: "list_dir", Op: "call", State: "active", RunID: "run", ResourceScope: root},
	}}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}

	res, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "list_dir", Args: json.RawMessage(`{}`)})
	if err != nil || !res.OK {
		t.Fatalf("list_dir: %+v %v", res, err)
	}
	var got struct {
		Path      string `json:"path"`
		Count     int    `json:"count"`
		Total     int    `json:"total"`
		Truncated bool   `json:"truncated"`
		Entries   []struct {
			Name string `json:"name"`
			Path string `json:"path"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Path != "." || got.Count != 2 || got.Total != 2 || got.Truncated {
		t.Fatalf("unexpected list_dir root result: %+v", got)
	}
	if got.Entries[0].Name != "a.txt" || got.Entries[0].Type != "file" || got.Entries[0].Size != 5 || got.Entries[0].Path != "a.txt" {
		t.Fatalf("unexpected file entry: %+v", got.Entries[0])
	}
	if got.Entries[1].Name != "sub" || got.Entries[1].Type != "directory" || got.Entries[1].Path != "sub" {
		t.Fatalf("unexpected directory entry: %+v", got.Entries[1])
	}
	for _, e := range got.Entries {
		if e.Name == ".git" || e.Name == ".cloud-harness" {
			t.Fatalf("list_dir leaked ignored entry: %+v", e)
		}
	}

	subRes, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "list_dir", Args: json.RawMessage(`{"path":"sub"}`)})
	if err != nil || !subRes.OK {
		t.Fatalf("list_dir sub: %+v %v", subRes, err)
	}
	got = struct {
		Path      string `json:"path"`
		Count     int    `json:"count"`
		Total     int    `json:"total"`
		Truncated bool   `json:"truncated"`
		Entries   []struct {
			Name string `json:"name"`
			Path string `json:"path"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"entries"`
	}{}
	if err := json.Unmarshal(subRes.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Path != "sub" || got.Count != 1 || got.Entries[0].Path != "sub/child.go" || got.Entries[0].Type != "file" {
		t.Fatalf("unexpected list_dir sub result: %+v", got)
	}

	controlRes, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "list_dir", Args: json.RawMessage(`{"path":".cloud-harness"}`)})
	if err != nil || controlRes.OK || controlRes.Error == nil || controlRes.Error.Code != domain.CodeAuthDenied {
		t.Fatalf("list_dir control path should be denied: %+v %v", controlRes, err)
	}

	write(" reports/inside.txt", "x")
	spaceRes, err := b.Invoke(context.Background(), Request{RunID: "run", Tool: "list_dir", Args: json.RawMessage(`{"path":" reports"}`)})
	if err != nil || !spaceRes.OK {
		t.Fatalf("list_dir whitespace path: %+v %v", spaceRes, err)
	}
	var spaceGot struct {
		Path    string `json:"path"`
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(spaceRes.Data, &spaceGot); err != nil {
		t.Fatal(err)
	}
	if spaceGot.Path != " reports" || len(spaceGot.Entries) != 1 || spaceGot.Entries[0].Name != "inside.txt" {
		t.Fatalf("unexpected list_dir whitespace result: %+v", spaceGot)
	}
}

func TestRunCommandEnvWhitelist(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HARNESS_SECRET_API_KEY_SENTINEL", "sk-secret-must-not-leak-999")
	t.Setenv("HARNESS_EXTRA_ALLOWED_VAR", "allowed-extra-val-42")

	charge := &memCharge{b: domain.Budget{MaxToolCalls: 20}}
	grants := &memGrants{grants: []domain.Grant{
		{ID: "g_env_1", Tool: "run_command", Op: "call", State: "active", RunID: "run", InvocationID: "inv_env_1", ResourceScope: "env"},
		{ID: "g_env_2", Tool: "run_command", Op: "call", State: "active", RunID: "run", InvocationID: "inv_env_2", ResourceScope: "env"},
	}}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}

	// 1. Default whitelist: PATH is present, sentinel and unconfigured extra var are NOT present.
	res, err := b.Invoke(context.Background(), Request{
		RunID: "run", InvocationID: "inv_env_1", Tool: "run_command",
		Args: json.RawMessage(`{"command":"env"}`),
	})
	if err != nil || !res.OK {
		t.Fatalf("run_command env failed: %+v %v", res, err)
	}
	var got struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(res.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", got.ExitCode)
	}
	if !strings.Contains(got.Stdout, "PATH=") {
		t.Fatalf("expected PATH in env output, got:\n%s", got.Stdout)
	}
	if strings.Contains(got.Stdout, "HARNESS_SECRET_API_KEY_SENTINEL") || strings.Contains(got.Stdout, "sk-secret-must-not-leak-999") {
		t.Fatalf("sentinel env var leaked into child process:\n%s", got.Stdout)
	}
	if strings.Contains(got.Stdout, "HARNESS_EXTRA_ALLOWED_VAR") {
		t.Fatalf("unallowed extra env var leaked into child process:\n%s", got.Stdout)
	}

	// 2. With command_env_allow configured: allowed extra var is passed, sentinel is still excluded.
	b.SetCommandEnvAllow([]string{"HARNESS_EXTRA_ALLOWED_VAR"})
	res2, err := b.Invoke(context.Background(), Request{
		RunID: "run", InvocationID: "inv_env_2", Tool: "run_command",
		Args: json.RawMessage(`{"command":"env"}`),
	})
	if err != nil || !res2.OK {
		t.Fatalf("run_command env (with allowlist) failed: %+v %v", res2, err)
	}
	if err := json.Unmarshal(res2.Data, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Stdout, "PATH=") {
		t.Fatalf("expected PATH in env output, got:\n%s", got.Stdout)
	}
	if !strings.Contains(got.Stdout, "HARNESS_EXTRA_ALLOWED_VAR=allowed-extra-val-42") {
		t.Fatalf("expected HARNESS_EXTRA_ALLOWED_VAR in env output, got:\n%s", got.Stdout)
	}
	if strings.Contains(got.Stdout, "HARNESS_SECRET_API_KEY_SENTINEL") || strings.Contains(got.Stdout, "sk-secret-must-not-leak-999") {
		t.Fatalf("sentinel env var leaked into child process:\n%s", got.Stdout)
	}
}

func TestRunCommandCwdEscapeDenied(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	insideSub := filepath.Join(root, "subdir")
	if err := os.MkdirAll(insideSub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "symlink_outside")); err != nil {
		t.Fatal(err)
	}

	charge := &memCharge{b: domain.Budget{MaxToolCalls: 20}}
	grants := &memGrants{grants: []domain.Grant{
		{ID: "g", Tool: "run_command", Op: "call", State: "active", RunID: "run", ResourceScope: root},
	}}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}

	escapes := []string{
		"..",
		"../" + filepath.Base(outside),
		outside,
		"symlink_outside",
	}
	for _, badCwd := range escapes {
		args := json.RawMessage(fmt.Sprintf(`{"command":"pwd","cwd":%q}`, badCwd))
		authRes, err := b.Authorize(context.Background(), Request{
			RunID: "run", Tool: "run_command", ApprovalPossible: true, Args: args,
		})
		if err != nil || authRes.OK || authRes.NeedsApproval || authRes.Error == nil || authRes.Error.Code != domain.CodeAuthDenied {
			t.Fatalf("Authorize cwd=%q should be denied with auth_denied, got %+v %v", badCwd, authRes, err)
		}
		invRes, err := b.Invoke(context.Background(), Request{
			RunID: "run", Tool: "run_command", Args: args,
		})
		if err != nil || invRes.OK || invRes.Dispatched || invRes.Error == nil || invRes.Error.Code != domain.CodeAuthDenied {
			t.Fatalf("Invoke cwd=%q should be denied with auth_denied without dispatch, got %+v %v", badCwd, invRes, err)
		}
	}

	// Valid subdirectory inside workspace succeeds and runs in that directory.
	okRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "run_command",
		Args: json.RawMessage(`{"command":"pwd","cwd":"subdir"}`),
	})
	if err != nil || !okRes.OK || !okRes.Dispatched {
		t.Fatalf("valid cwd Invoke failed: %+v %v", okRes, err)
	}
	var got struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(okRes.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 0 || !strings.HasSuffix(strings.TrimSpace(got.Stdout), "subdir") {
		t.Fatalf("unexpected pwd output for subdir: %+v", got)
	}
}

func TestRunCommandRejectsControlPathArguments(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cloud-harness"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"ls .cloud-harness", "wc .cloud-harness/harness.db"} {
		if _, _, err := parseCommand(root, command); err == nil {
			t.Fatalf("%s was allowed", command)
		}
	}
	if _, dangerous, err := parseCommand(root, "ls ."); err != nil || dangerous {
		t.Fatalf("ordinary safe command changed: dangerous=%v err=%v", dangerous, err)
	}
}

func TestRunCommandRejectsControlWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cloud-harness", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := NewBroker(&memCharge{b: domain.Budget{MaxToolCalls: 1}}, &memGrants{})
	specs, err := LocalSpecs(b, root)
	if err != nil {
		t.Fatal(err)
	}
	var spec Spec
	for _, s := range specs {
		if s.Name == "run_command" {
			spec = s
			break
		}
	}
	if spec.Execute == nil {
		t.Fatal("run_command spec missing")
	}
	for _, cwd := range []string{".cloud-harness", ".cloud-harness/sub"} {
		if _, err := spec.Execute(context.Background(), json.RawMessage(`{"command":"ls","cwd":"`+cwd+`"}`)); err == nil {
			t.Fatalf("cwd %s was allowed", cwd)
		}
	}
}

func TestRunCommandReportsExitCode(t *testing.T) {
	root := t.TempDir()
	binDir := t.TempDir()

	helperPath := filepath.Join(binDir, "exit3-helper")
	if err := os.WriteFile(helperPath, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bigHelperPath := filepath.Join(binDir, "bigout-helper")
	if err := os.WriteFile(bigHelperPath, []byte("#!/bin/sh\ndd if=/dev/zero bs=1024 count=1100 2>/dev/null | tr '\\0' 'A'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	shExitCmd := "sh -c 'exit 3'"
	helperCmd := "exit3-helper"
	bigCmd := "bigout-helper"

	charge := &memCharge{b: domain.Budget{MaxToolCalls: 20}}
	grants := &memGrants{grants: []domain.Grant{
		{ID: "g_root", Tool: "run_command", Op: "call", State: "active", RunID: "run", ResourceScope: root},
		{ID: "g_sh", Tool: "run_command", Op: "call", State: "active", RunID: "run", InvocationID: "inv_sh", ResourceScope: shExitCmd},
		{ID: "g_helper", Tool: "run_command", Op: "call", State: "active", RunID: "run", InvocationID: "inv_helper", ResourceScope: helperCmd},
		{ID: "g_big", Tool: "run_command", Op: "call", State: "active", RunID: "run", InvocationID: "inv_big", ResourceScope: bigCmd},
	}}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}

	// 1. sh -c 'exit 3' reports exit_code 3.
	shRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", InvocationID: "inv_sh", Tool: "run_command",
		Args: json.RawMessage(fmt.Sprintf(`{"command":%q}`, shExitCmd)),
	})
	if err != nil || !shRes.Dispatched || len(shRes.Data) == 0 {
		t.Fatalf("sh exit 3 invoke failed: %+v %v", shRes, err)
	}
	var got struct {
		ExitCode  int    `json:"exit_code"`
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		Truncated bool   `json:"truncated"`
		Notice    string `json:"notice"`
	}
	if err := json.Unmarshal(shRes.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 3 {
		t.Fatalf("sh -c 'exit 3' exit_code = %d, want 3 (data=%s)", got.ExitCode, shRes.Data)
	}

	// 2. Helper binary exiting 3 also reports exit_code 3.
	helperRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", InvocationID: "inv_helper", Tool: "run_command",
		Args: json.RawMessage(fmt.Sprintf(`{"command":%q}`, helperCmd)),
	})
	if err != nil || !helperRes.Dispatched || len(helperRes.Data) == 0 {
		t.Fatalf("helper exit 3 invoke failed: %+v %v", helperRes, err)
	}
	if err := json.Unmarshal(helperRes.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 3 {
		t.Fatalf("helper exit_code = %d, want 3 (data=%s)", got.ExitCode, helperRes.Data)
	}

	// 3. Output exceeding 1MiB cap is truncated with explicit truncation marker.
	bigRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", InvocationID: "inv_big", Tool: "run_command",
		Args: json.RawMessage(fmt.Sprintf(`{"command":%q}`, bigCmd)),
	})
	if err != nil || !bigRes.OK {
		t.Fatalf("big output command failed: %+v %v", bigRes, err)
	}
	if err := json.Unmarshal(bigRes.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 0 || !got.Truncated || !strings.Contains(got.Stdout, "truncated") || !strings.Contains(got.Notice, "truncated") {
		t.Fatalf("expected explicit truncation marker for >1MiB output, got exit_code=%d truncated=%v notice=%q len(stdout)=%d", got.ExitCode, got.Truncated, got.Notice, len(got.Stdout))
	}
}

func TestGitToolsOnTempRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not installed")
	}
	root := t.TempDir()
	outside := t.TempDir()

	runGitSetup := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	runGitSetup("init")
	runGitSetup("config", "user.email", "test@example.com")
	runGitSetup("config", "user.name", "Test User")
	runGitSetup("config", "commit.gpgsign", "false")

	tracked := filepath.Join(root, "hello.txt")
	if err := os.WriteFile(tracked, []byte("line one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitSetup("add", "hello.txt")
	runGitSetup("commit", "-m", "initial commit message")

	// Modify tracked file after committing.
	if err := os.WriteFile(tracked, []byte("line one\nline two modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	charge := &memCharge{b: domain.Budget{MaxToolCalls: 30}}
	grants := &memGrants{grants: []domain.Grant{
		{ID: "g1", Tool: "git_status", Op: "call", State: "active", RunID: "run", ResourceScope: root},
		{ID: "g2", Tool: "git_diff", Op: "call", State: "active", RunID: "run", ResourceScope: root},
		{ID: "g3", Tool: "git_log", Op: "call", State: "active", RunID: "run", ResourceScope: root},
	}}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}

	// 1. git_status shows the modification to hello.txt.
	statusRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "git_status", Args: json.RawMessage(`{}`),
	})
	if err != nil || !statusRes.OK {
		t.Fatalf("git_status failed: %+v %v", statusRes, err)
	}
	var gotStatus struct {
		Status string `json:"status"`
		Clean  bool   `json:"clean"`
	}
	if err := json.Unmarshal(statusRes.Data, &gotStatus); err != nil {
		t.Fatal(err)
	}
	if gotStatus.Clean || !strings.Contains(gotStatus.Status, "hello.txt") || !strings.Contains(gotStatus.Status, "M") {
		t.Fatalf("git_status did not report modified hello.txt: %+v", gotStatus)
	}

	// 2. git_diff shows the unstaged change, and optional path filter works.
	diffRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "git_diff", Args: json.RawMessage(`{"path":"hello.txt"}`),
	})
	if err != nil || !diffRes.OK {
		t.Fatalf("git_diff failed: %+v %v", diffRes, err)
	}
	var gotDiff struct {
		Diff   string `json:"diff"`
		Staged bool   `json:"staged"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(diffRes.Data, &gotDiff); err != nil {
		t.Fatal(err)
	}
	if gotDiff.Staged || gotDiff.Path != "hello.txt" || !strings.Contains(gotDiff.Diff, "+line two modified") {
		t.Fatalf("git_diff did not show expected diff: %+v", gotDiff)
	}

	// git_diff with escaping path must be denied with auth_denied.
	escapeDiff, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "git_diff",
		Args: json.RawMessage(fmt.Sprintf(`{"path":%q}`, filepath.Join(outside, "x.txt"))),
	})
	if err != nil || escapeDiff.OK || escapeDiff.Dispatched || escapeDiff.Error == nil || escapeDiff.Error.Code != domain.CodeAuthDenied {
		t.Fatalf("git_diff with outside path should be denied: %+v %v", escapeDiff, err)
	}

	// 3. git_log shows the commit.
	logRes, err := b.Invoke(context.Background(), Request{
		RunID: "run", Tool: "git_log", Args: json.RawMessage(`{"limit":5}`),
	})
	if err != nil || !logRes.OK {
		t.Fatalf("git_log failed: %+v %v", logRes, err)
	}
	var gotLog struct {
		Log   string `json:"log"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(logRes.Data, &gotLog); err != nil {
		t.Fatal(err)
	}
	if gotLog.Limit != 5 || !strings.Contains(gotLog.Log, "initial commit message") {
		t.Fatalf("git_log did not show expected commit: %+v", gotLog)
	}
}

func TestGitToolsOutsideRepoError(t *testing.T) {
	root := t.TempDir() // Not a git repository.
	charge := &memCharge{b: domain.Budget{MaxToolCalls: 20}}
	grants := &memGrants{grants: []domain.Grant{
		{ID: "g1", Tool: "git_status", Op: "call", State: "active", RunID: "run", ResourceScope: root},
		{ID: "g2", Tool: "git_diff", Op: "call", State: "active", RunID: "run", ResourceScope: root},
		{ID: "g3", Tool: "git_log", Op: "call", State: "active", RunID: "run", ResourceScope: root},
	}}
	b := NewBroker(charge, grants)
	if err := RegisterLocal(b, root); err != nil {
		t.Fatal(err)
	}

	for _, toolName := range []string{"git_status", "git_diff", "git_log"} {
		res, err := b.Invoke(context.Background(), Request{
			RunID: "run", Tool: toolName, Args: json.RawMessage(`{}`),
		})
		if err != nil {
			t.Fatalf("%s returned unexpected Go error: %v", toolName, err)
		}
		if res.OK || res.Error == nil || res.Error.Code != domain.CodeToolError {
			t.Fatalf("%s outside git repo should return clean tool_error, got %+v", toolName, res)
		}
	}
}
