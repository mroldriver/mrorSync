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
		{"http 403 idc", &HTTPError{Status: http.StatusForbidden, Body: `{"error_code":-11015}`}, false},
		{"http 500", &HTTPError{Status: http.StatusInternalServerError}, false},
		{"plain", errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := IsAuthError(c.err); got != c.want {
			t.Errorf("%s: IsAuthError=%v, want %v", c.name, got, c.want)
		}
	}
}

func TestTransientPCS(t *testing.T) {
	if !transientPCS([]byte(`{"error_code":-11015,"error_msg":"dest idc equal src idc"}`)) {
		t.Fatal("-11015 should be transient")
	}
	for _, body := range []string{`{"error_code":-6}`, `{"error_code":0}`, `not json`, ``} {
		if transientPCS([]byte(body)) {
			t.Fatalf("%q should not be transient", body)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	if d := retryAfter("2"); d != 2*time.Second {
		t.Fatalf("retryAfter(2)=%v", d)
	}
	if d := retryAfter("0.5"); d != 0 {
		t.Fatalf("retryAfter(0.5)=%v, want 0 (not an integer)", d)
	}
	if d := retryAfter("Wed, 21 Oct 2027 07:28:00 GMT"); d <= 0 {
		t.Fatalf("http-date in the future should be honored, got %v", d)
	}
	if d := retryAfter("Wed, 21 Oct 2020 07:28:00 GMT"); d != 0 {
		t.Fatalf("http-date in the past should be ignored, got %v", d)
	}
	if d := retryAfter("garbage"); d != 0 {
		t.Fatalf("garbage retryAfter=%v", d)
	}
}

func TestBackoffBoundsAndJitter(t *testing.T) {
	backoffMax := 8 * time.Second
	busyMax := 40 * time.Second
	for attempt := 0; attempt < 7; attempt++ {
		base := time.Duration(1<<attempt) * time.Second
		if base > backoffMax {
			base = backoffMax
		}
		d := backoff(attempt, backoffMax)
		if d < base/2 || d > base {
			t.Fatalf("backoff(%d)=%v, want within [%v,%v]", attempt, d, base/2, base)
		}

		busyBase := time.Duration(5<<attempt) * time.Second
		if busyBase > busyMax {
			busyBase = busyMax
		}
		bd := busyBackoff(attempt, busyMax)
		if bd < busyBase/2 || bd > busyBase {
			t.Fatalf("busyBackoff(%d)=%v, want within [%v,%v]", attempt, bd, busyBase/2, busyBase)
		}
	}
}
