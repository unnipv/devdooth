package worker

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
)

// Version is the worker build version. Overridden at release time with -ldflags.
var Version = "0.0.0-dev"

func randomID() string {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		panic("devdooth: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// firstLine returns the first non-empty line of s, truncated. Browser crashes
// print a lot; this keeps one useful line in the error.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > 300 {
				line = line[:300] + "…"
			}
			return line
		}
	}
	return ""
}
