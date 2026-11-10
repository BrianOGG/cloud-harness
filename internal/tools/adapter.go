package tools

import "context"

// Adapter discovers tool specs. It must not execute them.
// Protocol "mcp" is reserved; M0 does not dial an MCP server.
type Adapter interface {
	Protocol() string
	Discover(ctx context.Context) ([]Spec, error)
}

// MCPAdapter is the reserved mapping from an MCP server label to local specs.
// An implementation may exist later. Nothing in M0 opens a remote session.
type MCPAdapter interface {
	Adapter
	ServerLabel() string
}

// LocalAdapter exposes specs that were registered in-process.
type LocalAdapter struct {
	Specs []Spec
}

func (LocalAdapter) Protocol() string { return "local" }

func (a LocalAdapter) Discover(context.Context) ([]Spec, error) {
	out := make([]Spec, len(a.Specs))
	copy(out, a.Specs)
	return out, nil
}
