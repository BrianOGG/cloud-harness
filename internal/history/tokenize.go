// Package history builds the tokens shared by the indexer and the query.
// SQLite FTS5 unicode61 does not segment Chinese, and the trigram tokenizer
// cannot match two-character words. Callers pre-tokenize with Analyze and
// store the atomic tokens in FTS5. Paths, filenames, and ids are also stored
// for exact lookup because unicode61 splits on '_', '.' and '/'.
package history

import (
	"strings"
	"unicode"
)

// ExactKind classifies a term that must match as a whole value.
type ExactKind string

const (
	ExactPath  ExactKind = "path"
	ExactFile  ExactKind = "file"
	ExactIdent ExactKind = "ident"
	ExactID    ExactKind = "id"
)

// ExactTerm is stored beside the FTS index and compared with equality.
type ExactTerm struct {
	Term string
	Kind ExactKind
}

// Analysis is the index-time and query-time view of one piece of text.
type Analysis struct {
	FTSTokens []string
	Exact     []ExactTerm
}

// QueryPlan is how Search should use an analysis. ExactOnly queries never
// fall back to a loose FTS AND, so "server.go" does not match a document
// that merely contains the words "server" and "go".
type QueryPlan struct {
	Exact     []string
	FTSTokens []string
	FTSMatch  string
	ExactOnly bool
}

// Analyze tokenizes text. Index and query must call the same function.
func Analyze(text string) Analysis {
	var a Analysis
	seenTok := map[string]struct{}{}
	seenEx := map[string]struct{}{}
	addFTS := func(tok string) {
		if tok == "" || !atomic(tok) {
			return
		}
		if _, ok := seenTok[tok]; ok {
			return
		}
		seenTok[tok] = struct{}{}
		a.FTSTokens = append(a.FTSTokens, tok)
	}
	addExact := func(term string, kind ExactKind) {
		term = strings.TrimSpace(term)
		if term == "" {
			return
		}
		key := string(kind) + "\x00" + term
		if _, ok := seenEx[key]; ok {
			return
		}
		seenEx[key] = struct{}{}
		a.Exact = append(a.Exact, ExactTerm{Term: term, Kind: kind})
	}
	addWord := func(word string) {
		if word == "" {
			return
		}
		switch {
		case strings.ContainsAny(word, `/\`):
			addExact(word, ExactPath)
			for _, seg := range splitAny(word, `/\`) {
				addLeaf(seg, addFTS, addExact)
			}
		default:
			addLeaf(word, addFTS, addExact)
		}
	}

	var han []rune
	var word []rune
	flushHan := func() {
		if len(han) == 1 {
			addFTS(string(han))
		}
		for i := 0; i+1 < len(han); i++ {
			addFTS(string(han[i : i+2]))
		}
		han = han[:0]
	}
	flushWord := func() {
		if len(word) > 0 {
			addWord(string(word))
			word = word[:0]
		}
	}
	for _, r := range text {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			han = append(han, r)
		case isWordRune(r):
			flushHan()
			word = append(word, r)
		default:
			flushHan()
			flushWord()
		}
	}
	flushHan()
	flushWord()
	return a
}

func addLeaf(word string, addFTS func(string), addExact func(string, ExactKind)) {
	if word == "" {
		return
	}
	switch {
	case strings.Contains(word, "."):
		addExact(word, ExactFile)
		for _, part := range strings.Split(word, ".") {
			addIdent(part, addFTS, addExact)
		}
	case strings.Contains(word, "_"):
		addExact(word, ExactID)
		for _, part := range strings.Split(word, "_") {
			addIdent(part, addFTS, addExact)
		}
	default:
		addIdent(word, addFTS, addExact)
	}
}

func addIdent(word string, addFTS func(string), addExact func(string, ExactKind)) {
	if word == "" {
		return
	}
	if allDigits(word) {
		addExact(word, ExactID)
		addFTS(word)
		return
	}
	addFTS(word)
	lower := strings.ToLower(word)
	if lower != word {
		addFTS(lower)
	}
	parts := splitCamel(word)
	if len(parts) > 1 {
		addExact(word, ExactIdent)
		for _, p := range parts {
			addFTS(p)
			pl := strings.ToLower(p)
			if pl != p {
				addFTS(pl)
			}
		}
		for i := 0; i+1 < len(parts); i++ {
			join := parts[i] + parts[i+1]
			addFTS(join)
			jl := strings.ToLower(join)
			if jl != join {
				addFTS(jl)
			}
		}
	}
}

// PlanQuery selects exact equality, FTS, or both.
func PlanQuery(query string) QueryPlan {
	query = strings.TrimSpace(query)
	if query == "" {
		return QueryPlan{}
	}
	a := Analyze(query)
	plan := QueryPlan{
		FTSTokens: a.FTSTokens,
		FTSMatch:  Match(a.FTSTokens),
		ExactOnly: strings.ContainsAny(query, "/\\._"),
	}
	if plan.ExactOnly {
		plan.Exact = []string{query}
		for _, e := range a.Exact {
			if e.Term == query {
				plan.Exact = append(plan.Exact, e.Term)
			}
		}
		plan.Exact = unique(plan.Exact)
		plan.FTSTokens = nil
		plan.FTSMatch = ""
		return plan
	}
	plan.Exact = []string{query}
	for _, e := range a.Exact {
		plan.Exact = append(plan.Exact, e.Term)
	}
	plan.Exact = unique(plan.Exact)
	return plan
}

// Match builds an FTS5 MATCH expression that ANDs quoted tokens.
func Match(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	parts := make([]string, len(tokens))
	for i, tok := range tokens {
		parts[i] = `"` + strings.ReplaceAll(tok, `"`, `""`) + `"`
	}
	return strings.Join(parts, " AND ")
}

func atomic(tok string) bool {
	if tok == "" {
		return false
	}
	for _, r := range tok {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool {
	if r > 127 {
		return false
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '/' || r == '\\' || r == '.' || r == '_' || r == '-' || r == '+' || r == '#'
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func splitAny(s, seps string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return strings.ContainsRune(seps, r)
	})
}

func splitCamel(s string) []string {
	rs := []rune(s)
	if len(rs) == 0 {
		return nil
	}
	var parts []string
	start := 0
	for i := 1; i < len(rs); i++ {
		prev, cur := rs[i-1], rs[i]
		var next rune
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		boundary := false
		if unicode.IsLower(prev) && unicode.IsUpper(cur) {
			boundary = true
		}
		if unicode.IsUpper(prev) && unicode.IsUpper(cur) && next != 0 && unicode.IsLower(next) {
			boundary = true
		}
		if (unicode.IsLetter(prev) && unicode.IsDigit(cur)) || (unicode.IsDigit(prev) && unicode.IsLetter(cur)) {
			boundary = true
		}
		if boundary {
			parts = append(parts, string(rs[start:i]))
			start = i
		}
	}
	parts = append(parts, string(rs[start:]))
	return parts
}

func unique(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
