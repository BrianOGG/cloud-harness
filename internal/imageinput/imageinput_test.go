package imageinput

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/gif"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/provider"
)

func TestParseAndLoadSingleMultipleQuotedAndAbsolutePaths(t *testing.T) {
	workspace := t.TempDir()
	one := filepath.Join(workspace, "one.txt")
	two := filepath.Join(workspace, "my shot.png")
	writeImage(t, one, testPNG())
	writeImage(t, two, testPNG())

	text, images, err := ParseAndLoad("Inspect @image:one.txt and @image:\"my shot.png\"", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Inspect and" || len(images) != 2 {
		t.Fatalf("text=%q images=%d", text, len(images))
	}
	if images[0].Path != one || images[1].Path != two {
		t.Fatalf("resolved paths: %+v", images)
	}
	for _, image := range images {
		if image.MediaType != "image/png" || image.Width != 1 || image.Height != 1 || image.SHA256 == "" || len(image.Data) == 0 {
			t.Fatalf("image metadata: %+v", image)
		}
	}

	text, images, err = ParseAndLoad("@image:"+one, workspace)
	if err != nil || text != "" || len(images) != 1 || images[0].Path != one {
		t.Fatalf("absolute path text=%q images=%+v err=%v", text, images, err)
	}
}

func TestParseImageTokensOutsideCodeAndEscaping(t *testing.T) {
	input := "literal `@image:missing.png` and\n```text\n@image:\n```\n\\@image:also-literal.png"
	text, paths, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	want := "literal `@image:missing.png` and\n```text\n@image:\n```\n@image:also-literal.png"
	if text != want || len(paths) != 0 {
		t.Fatalf("text=%q paths=%q", text, paths)
	}
}

func TestParseCodeAndQuotedEscapeRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		text  string
		paths []string
	}{
		{"plain unchanged", " keep  all \twhitespace\n", " keep  all \twhitespace\n", nil},
		{"quoted escapes", `see @image:"a\"b\\c.png" now`, "see now", []string{`a"b\c.png`}},
		{"backslash literal", `@image:C:\shots\one.png`, "", []string{`C:\shots\one.png`}},
		{"inline double delimiter", "``example ` @image:missing`` @image:one.png", "``example ` @image:missing``", []string{"one.png"}},
		{"tilde fenced", "~~~md\n@image:\n~~~\n@image:one.png", "~~~md\n@image:\n~~~", []string{"one.png"}},
		{"unclosed fenced", "```\n@image:", "```\n@image:", nil},
		{"fence longer close", "````\n@image:\n```\n@image:missing\n`````\n@image:one.png", "````\n@image:\n```\n@image:missing\n`````", []string{"one.png"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, paths, err := Parse(tc.input)
			if err != nil || text != tc.text || strings.Join(paths, "\x00") != strings.Join(tc.paths, "\x00") {
				t.Fatalf("text=%q paths=%q err=%v", text, paths, err)
			}
		})
	}
}

func TestParseMissingAndUnterminatedPaths(t *testing.T) {
	for _, input := range []string{"look @image:", "@image: \nnext", "@image:\u3000", `@image:""`, `@image:"shot.png`} {
		t.Run(input, func(t *testing.T) {
			if _, _, err := Parse(input); err == nil {
				t.Fatalf("Parse(%q) succeeded", input)
			}
		})
	}
}

