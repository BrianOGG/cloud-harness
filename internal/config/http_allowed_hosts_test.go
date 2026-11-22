package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHTTPAllowedHostsConfig(t *testing.T) {
	for _, tt := range []struct {
		name string
		file string
		env  string
		want []string
	}{
		{name: "default", file: `{}`},
		{name: "file", file: `{"http_allowed_hosts":["Harness.Test"," proxy.test:8443 ",""]}`, want: []string{"harness.test", "proxy.test:8443"}},
		{name: "env-overrides-file", file: `{"http_allowed_hosts":["harness.test"]}`, env: "a.test, [::1]:8080,192.0.2.1", want: []string{"a.test", "[::1]:8080", "192.0.2.1"}},
		{name: "bracketed-ipv6", file: `{"http_allowed_hosts":["[2001:db8::1]"]}`, want: []string{"2001:db8::1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tt.file), 0644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path, func(k string) string {
				if k == "CLOUD_HARNESS_HTTP_ALLOWED_HOSTS" {
					return tt.env
				}
				return ""
			})
			if err != nil || !reflect.DeepEqual(cfg.HTTPAllowedHosts, tt.want) {
				t.Fatalf("hosts: %q, %v, want %q", cfg.HTTPAllowedHosts, err, tt.want)
			}
			shown, ok := cfg.Redacted()["http_allowed_hosts"]
			if ok != (len(tt.want) > 0) || (ok && !reflect.DeepEqual(shown, tt.want)) {
				t.Fatalf("redacted hosts: %v", shown)
			}
		})
	}
	for _, entry := range []string{"http://harness.test", "harness.test/ui", "harness.test:0", "harness.test:port", "harness.test:", "*", "*.test", "a..test", "-a.test", "user@harness.test"} {
		if hosts, err := ParseHTTPAllowedHosts([]string{entry}); err == nil {
			t.Errorf("entry %q accepted as %q", entry, hosts)
		}
	}
}
