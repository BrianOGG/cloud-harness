package web_test

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/transport"
)

func TestEmbeddedUI(t *testing.T) {
	handler := (&transport.Server{Token: "ui-acceptance-token"}).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		return r
	}

	var index string
	for _, path := range []string{"/ui", "/ui/", "/ui/index.html", "/ui/some/spa/route", "/ui/some/spa/route/"} {
		r := get(path)
		if r.Code != http.StatusOK || r.Header().Get("Content-Type") != "text/html; charset=utf-8" || r.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: status %d, headers %v", path, r.Code, r.Header())
		}
		if index == "" {
			index = r.Body.String()
			if !strings.Contains(index, `<div id="root"></div>`) {
				t.Fatal("/ui did not return the HTML entry point")
			}
		}
		if r.Body.String() != index {
			t.Fatalf("%s did not return the same entry point as /ui", path)
		}
	}

	for _, tc := range []struct {
		ext, cache, contentType string
	}{
		{"js", "public, max-age=31536000, immutable", "text/javascript"},
		{"css", "public, max-age=31536000, immutable", "text/css"},
		{"svg", "no-cache", "image/svg+xml"},
	} {
		match := regexp.MustCompile(`(?:src|href)="(/ui/[^" ]+\.` + tc.ext + `)"`).FindStringSubmatch(index)
		if match == nil {
			t.Fatalf("entry point does not reference a .%s file under /ui/", tc.ext)
		}
		asset := match[1]
		r := get(asset)
		mediaType, _, err := mime.ParseMediaType(r.Header().Get("Content-Type"))
		if r.Code != http.StatusOK || err != nil || r.Body.Len() == 0 || r.Header().Get("Cache-Control") != tc.cache {
			t.Fatalf("%s: status %d, headers %v, bytes %d", asset, r.Code, r.Header(), r.Body.Len())
		}
		if mediaType != tc.contentType && !(tc.ext == "js" && mediaType == "application/javascript") {
			t.Errorf("%s: unexpected Content-Type %q", asset, mediaType)
		}
	}
}

func TestUIPathBoundary(t *testing.T) {
	handler := (&transport.Server{Token: "ui-acceptance-token"}).Handler()
	for _, path := range []string{
		"/ui/../", "/ui/../healthz", "/ui/%2e%2e/healthz", "/ui/assets/../index.html",
		"/ui/assets/", "/ui/assets/missing.js",
	} {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		if r.Code != http.StatusNotFound || r.Header().Get("Location") != "" {
			t.Errorf("%s: expected 404 without a redirect, got %d %v", path, r.Code, r.Header())
		}
	}
	for _, path := range []string{"/ui-preview", "/v1/sessions"} {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		if r.Code != http.StatusUnauthorized || r.Header().Get("WWW-Authenticate") != `Bearer realm="cloud-harness"` {
			t.Errorf("%s: expected 401 and Bearer challenge, got %d %v", path, r.Code, r.Header())
		}
	}
}
