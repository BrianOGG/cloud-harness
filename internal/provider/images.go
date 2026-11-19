package provider

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

// ValidateImages checks in-memory image payloads before a provider request.
func ValidateImages(images []Image) error {
	var total int
	for i, image := range images {
		if len(image.Data) == 0 {
			return domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %d has no data", i+1))
		}
		if len(image.Data) > MaxImageBytes {
			return domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %d exceeds the 10 MB per-image limit", i+1))
		}
		mediaType := ImageMediaType(image.Data)
		if mediaType == "" {
			return domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %d has an unsupported or invalid file signature", i+1))
		}
		if !strings.EqualFold(mediaType, image.MediaType) {
			return domain.E(domain.CodeInvalidArgument, fmt.Sprintf("image %d media type does not match its file signature", i+1))
		}
		total += len(image.Data)
		if total > MaxImagesTotalBytes {
			return domain.E(domain.CodeInvalidArgument, "images exceed the 20 MB per-turn limit")
		}
	}
	return nil
}

// ImageMediaType returns the MIME type supported by image file signatures.
func ImageMediaType(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(data) >= 3 && bytes.Equal(data[:3], []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return "image/gif"
	default:
		return ""
	}
}
