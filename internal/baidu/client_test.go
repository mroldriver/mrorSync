package baidu

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestClipKeepsValidUTF8(t *testing.T) {
	s := strings.Repeat("中文", 10)

	got := clip([]byte(s), 7)
	if len(got) > 7 {
		t.Fatalf("clip returned %d bytes, want <= 7", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("clip broke UTF-8: %q", got)
	}
	if got != "中文" {
		t.Fatalf("clip=%q, want 中文", got)
	}

	if got := clip([]byte("abcdef"), 4); got != "abcd" {
		t.Fatalf("ascii clip=%q", got)
	}
	if got := clip([]byte("short"), 99); got != "short" {
		t.Fatalf("short clip=%q", got)
	}
}

func TestIsAuthError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"errno -6", &APIError{Errno: -6}, true},
		{"errno 110", &APIError{Errno: 110}, true},
		{"errno 111", &APIError{Errno: 111}, true},
		{"bad filename", &APIError{Errno: 31066}, false},
		{"http 401", &HTTPError{Status: http.StatusUnauthorized}, true},
		{"http 403", &HTTPError{Status: http.StatusForbidden}, true},
		{"http 500", &HTTPError{Status: http.StatusInternalServerError}, false},
		{"plain", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := IsAuthError(c.err); got != c.want {
			t.Errorf("%s: IsAuthError=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	if d := retryAfter("2"); d != 2*time.Second {
		t.Fatalf("retryAfter(2)=%v", d)
	}
	if d := retryAfter("0.5"); d != 500*time.Millisecond {
		t.Fatalf("retryAfter(0.5)=%v", d)
	}
	if d := retryAfter("Wed, 21 Oct 2026 07:28:00 GMT"); d != 0 {
		t.Fatalf("http-date should be unsupported for now, got %v", d)
	}
}
