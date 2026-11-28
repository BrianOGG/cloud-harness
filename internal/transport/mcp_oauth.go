package transport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

// Each configured resource has its own token slot, issuer and one pending
// authorization. Secrets never enter config, SQLite, diagnostic text or URLs.
type mcpOAuth struct {
	mu                          sync.Mutex
	cfg                         config.MCPOAuthConfig
	resource                    string
	metadataURL, challengeScope string
	access, refresh, tokenURL   string
	expiry                      time.Time
	pending                     *pendingOAuth
}
type pendingOAuth struct {
	state, verifier, tokenURL string
	requireIssuer             bool
	expires                   time.Time
}

func randomOAuthValue() string {
	var buf [32]byte
	rand.Read(buf[:])
	return base64.RawURLEncoding.EncodeToString(buf[:])
}

var bearerParam = regexp.MustCompile(`(?i)(?:^|[, ])(resource_metadata|scope)="([^"]*)"`)

func (a *mcpOAuth) reject(challenges []string, used string, retry bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Another request may already have refreshed the rejected credential.
	if retry && used != "" && a.access != "" && a.access != used {
		return true
	}
	a.access = ""
	if !retry {
		a.refresh = ""
	}
	a.metadataURL = ""
	a.challengeScope = ""
	for _, h := range challenges {
		if used != "" && strings.Contains(h, used) {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(h)), "bearer ") {
			continue
		}
		for _, pair := range bearerParam.FindAllStringSubmatch(h, -1) {
			switch strings.ToLower(pair[1]) {
			case "resource_metadata":
				a.metadataURL = pair[2]
			case "scope":
				a.challengeScope = pair[2]
			}
		}
	}
	return retry && used != "" && a.refresh != ""
}
func (a *mcpOAuth) Forget() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.access = ""
	a.refresh = ""
	a.tokenURL = ""
	a.pending = nil
}
func (a *mcpOAuth) bearer(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.access != "" && (a.expiry.IsZero() || time.Now().Add(15*time.Second).Before(a.expiry)) {
		return a.access, nil
	}
	if a.refresh == "" {
		if a.access != "" {
			a.access = ""
			return "", tools.ErrMCPAuthorization
		}
		return "", nil // initial discovery can obtain the resource's bearer challenge
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {a.refresh}}
	if err := a.exchange(ctx, a.tokenURL, form); err != nil {
		return "", err
	}
	return a.access, nil
}