func TestLoadFormatsUseMagicAndReadDimensions(t *testing.T) {
	workspace := t.TempDir()
	raster := image.NewRGBA(image.Rect(0, 0, 3, 2))
	var jpg, gifData bytes.Buffer
	if err := jpeg.Encode(&jpg, raster, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifData, raster, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mime   string
		data   []byte
		width  int
		height int
	}{
		{"png", "image/png", testPNG(), 1, 1},
		{"jpg", "image/jpeg", jpg.Bytes(), 3, 2},
		{"jpeg", "image/jpeg", jpg.Bytes(), 3, 2},
		{"gif", "image/gif", gifData.Bytes(), 3, 2},
		{"webp lossy", "image/webp", testWebP("VP8 ", []byte{0, 0, 0, 0x9d, 0x01, 0x2a, 3, 0, 2, 0}), 3, 2},
		{"webp lossless", "image/webp", testWebP("VP8L", []byte{0x2f, 2, 0x40, 0, 0}), 3, 2},
		{"webp extended", "image/webp", testWebP("VP8X", []byte{0, 0, 0, 0, 2, 0, 0, 1, 0, 0}), 3, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The misleading suffix proves decoding does not rely on extensions.
			path := filepath.Join(workspace, tc.name+".txt")
			writeImage(t, path, tc.data)
			images, err := Load([]string{path}, workspace)
			if err != nil || len(images) != 1 {
				t.Fatalf("images=%+v err=%v", images, err)
			}
			if images[0].MediaType != tc.mime || images[0].Width != tc.width || images[0].Height != tc.height || images[0].SizeBytes != len(tc.data) {
				t.Fatalf("metadata=%+v", images[0])
			}
		})
	}
}

func TestLoadAcceptsExactSizeLimits(t *testing.T) {
	workspace := t.TempDir()
	data := make([]byte, provider.MaxImageBytes)
	copy(data, testPNG())
	writeImage(t, filepath.Join(workspace, "ten.png"), data)
	images, err := Load([]string{"ten.png", "ten.png"}, workspace)
	if err != nil || len(images) != 2 {
		t.Fatalf("exact 10/20 MB limits rejected: images=%d err=%v", len(images), err)
	}
}

func TestLoadRejectsMissingOversizedAndUnsupportedImages(t *testing.T) {
	workspace := t.TempDir()
	if _, _, err := ParseAndLoad("@image:absent.png", workspace); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("missing image error: %v", err)
	}
	tooLarge := append(testPNG(), make([]byte, provider.MaxImageBytes+1-len(testPNG()))...)
	writeImage(t, filepath.Join(workspace, "too-large.png"), tooLarge)
	if _, _, err := ParseAndLoad("@image:too-large.png", workspace); err == nil || !strings.Contains(err.Error(), "10 MB") {
		t.Fatalf("oversize error: %v", err)
	}
	writeImage(t, filepath.Join(workspace, "not-image.png"), []byte("plain text"))
	if _, _, err := ParseAndLoad("@image:not-image.png", workspace); err == nil || !strings.Contains(err.Error(), "file signature") {
		t.Fatalf("unsupported image error: %v", err)
	}
}

func TestLoadRejectsAggregateOversize(t *testing.T) {
	workspace := t.TempDir()
	payload := append(testPNG(), make([]byte, 7<<20-len(testPNG()))...)
	paths := []string{"one.png", "two.png", "three.png"}
	for _, path := range paths {
		writeImage(t, filepath.Join(workspace, path), payload)
	}
	_, _, err := ParseAndLoad("@image:one.png @image:two.png @image:three.png", workspace)
	if err == nil || !strings.Contains(err.Error(), "20 MB") {
		t.Fatalf("aggregate-size error: %v", err)
	}
}

func TestReloadDetectsChangedReference(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "shot.png")
	writeImage(t, path, testPNG())
	_, images, err := ParseAndLoad("@image:shot.png", workspace)
	if err != nil {
		t.Fatal(err)
	}
	changed := append(testPNG(), 0)
	writeImage(t, path, changed)
	if _, err := Reload(images); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed reference error: %v", err)
	}
}

func testPNG() []byte {
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC")
	if err != nil {
		panic(err)
	}
	return data
}

func writeImage(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func testWebP(chunk string, body []byte) []byte {
	data := make([]byte, 20+len(body)+(len(body)&1))
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WEBP")
	copy(data[12:], chunk)
	binary.LittleEndian.PutUint32(data[16:], uint32(len(body)))
	copy(data[20:], body)
	return data
}
