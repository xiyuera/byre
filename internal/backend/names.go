package backend

import (
	"crypto/rand"
	"encoding/hex"
)

// randomSuffix returns 8 hex characters used to make generated object names unique without embedding
// any identity material in them.
func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failure here means the process cannot produce names at all; returning a fixed prefix would
		// collide, so panic is the honest outcome.
		panic("backend: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
