package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
)

type apiKeyVerifier struct {
	keys map[string]config.APIKey
	now  func() time.Time
}

func newAPIKeyVerifier(keys []config.APIKey) (*apiKeyVerifier, error) {
	v := &apiKeyVerifier{
		keys: make(map[string]config.APIKey, len(keys)),
		now:  time.Now,
	}
	for _, k := range keys {
		if _, ok := v.keys[k.ID]; ok {
			return nil, fmt.Errorf("duplicate API key id %q", k.ID)
		}
		raw, err := hex.DecodeString(k.Hash)
		if err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("API key %q has an invalid SHA-256 hash", k.ID)
		}
		v.keys[k.ID] = k
	}
	return v, nil
}

// verify expects the format gw_<id>_<secret>. Only the SHA-256 of the secret
// is stored. Comparisons are constant-time, including for unknown IDs.
func (v *apiKeyVerifier) verify(raw string) (*Principal, error) {
	if len(raw) > maxTokenBytes {
		return nil, authError(ReasonInvalid, "API key too large")
	}
	if !strings.HasPrefix(raw, "gw_") {
		return nil, authError(ReasonInvalid, "API key has an invalid format")
	}
	parts := strings.SplitN(raw, "_", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return nil, authError(ReasonInvalid, "API key has an invalid format")
	}
	id, secret := parts[1], parts[2]
	sum := sha256.Sum256([]byte(secret))

	rec, ok := v.keys[id]
	if !ok {
		var zero [sha256.Size]byte
		subtle.ConstantTimeCompare(sum[:], zero[:])
		return nil, authError(ReasonInvalid, "unknown API key")
	}
	stored, err := hex.DecodeString(rec.Hash)
	if err != nil || len(stored) != sha256.Size {
		return nil, authError(ReasonInvalid, "API key record is invalid")
	}
	if subtle.ConstantTimeCompare(sum[:], stored) != 1 {
		return nil, authError(ReasonInvalid, "invalid API key")
	}
	if rec.Revoked {
		return nil, authError(ReasonRevoked, "API key is revoked")
	}
	if rec.ExpiresAt != nil && !rec.ExpiresAt.After(v.now()) {
		return nil, authError(ReasonExpired, "API key has expired")
	}

	owner := rec.Owner
	if owner == "" {
		owner = rec.ID
	}
	return &Principal{
		ID:     rec.ID,
		Owner:  owner,
		Kind:   "api_key",
		Scopes: dedupe(rec.Scopes),
		Roles:  dedupe(rec.Roles),
	}, nil
}
