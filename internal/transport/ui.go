package transport

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/web"
)

var hashedUIAsset = regexp.MustCompile(`^assets/.+-[A-Za-z0-9_-]{8}\.[^./]+$`)

// withUI routes the public UI before the API's authentication and path cleaning.
func withUI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ui" && !strings.HasPrefix(r.URL.Path, "/ui/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		serveUI(w, r)
	})
}

func serveUI(w http.ResponseWriter, r *http.Request) {
	const index = "index.html"
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
	if name == "" || r.URL.Path == "/ui" {
		name = index
	} else {
		name = strings.TrimSuffix(name, "/")
		if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\\x00") {
			http.NotFound(w, r)
			return
		}
	}

	f, err := web.Dist.Open("dist/" + name)
	if err != nil && path.Ext(name) == "" && !strings.HasPrefix(name, "assets/") {
		// A navigation to an SPA route gets the same entry point as /ui.
		name = index
		f, err = web.Dist.Open("dist/" + name)
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	body, ok := f.(io.ReadSeeker) // embed.FS files are seekable; directories are not.
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch {
	case name == index:
		w.Header().Set("Cache-Control", "no-store")
	case hashedUIAsset.MatchString(name):
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, time.Time{}, body)
}
