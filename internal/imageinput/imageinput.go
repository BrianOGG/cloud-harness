// Package imageinput parses @image tokens and loads validated image references.
package imageinput

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
)

// Parse removes image tokens and returns their path arguments. Fenced and
// inline Markdown code spans are copied literally without token processing.
func Parse(input string) (string, []string, error) {
	out := make([]byte, 0, len(input))
	var paths []string
	fenceLen := 0
	var fenceChar byte
	lineStart := true

	for i := 0; i < len(input); {
		if lineStart {
			lineEnd := strings.IndexByte(input[i:], '\n')
			if lineEnd < 0 {
				lineEnd = len(input)
			} else {
				lineEnd += i + 1
			}
			if fence, marker, ok := fenceMarker(input[i:lineEnd], fenceLen, fenceChar); ok {
				out = append(out, input[i:lineEnd]...)
				i = lineEnd
				if fenceLen == 0 {
					fenceLen = fence
					fenceChar = marker
				} else {
					fenceLen = 0
				}
				lineStart = true
				continue
			}
			if fenceLen > 0 {
				out = append(out, input[i:lineEnd]...)
				i = lineEnd
				lineStart = true
				continue
			}
		}

		if input[i] == '`' {
			run := backtickRun(input, i)
			if end, ok := inlineCodeEnd(input, i+run, run); ok {
				out = append(out, input[i:end]...)
				i = end
				lineStart = false
				continue
			}
			out = append(out, input[i:i+run]...)
			i += run
			lineStart = false
			continue
		}

		if strings.HasPrefix(input[i:], `\@image:`) {
			out = append(out, "@image:"...)
			i += len(`\@image:`)
			lineStart = false
			continue
		}
		if strings.HasPrefix(input[i:], "@image:") {
			path, end, err := parsePath(input, i+len("@image:"))
			if err != nil {
				return "", nil, err
			}
			paths = append(paths, path)
			leftSpace := len(out) > 0 && (out[len(out)-1] == ' ' || out[len(out)-1] == '\t')
			rightSpace := end < len(input) && (input[end] == ' ' || input[end] == '\t')
			if leftSpace && rightSpace {
				for len(out) > 0 && (out[len(out)-1] == ' ' || out[len(out)-1] == '\t') {
					out = out[:len(out)-1]
				}
				out = append(out, ' ')
				for end < len(input) && (input[end] == ' ' || input[end] == '\t') {
					end++
				}
			}
			i = end
			lineStart = false
			continue
		}

		r, size := utf8.DecodeRuneInString(input[i:])
		out = append(out, input[i:i+size]...)
		i += size
		lineStart = r == '\n'
	}

	text := string(out)
	if len(paths) > 0 {
		text = strings.TrimSpace(text)
	}
	return text, paths, nil
}

// ParseAndLoad extracts tokens and loads every referenced image before a run is
// created, so malformed or unreadable inputs cannot start model work.
func ParseAndLoad(input, workspace string) (string, []provider.Image, error) {
	text, paths, err := Parse(input)
	if err != nil {
		return "", nil, err
	}
	images, err := Load(paths, workspace)
	if err != nil {
		return "", nil, err
	}
	return text, images, nil
}

// Load resolves relative paths from workspace and validates all files and
// aggregate size before returning any image payload.
func Load(paths []string, workspace string) ([]provider.Image, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if workspace == "" {
		workspace = "."
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, domain.E(domain.CodeInvalidArgument, "cannot resolve image workspace: "+err.Error())
	}
	resolved := make([]string, len(paths))
	var total int64
	for i, path := range paths {
		if path == "" {
			return nil, domain.E(domain.CodeInvalidArgument, "@image: requires a non-empty path")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, domain.E(domain.CodeInvalidArgument, "cannot resolve image path: "+err.Error())
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %q cannot be read: %v", path, statErr))
		}
		if !info.Mode().IsRegular() {
			return nil, domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %q is not a regular file", path))
		}
		if info.Size() > provider.MaxImageBytes {
			return nil, domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %q exceeds the 10 MB per-image limit", path))
		}
		if info.Size() <= 0 {
			return nil, domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %q is empty", path))
		}
		total += info.Size()
		if total > provider.MaxImagesTotalBytes {
			return nil, domain.E(domain.CodeInvalidArgument, "images exceed the 20 MB per-turn limit")
		}
		resolved[i] = path
	}

	images := make([]provider.Image, 0, len(paths))
	total = 0
	for _, path := range resolved {
		file, err := os.Open(path)
		if err != nil {
			return nil, domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %q cannot be read: %v", path, err))
		}
		data, readErr := io.ReadAll(io.LimitReader(file, provider.MaxImageBytes+1))
		closeErr := file.Close()
		if readErr != nil {
			return nil, domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %q cannot be read: %v", path, readErr))
		}
		if closeErr != nil {
			return nil, domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %q cannot be closed: %v", path, closeErr))
		}
		if len(data) > provider.MaxImageBytes {
			return nil, domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %q exceeds the 10 MB per-image limit", path))
		}
		total += int64(len(data))
		if total > provider.MaxImagesTotalBytes {
			return nil, domain.E(domain.CodeInvalidArgument, "images exceed the 20 MB per-turn limit")
		}
		mediaType := provider.ImageMediaType(data)
		if mediaType == "" {
			return nil, domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %q has an unsupported or invalid file signature (supported: png, jpg, jpeg, webp, gif)", path))
		}
		width, height, err := dimensions(data, mediaType)
		if err != nil {
			return nil, domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %q is invalid: %v", path, err))
		}
		hash := sha256.Sum256(data)
		images = append(images, provider.Image{
			Path: path, MediaType: mediaType, SizeBytes: len(data), Width: width, Height: height,
			SHA256: hex.EncodeToString(hash[:]), Data: data,
		})
	}
	if err := provider.ValidateImages(images); err != nil {
		return nil, err
	}
	return images, nil
}

