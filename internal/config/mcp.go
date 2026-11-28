package config

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
)

const MCPOAuthCallbackPath = "/v1/mcp/oauth/callback"

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseMCPEndpoint validates an operator-configured HTTPS or loopback endpoint.
func ParseMCPEndpoint(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err == nil && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" {
		if u.Scheme == "https" || u.Scheme == "http" && (net.ParseIP(u.Hostname()).IsLoopback() || u.Hostname() == "localhost") {
			return u, nil
		}
	}
	return nil, errors.New("endpoint must use HTTPS or loopback HTTP without credentials, query or fragment")
}

func (c MCPServerConfig) TransportName() string {
	if c.Transport != "" {
		return c.Transport
	}
	if c.URL != "" {
		return "streamable_http"
	}
	return "stdio"
}

func (c MCPServerConfig) Validate() error {
	switch c.TransportName() {
	case "stdio":
		if strings.TrimSpace(c.Command) == "" {
			return errors.New("command is required")
		}
		if c.URL != "" || c.TokenEnv != "" || c.OAuth != nil {
			return errors.New("stdio cannot use HTTP settings")
		}
	case "streamable_http", "http":
		if _, err := ParseMCPEndpoint(c.URL); err != nil {
			return err
		}
		if c.Command != "" || len(c.Args) > 0 {
			return errors.New("HTTP transport cannot start a command")
		}
		if c.TokenEnv != "" && (!envName.MatchString(c.TokenEnv) || c.OAuth != nil) {
			return errors.New("use a token_env variable name or OAuth")
		}
		if a := c.OAuth; a != nil {
			if _, err := ParseMCPEndpoint(a.Issuer); err != nil || strings.TrimSpace(a.ClientID) == "" {
				return errors.New("OAuth requires an HTTPS or loopback issuer and a public client ID")
			}
			if u, err := ParseMCPEndpoint(a.RedirectURI); err != nil || u.Path != MCPOAuthCallbackPath {
				return errors.New("OAuth redirect_uri must target /v1/mcp/oauth/callback")
			}
		}
	default:
		return errors.New("unsupported MCP transport")
	}
	return nil
}
