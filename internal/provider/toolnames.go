package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// Chat Completions function names are limited to this alphabet and length.
// Harness names such as mcp.<server>.<tool> contain dots, so they are encoded
// on the wire only; policy, grants, and audit keep the harness name.
var wireToolNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var wireToolNameInvalid = regexp.MustCompile(`[^A-Za-z0-9_-]`)

const wireToolNameMax = 64

type wireToolNames struct {
	toWire   map[string]string
	fromWire map[string]string
}

func newWireToolNames(defs []ToolDefinition) *wireToolNames {
	w := &wireToolNames{toWire: map[string]string{}, fromWire: map[string]string{}}
	// Names that are already valid keep themselves, so an encoded name can never shadow one.
	for _, def := range defs {
		if wireToolNameRE.MatchString(def.Name) {
			w.toWire[def.Name] = def.Name
			w.fromWire[def.Name] = def.Name
		}
	}
	for _, def := range defs {
		if _, ok := w.toWire[def.Name]; ok {
			continue
		}
		wire := sanitizeWireToolName(def.Name)
		if _, taken := w.fromWire[wire]; taken {
			wire = hashedWireToolName(def.Name)
		}
		w.toWire[def.Name] = wire
		w.fromWire[wire] = def.Name
	}
	return w
}

// wire returns the name to send. Names from history that are no longer offered
// are still made valid so one stale call cannot fail the whole request.
func (w *wireToolNames) wire(name string) string {
	if wire, ok := w.toWire[name]; ok {
		return wire
	}
	if wireToolNameRE.MatchString(name) {
		return name
	}
	return sanitizeWireToolName(name)
}

// restore maps tool calls in a provider response back to harness names.
func (w *wireToolNames) restore(resp domain.ModelResponse, err error) (domain.ModelResponse, error) {
	for i := range resp.Items {
		if call := resp.Items[i].ToolCall; call != nil {
			if name, ok := w.fromWire[call.Name]; ok {
				call.Name = name
			}
		}
	}
	return resp, err
}

func sanitizeWireToolName(name string) string {
	wire := wireToolNameInvalid.ReplaceAllString(name, "_")
	if wire == "" || len(wire) > wireToolNameMax {
		return hashedWireToolName(name)
	}
	return wire
}

func hashedWireToolName(name string) string {
	sum := sha256.Sum256([]byte(name))
	suffix := "_" + hex.EncodeToString(sum[:4])
	wire := wireToolNameInvalid.ReplaceAllString(name, "_")
	if keep := wireToolNameMax - len(suffix); len(wire) > keep {
		wire = wire[:keep]
	}
	return wire + suffix
}
