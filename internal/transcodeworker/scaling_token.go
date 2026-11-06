// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/deadfin-org/deadfin/internal/store"
)

// scalingTokenPrefix marks a scaling token, so it is not mistaken for a
// worker token or an API key.
const scalingTokenPrefix = "dfs1."

// NewScalingToken returns a new scaling token (#617). It reads only the
// transcode metrics and the scaling signals.
func NewScalingToken() (string, error) {
	secret, err := newSecret()
	if err != nil {
		return "", err
	}
	return scalingTokenPrefix + secret, nil
}

// ScalingTokenHash is the stored form of a scaling token.
func ScalingTokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// ScalingTokens checks the scaling tokens: the static ones from the server
// environment and the one the dashboard created.
type ScalingTokens struct {
	Store  *store.TranscodeWorkerStore
	Static []string
}

// Check reports whether r carries "Authorization: Bearer <scaling token>".
// Any other credential is for the route policy to judge.
func (tokens ScalingTokens) Check(r *http.Request) (bool, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(token, scalingTokenPrefix) {
		return false, nil
	}
	return tokens.valid(r.Context(), token)
}

func (tokens ScalingTokens) valid(ctx context.Context, token string) (bool, error) {
	match := 0
	for _, static := range tokens.Static {
		match |= subtle.ConstantTimeCompare([]byte(token), []byte(static))
	}
	if match == 1 {
		return true, nil
	}
	if tokens.Store == nil {
		return false, nil
	}
	hash, err := tokens.Store.ScalingTokenHash(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(ScalingTokenHash(token), hash) == 1, nil
}
