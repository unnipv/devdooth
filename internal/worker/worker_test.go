package worker

import (
	"strings"
	"testing"

	"github.com/unnipv/devdooth/internal/protocol"
)

func TestToWS(t *testing.T) {
	tests := []struct {
		base, path, want string
		wantErr          bool
	}{
		{"http://localhost:8080", "/v1/worker/connect", "ws://localhost:8080/v1/worker/connect", false},
		{"https://devdooth.example", "/v1/tunnel/x", "wss://devdooth.example/v1/tunnel/x", false},
		{"https://devdooth.example/base/", "/v1/tunnel/x", "wss://devdooth.example/base/v1/tunnel/x", false},
		{"ftp://nope", "/x", "", true},
	}
	for _, tc := range tests {
		got, err := toWS(tc.base, tc.path)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("toWS(%q) expected error", tc.base)
			}
			continue
		}
		if err != nil {
			t.Fatalf("toWS(%q): %v", tc.base, err)
		}
		if got != tc.want {
			t.Fatalf("toWS(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestLaunchArgs(t *testing.T) {
	headless := launchArgs("/tmp/p", false, false, "")
	joined := strings.Join(headless, " ")
	for _, want := range []string{"--remote-debugging-port=0", "--user-data-dir=/tmp/p", "--headless=new", "--remote-allow-origins=*"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("headless args missing %q: %v", want, headless)
		}
	}
	if strings.Contains(joined, "--no-sandbox") {
		t.Fatalf("sandbox must stay on by default: %v", headless)
	}

	noSandbox := strings.Join(launchArgs("/tmp/p", false, true, ""), " ")
	if !strings.Contains(noSandbox, "--no-sandbox") {
		t.Fatalf("--no-sandbox requested but missing: %v", noSandbox)
	}

	headful := strings.Join(launchArgs("/tmp/p", true, false, "https://example.com"), " ")
	if strings.Contains(headful, "--headless") {
		t.Fatalf("headful args must not include --headless: %v", headful)
	}
	if !strings.HasSuffix(headful, "https://example.com") {
		t.Fatalf("start URL should be the last argument: %v", headful)
	}
}

func TestPickBrowser(t *testing.T) {
	w := &Worker{browsers: []protocol.Browser{
		{Name: "chrome", Path: "/apps/chrome"},
		{Name: "chromium", Path: "/apps/chromium"},
	}}

	path, name, err := w.pickBrowser("")
	if err != nil || name != "chrome" || path != "/apps/chrome" {
		t.Fatalf("default pick = (%q,%q,%v)", path, name, err)
	}
	if _, name, err := w.pickBrowser("chromium"); err != nil || name != "chromium" {
		t.Fatalf("requested pick = (%q,%v)", name, err)
	}
	if _, _, err := w.pickBrowser("firefox"); err == nil {
		t.Fatal("expected error for unavailable browser")
	}

	empty := &Worker{}
	if _, _, err := empty.pickBrowser(""); err == nil {
		t.Fatal("expected error when no browsers are available")
	}
}
