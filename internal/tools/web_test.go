package tools

import (
	"strings"
	"testing"
)

func TestValidateWebURLBlocksSSRF(t *testing.T) {
	blocked := []string{
		"http://127.0.0.1/",
		"http://localhost/",
		"http://[::1]/",
		"http://169.254.169.254/",
		"http://10.0.0.1/",
		"http://192.168.1.1/",
		"http://0.0.0.0/",
		// IPv4-compatible IPv6 spellings (::/96) used to bypass the IPv4
		// checks before langchain-golang v0.11.0 unwrapped them.
		"http://[::127.0.0.1]/",
		"http://[::169.254.169.254]/",
		"http://[::ffff:127.0.0.1]/",
		// NAT64 well-known and discovery prefixes (blocked since v0.11.0).
		"http://[64:ff9b::192.0.2.1]/",
		"http://[64:ff9b:1::192.0.2.1]/",
	}
	for _, rawurl := range blocked {
		rawurl := rawurl
		t.Run(rawurl, func(t *testing.T) {
			err := validateWebURL(rawurl)
			if err == nil {
				t.Fatalf("validateWebURL(%q) = nil, want blocked error", rawurl)
			}
			if !strings.Contains(err.Error(), "blocked") {
				t.Errorf("validateWebURL(%q) error = %q, want to mention %q", rawurl, err, "blocked")
			}
		})
	}
}

func TestValidateWebURLAllowsSafeHost(t *testing.T) {
	safe := []string{
		"https://example.com/",
		"https://www.google.com/",
		"https://en.wikipedia.org/wiki/Go_(programming_language)",
	}
	for _, rawurl := range safe {
		rawurl := rawurl
		t.Run(rawurl, func(t *testing.T) {
			if err := validateWebURL(rawurl); err != nil {
				t.Errorf("validateWebURL(%q) = %v, want nil", rawurl, err)
			}
		})
	}
}

func TestFetchHTTPBlocksSSRFBeforeNetwork(t *testing.T) {
	// fetchHTTP must reject loopback/private URLs at the validation step and
	// return before any network request is made.
	for _, rawurl := range []string{
		"http://127.0.0.1/",
		"http://localhost/",
		"http://[::1]/",
		"http://169.254.169.254/",
		"http://[::127.0.0.1]/",
		"http://[::169.254.169.254]/",
	} {
		_, err := fetchHTTP(t.Context(), rawurl)
		if err == nil {
			t.Fatalf("fetchHTTP(%q) = nil error, want blocked", rawurl)
		}
		if !strings.Contains(err.Error(), "blocked") {
			t.Errorf("fetchHTTP(%q) error = %q, want to mention %q", rawurl, err, "blocked")
		}
	}
}
