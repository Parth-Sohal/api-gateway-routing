package httpx

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"
)

// HeaderRequestID is the inbound and outbound correlation header.
const HeaderRequestID = "X-Request-Id"

// crockford is the ULID alphabet.
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// NewRequestID returns a 26-character ULID: 48-bit millisecond timestamp plus
// 80 bits of cryptographic randomness. It is sortable by time and needs no
// external dependency.
func NewRequestID() string {
	var ts [6]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 5; i >= 0; i-- {
		ts[i] = byte(ms)
		ms >>= 8
	}
	var rnd [10]byte
	_, _ = rand.Read(rnd[:]) // crypto/rand.Read never returns an error on supported platforms
	return crockford.EncodeToString(ts[:]) + crockford.EncodeToString(rnd[:])
}

// SanitizeRequestID validates an inbound request ID. Accepting arbitrary bytes
// would allow log injection and unbounded labels, so only 1-128 characters from
// a conservative alphabet are allowed.
func SanitizeRequestID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 128 {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '-', '_', '.', ':':
			continue
		}
		return "", false
	}
	return s, true
}
