// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// identityFile is the worker identity in the agent state directory.
const identityFile = "identity.json"

// Identity is the durable worker identity that the server minted at
// enrollment (#615). The secret never leaves the worker again; the
// handshake proves it.
type Identity struct {
	WorkerID     string `json:"workerId"`
	WorkerSecret string `json:"workerSecret"`
	// Fingerprints are the server key fingerprints the worker accepts.
	// Empty means CA verification, not pinning.
	Fingerprints []string `json:"fingerprints,omitempty"`
}

// loadIdentity reads the identity. It returns fs.ErrNotExist when the
// worker has not enrolled.
func loadIdentity(dir string) (Identity, error) {
	data, err := os.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		return Identity{}, err
	}
	var identity Identity
	if err := json.Unmarshal(data, &identity); err != nil {
		return Identity{}, fmt.Errorf("parse worker identity: %w", err)
	}
	if identity.WorkerID == "" || identity.WorkerSecret == "" {
		return Identity{}, errors.New("worker identity is incomplete")
	}
	return identity, nil
}

// checkStateDir reports an error when the worker cannot write its identity
// to dir.
func checkStateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create worker state directory: %w", err)
	}
	probe, err := os.CreateTemp(dir, identityFile+".*")
	if err != nil {
		return fmt.Errorf("worker state directory is not writable: %w", err)
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}

// saveIdentity writes the identity atomically with mode 0600.
func saveIdentity(dir string, identity Identity) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create worker state directory: %w", err)
	}
	data, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, identityFile+".*")
	if err != nil {
		return fmt.Errorf("write worker identity: %w", err)
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write worker identity: %w", err)
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write worker identity: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write worker identity: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write worker identity: %w", err)
	}
	if err := os.Rename(name, filepath.Join(dir, identityFile)); err != nil {
		return fmt.Errorf("write worker identity: %w", err)
	}
	return nil
}

// unionPins returns the sorted union of two fingerprint sets, without
// empty values.
func unionPins(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, pin := range append(append([]string(nil), a...), b...) {
		if pin != "" {
			seen[pin] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for pin := range seen {
		out = append(out, pin)
	}
	sort.Strings(out)
	return out
}