// Reload re-reads checkpoint references and rejects files changed since the
// original turn. Image bytes remain out of checkpoint JSON.
func Reload(refs []provider.Image) ([]provider.Image, error) {
	paths := make([]string, len(refs))
	for i, ref := range refs {
		paths[i] = ref.Path
	}
	images, err := Load(paths, "")
	if err != nil {
		return nil, err
	}
	for i, image := range images {
		if refs[i].SHA256 != "" && refs[i].SHA256 != image.SHA256 {
			return nil, domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %q changed since it was attached", refs[i].Path))
		}
	}
	return images, nil
}

func parsePath(input string, start int) (string, int, error) {
	first, _ := utf8.DecodeRuneInString(input[start:])
	if start >= len(input) || unicode.IsSpace(first) {
		return "", start, domain.E(domain.CodeInvalidArgument, "@image: requires a non-empty path")
	}
	if input[start] != '"' {
		end := start
		for end < len(input) {
			r, size := utf8.DecodeRuneInString(input[end:])
			if unicode.IsSpace(r) {
				break
			}
			end += size
		}
		if end == start {
			return "", end, domain.E(domain.CodeInvalidArgument, "@image: requires a non-empty path")
		}
		return input[start:end], end, nil
	}

	var path strings.Builder
	for i := start + 1; i < len(input); i++ {
		switch input[i] {
		case '"':
			if path.Len() == 0 {
				return "", 0, domain.E(domain.CodeInvalidArgument, "@image: requires a non-empty path")
			}
			return path.String(), i + 1, nil
		case '\\':
			if i+1 < len(input) && (input[i+1] == '"' || input[i+1] == '\\') {
				i++
			}
			path.WriteByte(input[i])
		default:
			path.WriteByte(input[i])
		}
	}
	return "", 0, domain.E(domain.CodeInvalidArgument, "@image: has an unterminated quoted path")
}

func fenceMarker(line string, current int, marker byte) (int, byte, bool) {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	start := i
	if i >= len(line) || (line[i] != '`' && line[i] != '~') {
		return 0, 0, false
	}
	char := line[i]
	for i < len(line) && line[i] == char {
		i++
	}
	count := i - start
	if count < 3 {
		return 0, 0, false
	}
	if current == 0 {
		return count, char, true
	}
	if char != marker || count < current || strings.TrimSpace(strings.TrimSuffix(line[i:], "\n")) != "" {
		return 0, 0, false
	}
	return count, char, true
}

func backtickRun(s string, start int) int {
	i := start
	for i < len(s) && s[i] == '`' {
		i++
	}
	return i - start
}

func inlineCodeEnd(s string, start, delimiter int) (int, bool) {
	for i := start; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		run := backtickRun(s, i)
		if run == delimiter {
			return i + run, true
		}
		i += run
	}
	return 0, false
}

func dimensions(data []byte, mediaType string) (int, int, error) {
	if mediaType == "image/webp" {
		return webpDimensions(data)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	want := strings.TrimPrefix(mediaType, "image/")
	if format != want {
		return 0, 0, fmt.Errorf("decoder identified %s, expected %s", format, want)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, fmt.Errorf("image dimensions must be positive")
	}
	return cfg.Width, cfg.Height, nil
}

func webpDimensions(data []byte) (int, int, error) {
	if len(data) < 12 || uint64(binary.LittleEndian.Uint32(data[4:8]))+8 > uint64(len(data)) {
		return 0, 0, fmt.Errorf("truncated WebP RIFF header")
	}
	for offset := 12; offset+8 <= len(data); {
		chunk := string(data[offset : offset+4])
		size := binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		payload := offset + 8
		if uint64(size) > uint64(len(data)-payload) {
			break
		}
		body := data[payload : payload+int(size)]
		switch chunk {
		case "VP8X":
			if len(body) >= 10 {
				width := 1 + int(body[4]) + int(body[5])<<8 + int(body[6])<<16
				height := 1 + int(body[7]) + int(body[8])<<8 + int(body[9])<<16
				return width, height, nil
			}
		case "VP8 ":
			if len(body) >= 10 && bytes.Equal(body[3:6], []byte{0x9d, 0x01, 0x2a}) {
				width := int(binary.LittleEndian.Uint16(body[6:8]) & 0x3fff)
				height := int(binary.LittleEndian.Uint16(body[8:10]) & 0x3fff)
				if width > 0 && height > 0 {
					return width, height, nil
				}
			}
		case "VP8L":
			if len(body) >= 5 && body[0] == 0x2f {
				width := 1 + int(body[1]) + (int(body[2])&0x3f)<<8
				height := 1 + (int(body[2])>>6)&0x03 + int(body[3])<<2 + (int(body[4])&0x0f)<<10
				return width, height, nil
			}
		}
		offset = payload + int(size) + int(size&1)
	}
	return 0, 0, fmt.Errorf("WebP image has no supported dimension header")
}