func sameEndpointOrigin(raw string, base *url.URL) bool {
	u, err := config.ParseMCPEndpoint(raw)
	return err == nil && u.Scheme == base.Scheme && strings.EqualFold(u.Host, base.Host)
}
func wellKnown(base *url.URL, kind string) string {
	return base.Scheme + "://" + base.Host + "/.well-known/" + kind + strings.TrimSuffix(base.EscapedPath(), "/")
}
func readDocument(resp *http.Response, out any) error {
	defer func() { _ = resp.Body.Close() }()
	const limit = 128 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit || json.Unmarshal(body, out) != nil {
		return errors.New("invalid or oversized OAuth response")
	}
	return nil
}
func getMetadata(ctx context.Context, locations []string, out any) error {
	for _, location := range locations {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
		req.Header.Set("Accept", "application/json")
		resp, err := mcpHTTP.Do(req)
		if err != nil {
			return errors.New("OAuth discovery connection failed")
		}
		if resp.StatusCode == 200 {
			return readDocument(resp, out)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 404 {
			break
		}
	}
	return errors.New("OAuth metadata unavailable; check configured resource and issuer")
}
func (a *mcpOAuth) Begin(ctx context.Context) (location, state string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = nil
	res, _ := config.ParseMCPEndpoint(a.resource)
	locations := []string{wellKnown(res, "oauth-protected-resource")}
	if res.Path != "" && res.Path != "/" {
		locations = append(locations, res.Scheme+"://"+res.Host+"/.well-known/oauth-protected-resource")
	}
	if a.metadataURL != "" {
		if !sameEndpointOrigin(a.metadataURL, res) {
			return "", "", errors.New("resource metadata origin does not match the MCP endpoint")
		}
		locations = []string{a.metadataURL}
	}
	var prm struct {
		Resource string
		Servers  []string `json:"authorization_servers"`
		Scopes   []string `json:"scopes_supported"`
	}
	if err = getMetadata(ctx, locations, &prm); err != nil {
		return "", "", err
	}
	if prm.Resource != a.resource || !slices.Contains(prm.Servers, a.cfg.Issuer) {
		return "", "", errors.New("resource or pinned OAuth issuer mismatch")
	}
	issuer, _ := config.ParseMCPEndpoint(a.cfg.Issuer)
	locations = []string{wellKnown(issuer, "oauth-authorization-server"), wellKnown(issuer, "openid-configuration")}
	if strings.Trim(issuer.Path, "/") != "" {
		locations = append(locations, strings.TrimSuffix(a.cfg.Issuer, "/")+"/.well-known/openid-configuration")
	}
	var meta struct {
		Issuer        string
		Authorize     string   `json:"authorization_endpoint"`
		Token         string   `json:"token_endpoint"`
		PKCE          []string `json:"code_challenge_methods_supported"`
		RequireIssuer bool     `json:"authorization_response_iss_parameter_supported"`
	}
	if err = getMetadata(ctx, locations, &meta); err != nil {
		return "", "", err
	}
	if meta.Issuer != a.cfg.Issuer || !sameEndpointOrigin(meta.Authorize, issuer) || !sameEndpointOrigin(meta.Token, issuer) {
		return "", "", errors.New("OAuth issuer or endpoint origin mismatch")
	}
	if !slices.Contains(meta.PKCE, "S256") {
		return "", "", errors.New("OAuth server must advertise S256 PKCE")
	}
	p := &pendingOAuth{state: randomOAuthValue(), verifier: randomOAuthValue(), tokenURL: meta.Token, expires: time.Now().Add(5 * time.Minute), requireIssuer: meta.RequireIssuer}
	challenge := sha256.Sum256([]byte(p.verifier))
	form := url.Values{"response_type": {"code"}, "client_id": {a.cfg.ClientID}, "redirect_uri": {a.cfg.RedirectURI}, "resource": {a.resource}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "state": {p.state}}
	scope := a.challengeScope
	if scope == "" {
		scope = strings.Join(a.cfg.Scopes, " ")
	}
	if scope == "" {
		scope = strings.Join(prm.Scopes, " ")
	}
	if scope != "" {
		form.Set("scope", scope)
	}
	a.pending = p
	return meta.Authorize + "?" + form.Encode(), p.state, nil
}

func (a *mcpOAuth) Finish(ctx context.Context, state, code, issuer, host string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pending
	if p == nil || subtle.ConstantTimeCompare([]byte(p.state), []byte(state)) != 1 {
		return false, nil
	}
	a.pending = nil // consume even denied/failed attempts
	redirect, _ := config.ParseMCPEndpoint(a.cfg.RedirectURI)
	if !time.Now().Before(p.expires) || code == "" || !strings.EqualFold(host, redirect.Host) || ((issuer != "" || p.requireIssuer) && issuer != a.cfg.Issuer) {
		return true, errors.New("OAuth authorization expired, denied or mismatched; authorize again")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {a.cfg.RedirectURI}, "code_verifier": {p.verifier}}
	return true, a.exchange(ctx, p.tokenURL, form)
}

// exchange runs under mu so rotating refresh tokens can only be used once.
func (a *mcpOAuth) exchange(ctx context.Context, tokenURL string, form url.Values) error {
	form.Set("resource", a.resource)
	form.Set("client_id", a.cfg.ClientID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := mcpHTTP.Do(req)
	if err != nil {
		return errors.New("OAuth token endpoint unreachable")
	}
	if resp.StatusCode != 200 {
		_ = resp.Body.Close()
		if resp.StatusCode == 400 || resp.StatusCode == 401 {
			a.access = ""
			a.refresh = ""
			return tools.ErrMCPAuthorization
		}
		return errors.New("OAuth token endpoint temporarily unavailable")
	}
	var tok struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Type    string `json:"token_type"`
		Expires *int64 `json:"expires_in"`
	}
	if err := readDocument(resp, &tok); err != nil {
		return err
	}
	if tok.Access == "" || strings.ContainsAny(tok.Access, " \r\n\t") || !strings.EqualFold(tok.Type, "bearer") || (tok.Expires != nil && (*tok.Expires <= 0 || *tok.Expires > int64((1<<63-1)/time.Second))) {
		return errors.New("invalid OAuth bearer token response")
	}
	a.tokenURL = tokenURL
	a.access = tok.Access
	if tok.Refresh != "" || form.Get("grant_type") == "authorization_code" {
		a.refresh = tok.Refresh
	}
	a.expiry = time.Time{}
	if tok.Expires != nil {
		a.expiry = time.Now().Add(time.Duration(*tok.Expires) * time.Second)
	}
	return nil
}
