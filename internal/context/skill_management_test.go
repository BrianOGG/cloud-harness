package context

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

const reviewDoc = "---\r\nname: review\r\nx-author: Local\r\ndescription: 'Review: API'\r\n---\r\n# Review\r\n"

func TestSkillManagementAliasedScopes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(root, "config.json"))
	initial := writeSkillFixture(t, root, "review", "shared directory", "preserve")
	for _, scope := range []string{"project", "global"} {
		doc, err := OpenSkillDocument(root, scope, "review")
		if err != nil || doc.Content != initial {
			t.Fatalf("open %s: %+v %v", scope, doc, err)
		}
		if _, err := SaveSkillDocument(root, scope, "review", reviewDoc, doc.Revision); err == nil {
			t.Fatal("one scoped edit would affect both layers")
		}
		if err := DeleteSkillDocument(root, scope, "review", doc.Revision); err == nil {
			t.Fatal("one scoped delete would affect both layers")
		}
	}
}

func TestSkillManagementRejectsUnsafePaths(t *testing.T) {
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "SKILL.md")
	if err := os.WriteFile(outsideFile, []byte(reviewDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{".agents", ".agents/skills", ".agents/skills/review", ".agents/skills/review/SKILL.md"} {
		t.Run(link, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, link)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			target := outside
			if filepath.Base(path) == skillFile {
				target = outsideFile
			}
			if err := os.Symlink(target, path); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			if _, err := SaveSkillDocument(root, "project", "", reviewDoc, ""); err == nil {
				t.Fatal("create followed link")
			}
			if _, err := OpenSkillDocument(root, "project", "review"); err == nil {
				t.Fatal("open followed link")
			}
			if _, err := SaveSkillDocument(root, "project", "review", reviewDoc+"edit", skillRevision(reviewDoc)); err == nil {
				t.Fatal("update followed link")
			}
			if err := DeleteSkillDocument(root, "project", "review", skillRevision(reviewDoc)); err == nil {
				t.Fatal("delete followed link")
			}
			data, err := os.ReadFile(outsideFile)
			if err != nil || string(data) != reviewDoc {
				t.Fatalf("external file changed: %q %v", data, err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 1 {
				t.Fatalf("external directory changed: %v %v", entries, err)
			}
		})
	}
}

func TestSkillManagementConcurrentCreateAndEdit(t *testing.T) {
	root := t.TempDir()
	update := func(name, revision string) SkillDocument {
		t.Helper()
		type result struct {
			document SkillDocument
			err      error
		}
		const writers = 8
		results := make(chan result, writers)
		start := make(chan struct{})
		for i := range writers {
			go func() {
				<-start
				doc, err := SaveSkillDocument(root, "project", name, reviewDoc+strings.Repeat("x", i+1)+name, revision)
				results <- result{doc, err}
			}()
		}
		close(start)
		var saved SkillDocument
		success := 0
		for range writers {
			r := <-results
			if r.err == nil {
				saved = r.document
				success++
			} else if de := new(domain.Error); !errors.As(r.err, &de) || de.Code != domain.CodeConflict {
				t.Fatalf("unexpected concurrent failure: %v", r.err)
			}
		}
		if success != 1 {
			t.Fatalf("successful writers = %d", success)
		}
		return saved
	}
	initial := update("", "")
	latest := update(initial.Name, initial.Revision)
	actual, err := OpenSkillDocument(root, "project", "review")
	if err != nil || actual != latest {
		t.Fatalf("revision lost: %+v %+v %v", actual, latest, err)
	}
	// A direct file edit between opening the UI and saving invalidates a revision.
	external := reviewDoc + "externally edited\n"
	path := filepath.Join(root, skillDir, "review", skillFile)
	if err := os.WriteFile(path, []byte(external), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveSkillDocument(root, "project", "review", reviewDoc, latest.Revision); err == nil {
		t.Fatal("overwrote external edit")
	}
	if err := DeleteSkillDocument(root, "project", "review", latest.Revision); err == nil {
		t.Fatal("deleted external edit")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != external {
		t.Fatal("external edit was lost")
	}
}

func TestSkillManagementReadOnlyLargeDocument(t *testing.T) {
	root := t.TempDir()
	original := writeSkillFixture(t, root, "review", "large", strings.Repeat("x", MaxSkillBytes))
	doc, err := OpenSkillDocument(root, "project", "review")
	if err != nil || !doc.Truncated || doc.Revision != "" {
		t.Fatalf("invalid read-only preview: %+v %v", doc, err)
	}
	if err := DeleteSkillDocument(root, "project", "review", skillRevision(original)); err == nil {
		t.Fatal("deleted oversized document")
	}
	if _, err := SaveSkillDocument(root, "project", "review", original, "some-revision"); err == nil {
		t.Fatal("accepted oversized write")
	}
}
