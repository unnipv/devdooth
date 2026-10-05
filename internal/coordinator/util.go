package coordinator

import (
	"crypto/rand"
	"encoding/base64"
)

// randomToken returns n random bytes encoded as URL-safe base64 without
// padding. Tokens are opaque; they are never logged.
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("devdooth: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// TokenBytes is the entropy used for durable credentials.
const TokenBytes = 32
