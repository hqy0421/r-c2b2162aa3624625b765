package report

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// nowRFC3339 is a variable so that tests can pin the clock.
var nowRFC3339 = func() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// uniqueSuffix returns a short random token used to name staging files.
func uniqueSuffix() string {
	var buffer [6]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		// The suffix only needs to be unique among concurrent writers; the
		// nanosecond clock is a sufficient fallback and never affects
		// correctness of the published content.
		return time.Now().UTC().Format("150405.000000000")
	}
	return hex.EncodeToString(buffer[:])
}
