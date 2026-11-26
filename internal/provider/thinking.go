package provider

import (
	"regexp"
	"strings"
)

var thinkTagRE = regexp.MustCompile("(?i)</?think>")

// splitThinkingContent handles compatible endpoints that put reasoning in
// content instead of reasoning_content. An orphan closing tag means the
// preceding content was an implicitly opened reasoning block (Agnes).
// An unfinished opening block stays private, including on a cut-off response.
func splitThinkingContent(content string) (text, reasoning string, tagged bool) {
	var visible, hidden strings.Builder
	depth := 0
	for {
		loc := thinkTagRE.FindStringIndex(content)
		if loc == nil {
			if depth > 0 {
				hidden.WriteString(content)
			} else {
				visible.WriteString(content)
			}
			break
		}
		tagged = true
		closing := content[loc[0]+1] == '/'
		if closing && depth == 0 {
			hidden.WriteString(visible.String())
			visible.Reset()
			hidden.WriteString(content[:loc[0]])
		} else if depth > 0 {
			hidden.WriteString(content[:loc[0]])
		} else {
			visible.WriteString(content[:loc[0]])
		}
		if closing {
			if depth > 0 {
				depth--
			}
		} else {
			depth++
		}
		content = content[loc[1]:]
	}
	if !tagged {
		return visible.String(), "", false
	}
	return strings.TrimSpace(visible.String()), strings.TrimSpace(hidden.String()), true
}
