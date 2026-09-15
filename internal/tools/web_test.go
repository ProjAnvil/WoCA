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
