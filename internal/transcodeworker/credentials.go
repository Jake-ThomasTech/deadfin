// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/store"
)

// ErrUnknownCredential reports a join token or worker id that does not exist
// or is revoked.
var ErrUnknownCredential = errors.New("unknown transcode worker credential")

// Credential is the handshake key of one join token or one worker identity.
type Credential struct {
	Key []byte
	// Static marks a token from the server environment (GitOps setup). It
	// is reusable and enrolls no identity: each connection is a worker.
	Static bool
	// Name and Pools are the stored, operator-owned values of a worker
	// identity. They are empty for a join token.
	Name      string
	Pools     []string
	InputMode string
}

// Enrollment is a new worker identity minted from a join token.
type Enrollment struct {
	JoinTokenID string
	WorkerID    string
	Key         []byte
	Name        string
	Pools       []string
	InputMode   string
}

// Credentials resolves and records worker credentials (#615). The server
// uses StoreCredentials; every method takes the request context.
type Credentials interface {
	// JoinToken returns the key of a join token. It returns
	// ErrUnknownCredential for an unknown token; usability is checked by
	// Enroll, after the worker proved the key.
	JoinToken(ctx context.Context, id string) (Credential, error)
	// Worker returns the key of an active worker identity, or
	// ErrUnknownCredential when it does not exist or is revoked.
	Worker(ctx context.Context, id string) (Credential, error)
	// Enroll spends one use of the join token and stores the identity.
	Enroll(ctx context.Context, enrollment Enrollment) error
	// Connected records a worker connection.
	Connected(ctx context.Context, workerID string) error
	// Forget deletes the identity of an ephemeral worker (#623).
	Forget(ctx context.Context, workerID string) error
	// Pins returns the extra server key fingerprints that pinned workers
	// must accept.
	Pins(ctx context.Context) ([]string, error)
	// Enabled reports whether an operator configured transcode workers.
	// Until then the connect route answers 404.
	Enabled(ctx context.Context) (bool, error)
}

// deriveKey is the handshake key of a secret. The server stores only this
// key; the secret never crosses the wire after enrollment.
func deriveKey(secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("deadfin transcode worker v1"))
	return mac.Sum(nil)
}

// StoreCredentials implements Credentials over the store tables.
type StoreCredentials struct {
	Store *store.TranscodeWorkerStore
	Clock clock.Clock
}

// JoinToken implements Credentials.
func (c StoreCredentials) JoinToken(ctx context.Context, id string) (Credential, error) {
	token, err := c.Store.JoinToken(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return Credential{}, ErrUnknownCredential
	}
	if err != nil {
		return Credential{}, err
	}
	return Credential{Key: token.DerivedKey}, nil
}

// Worker implements Credentials.
func (c StoreCredentials) Worker(ctx context.Context, id string) (Credential, error) {
	worker, err := c.Store.Worker(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !worker.Revoked.IsZero()) {
		return Credential{}, ErrUnknownCredential
	}
	if err != nil {
		return Credential{}, err
	}
	return Credential{Key: worker.DerivedKey, Name: worker.Name, Pools: worker.Pools, InputMode: worker.InputMode}, nil
}

// Enroll implements Credentials.
func (c StoreCredentials) Enroll(ctx context.Context, enrollment Enrollment) error {
	err := c.Store.EnrollWorker(ctx, store.TranscodeWorker{
		ID: enrollment.WorkerID, Name: enrollment.Name, JoinTokenID: enrollment.JoinTokenID,
		DerivedKey: enrollment.Key, Pools: enrollment.Pools, InputMode: enrollment.InputMode,
	}, c.Clock.Now())
	if errors.Is(err, store.ErrNotFound) {
		return ErrUnknownCredential
	}
	if err != nil {
		return fmt.Errorf("enroll transcode worker: %w", err)
	}
	return nil
}

// Connected implements Credentials.
func (c StoreCredentials) Connected(ctx context.Context, workerID string) error {
	return c.Store.TouchWorker(ctx, workerID, c.Clock.Now())
}

// Forget implements Credentials.
func (c StoreCredentials) Forget(ctx context.Context, workerID string) error {
	return c.Store.DeleteWorker(ctx, workerID)
}

// Pins implements Credentials.
func (c StoreCredentials) Pins(ctx context.Context) ([]string, error) { return c.Store.Pins(ctx) }

// Enabled implements Credentials.
func (c StoreCredentials) Enabled(ctx context.Context) (bool, error) {
	return c.Store.Configured(ctx)
}

// StaticTokens adds the tokens from the server environment to inner. A
// static token connects workers directly, without enrollment, so a GitOps
// setup needs only one server variable and the server URL and token on each
// worker.
func StaticTokens(inner Credentials, tokens []Token) Credentials {
	if len(tokens) == 0 {
		return inner
	}
	keys := make(map[string][]byte, len(tokens))
	for _, token := range tokens {
		keys[token.ID] = token.Key()
	}
	return staticCredentials{Credentials: inner, keys: keys}
}

type staticCredentials struct {
	Credentials
	keys map[string][]byte
}

func (c staticCredentials) JoinToken(ctx context.Context, id string) (Credential, error) {
	if key, ok := c.keys[id]; ok {
		return Credential{Key: key, Static: true}, nil
	}
	return c.Credentials.JoinToken(ctx, id)
}

func (c staticCredentials) Enabled(context.Context) (bool, error) { return true, nil }
