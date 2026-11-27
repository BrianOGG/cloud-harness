package modelconfig

import "strings"

// ModelCategory leaves unfamiliar/manual IDs in the primary conversation list.
func ModelCategory(id, name string) string {
	text := strings.ToLower(id + " " + name)
	for _, hint := range []string{"tts", "asr", "realtime", "real-time", "whisper", "embedding", "embed-", "image", "video", "router", "rerank", "transcrib", "speech"} {
		if strings.Contains(text, hint) {
			return "other"
		}
	}
	return "chat"
}
