// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

func TestEnrollmentSavesTheIdentityAndReconnectsWithIt(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	dir := t.TempDir()

	first := startTestAgentIn(t, hub, dir, token, "gpu-a", Capabilities{MaxJobs: 1, Pools: []string{"QSV", "qsv", " big "}})
	enrolled := hub.waitEvent(t, EventEnrolled)
	hub.waitEvent(t, EventRegistered)
	// The hub reports before the agent reads its reply.
	eventually(t, nil, func() bool { return first.agent.WorkerID() != "" })
	info, err := os.Stat(filepath.Join(dir, identityFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode = %v, want 0600", info.Mode().Perm())
	}
	identity, err := loadIdentity(dir)
	if err != nil || identity.WorkerID != enrolled.WorkerID || len(identity.Fingerprints) != 0 {
		t.Fatalf("identity = %+v, %v; enrolled %q", identity, err, enrolled.WorkerID)
	}
	workers := hub.hub.Workers()
	if len(workers) != 1 || workers[0].ID != enrolled.WorkerID || !reflect.DeepEqual(workers[0].Pools, []string{"big", "qsv"}) {
		t.Fatalf("workers = %+v", workers)
	}
	stored, err := hub.workers.Worker(context.Background(), enrolled.WorkerID)
	if err != nil || stored.Name != "gpu-a" || stored.JoinTokenID != token.ID {
		t.Fatalf("stored worker = %+v, %v", stored, err)
	}
	first.kill()
	hub.waitEvent(t, EventDisconnected)

	// The join token is revoked; the identity still connects, under the
	// same id and without a second enrollment.
	if err := hub.workers.RevokeJoinToken(context.Background(), token.ID, testEpoch); err != nil {
		t.Fatal(err)
	}
	second := startTestAgentIn(t, hub, dir, Token{}, "renamed-locally", Capabilities{MaxJobs: 1})
	registered := hub.waitEvent(t, EventRegistered)
	if registered.WorkerID != enrolled.WorkerID || registered.Name != "gpu-a" || second.agent.WorkerID() != enrolled.WorkerID {
		t.Fatalf("reconnect registered %+v, agent id %q", registered, second.agent.WorkerID())
	}
	tokens, err := hub.workers.JoinTokens(context.Background())
	if err != nil || len(tokens) != 1 || tokens[0].UsesLeft != nil {
		t.Fatalf("join tokens = %+v, %v", tokens, err)
	}
	all, err := hub.workers.Workers(context.Background())
	if err != nil || len(all) != 1 {
		t.Fatalf("a reconnect enrolled again: %+v, %v", all, err)
	}
}

func TestSpentAndExpiredJoinTokensAreRefused(t *testing.T) {
	hub := newTestHub(t)
	one := 1
	single := mustToken(t)
	addJoinToken(t, hub.workers, single, &one, time.Time{})
	expired := mustToken(t)
	addJoinToken(t, hub.workers, expired, nil, testEpoch.Add(-time.Second))

	startTestAgent(t, hub, single, "first", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)

	for name, token := range map[string]Token{"spent": single, "expired": expired} {
		agent := startTestAgent(t, hub, token, name, Capabilities{MaxJobs: 1})
		if err := <-agent.done; !errors.Is(err, ErrRejected) {
			t.Fatalf("%s token: Run = %v, want ErrRejected", name, err)
		}
		agent.done <- nil
		if event := hub.waitEvent(t, EventRejected); event.Detail != "join token is revoked, expired or spent" {
			t.Fatalf("%s token: reject detail = %q", name, event.Detail)
		}
		if agent.agent.WorkerID() != "" {
			t.Fatalf("%s token: agent saved an identity", name)
		}
	}
}

func TestRevokedWorkerIsDisconnectedAndRefused(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	dir := t.TempDir()
	worker := startTestAgentIn(t, hub, dir, token, "w", Capabilities{MaxJobs: 1})
	id := hub.waitEvent(t, EventRegistered).WorkerID

	if err := hub.workers.RevokeWorker(context.Background(), id, testEpoch); err != nil {
		t.Fatal(err)
	}
	hub.hub.Disconnect(id)
	hub.waitEvent(t, EventDisconnected)
	// The agent reconnects after its delay and the server refuses the
	// revoked identity. The agent does not enroll again with its token.
	eventually(t, func() { worker.clock.Advance(2 * time.Second) }, func() bool {
		select {
		case err := <-worker.done:
			worker.done <- err
			return true
		default:
			return false
		}
	})
	if err := <-worker.done; !errors.Is(err, ErrRejected) {
		t.Fatalf("revoked worker: Run = %v, want ErrRejected", err)
	}
	worker.done <- nil
	if event := hub.waitEvent(t, EventRejected); event.Detail != "unknown token" {
		t.Fatalf("reject detail = %q", event.Detail)
	}
	if all, err := hub.workers.Workers(context.Background()); err != nil || len(all) != 1 {
		t.Fatalf("the revoked worker enrolled again: %+v, %v", all, err)
	}
}

func TestConnectRouteIsAbsentWithoutCredentials(t *testing.T) {
	hub := newTestHub(t)
	response, err := http.Get(hub.server.URL + PathConnect)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("connect without credentials = %d, want 404", response.StatusCode)
	}
}

// A pinned worker learns the next server key through the pins message and
// keeps working after the server rotates its certificate.
func TestPinUpdateSurvivesAServerKeyRotation(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubOn(t, HubConfig{}, httptest.NewTLSServer, token)
	next := selfSignedCertificate(t)
	nextPin := CertificateFingerprint(next.Leaf)
	rotated := httptest.NewUnstartedServer(hub.hub)
	rotated.TLS = &tls.Config{Certificates: []tls.Certificate{next}, MinVersion: tls.VersionTLS12}
	rotated.StartTLS()
	t.Cleanup(rotated.Close)
	rotatedHub := *hub
	rotatedHub.server = rotated

	pinned := token
	pinned.Fingerprint = CertificateFingerprint(hub.server.Certificate())
	dir, stale := t.TempDir(), t.TempDir()
	worker := startTestAgentIn(t, hub, dir, pinned, "pinned", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	other := startTestAgentIn(t, hub, stale, pinned, "stale", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	other.kill()
	hub.waitEvent(t, EventDisconnected)

	if err := hub.workers.SetPins(context.Background(), []string{nextPin}, testEpoch); err != nil {
		t.Fatal(err)
	}
	hub.hub.BroadcastPins([]string{nextPin})
	eventually(t, nil, func() bool {
		identity, err := loadIdentity(dir)
		return err == nil && len(identity.Fingerprints) == 2
	})
	worker.kill()
	hub.waitEvent(t, EventDisconnected)

	// The rotated server presents the next key. The updated worker
	// accepts it; the worker that missed the update refuses it.
	startTestAgentIn(t, &rotatedHub, dir, Token{}, "pinned", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	missed := startTestAgentIn(t, &rotatedHub, stale, Token{}, "stale", Capabilities{MaxJobs: 1})
	if err := <-missed.done; !errors.Is(err, errPinMismatch) {
		t.Fatalf("stale pins: Run = %v, want the pin mismatch", err)
	}
	missed.done <- nil
}

func selfSignedCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "rotated"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<32, 0),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// A static token from the server environment connects workers without
// enrollment and without a state directory; the worker name tells them
// apart.
func TestStaticTokensConnectWorkersWithoutEnrollment(t *testing.T) {
	static := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{Credentials: testStaticTokens{static}})
	if response, err := http.Get(hub.server.URL + PathConnect); err != nil {
		t.Fatal(err)
	} else if _ = response.Body.Close(); response.StatusCode == http.StatusNotFound {
		t.Fatal("connect answers 404 with a static token configured")
	}
	agents := make([]*testAgent, 0, 2)
	for _, name := range []string{"a", "b"} {
		agents = append(agents, startTestAgentIn(t, hub, "", static, name, Capabilities{MaxJobs: 1, Pools: []string{"QSV"}}))
		hub.waitEvent(t, EventRegistered)
	}
	workers := hub.hub.Workers()
	if len(workers) != 2 || !workers[0].Static || workers[0].ID != "static-"+static.ID+"-a" || workers[1].ID != "static-"+static.ID+"-b" ||
		strings.Join(workers[0].Pools, ",") != "qsv" {
		t.Fatalf("workers = %+v", workers)
	}
	for _, agent := range agents {
		if agent.agent.WorkerID() != "" {
			t.Fatal("a static token enrolled an identity")
		}
	}
	if stored, err := hub.workers.Workers(context.Background()); err != nil || len(stored) != 0 {
		t.Fatalf("static workers were stored: %+v, %v", stored, err)
	}
	agents[0].kill()
	hub.waitEvent(t, EventDisconnected)
	startTestAgentIn(t, hub, "", static, "a", Capabilities{MaxJobs: 1})
	if registered := hub.waitEvent(t, EventRegistered); registered.WorkerID != "static-"+static.ID+"-a" {
		t.Fatalf("reconnect id = %q", registered.WorkerID)
	}
}

// Without a state directory a dashboard join token still enrolls; the
// identity lives in memory.
func TestEnrollmentWithoutAStateDirectoryKeepsTheIdentityInMemory(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	worker := startTestAgentIn(t, hub, "", token, "w", Capabilities{MaxJobs: 1})
	enrolled := hub.waitEvent(t, EventEnrolled)
	hub.waitEvent(t, EventRegistered)
	eventually(t, nil, func() bool { return worker.agent.WorkerID() == enrolled.WorkerID })
}

func TestWorkerRoutesRequireTLSUnlessPlainHTTPIsAllowed(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		header string
		allow  bool
		secure bool
	}{
		{name: "remote plain", remote: "192.0.2.10:5000"},
		{name: "remote plain allowed", remote: "192.0.2.10:5000", allow: true, secure: true},
		{name: "proxy TLS", remote: "192.0.2.10:5000", header: "https", secure: true},
		{name: "loopback", remote: "127.0.0.1:5000", secure: true},
		{name: "loopback v6", remote: "[::1]:5000", secure: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allow := tc.allow
			hub := newTestHubWithConfig(t, HubConfig{AllowPlainHTTP: func() bool { return allow }})
			request := httptest.NewRequest(http.MethodGet, PathConnect, nil)
			request.RemoteAddr = tc.remote
			if tc.header != "" {
				request.Header.Set("X-Forwarded-Proto", tc.header)
			}
			recorder := httptest.NewRecorder()
			hub.hub.ServeHTTP(recorder, request)
			// Past the TLS check, an unconfigured hub answers 404.
			want := http.StatusNotFound
			if !tc.secure {
				want = http.StatusForbidden
			}
			if recorder.Code != want {
				t.Fatalf("status = %d, want %d", recorder.Code, want)
			}
		})
	}
}

// TestHubPlainHTTPToggleAppliesWithoutRestart reads the setting on each
// request (#664): turning it on admits a plain-HTTP worker, turning it off
// refuses the next one.
func TestHubPlainHTTPToggleAppliesWithoutRestart(t *testing.T) {
	var allow atomic.Bool
	hub := newTestHubWithConfig(t, HubConfig{AllowPlainHTTP: allow.Load})
	status := func() int {
		request := httptest.NewRequest(http.MethodGet, PathConnect, nil)
		request.RemoteAddr = "192.0.2.10:5000"
		recorder := httptest.NewRecorder()
		hub.hub.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if got := status(); got != http.StatusForbidden {
		t.Fatalf("off: status = %d, want 403", got)
	}
	allow.Store(true)
	// Past the TLS check, an unconfigured hub answers 404.
	if got := status(); got != http.StatusNotFound {
		t.Fatalf("on: status = %d, want 404", got)
	}
	allow.Store(false)
	if got := status(); got != http.StatusForbidden {
		t.Fatalf("off again: status = %d, want 403", got)
	}
}

// A worker that cannot save its identity must not enroll: the server would
// keep an orphan identity at each restart.
func TestUnwritableStateDirectoryFailsBeforeEnrollment(t *testing.T) {
	token := mustToken(t)
	hub := newTestHub(t, token)
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := NewAgent(AgentConfig{
		ServerURL: hub.server.URL, StateDir: dir, JoinToken: token,
		Name: "w", WorkDir: t.TempDir(), Capabilities: Capabilities{MaxJobs: 1}, Clock: clock.NewFake(testEpoch),
	})
	if err == nil || !strings.Contains(err.Error(), "worker state directory") {
		t.Fatalf("NewAgent error = %v, want a state directory error", err)
	}
	all, err := hub.workers.Workers(context.Background())
	if err != nil || len(all) != 0 {
		t.Fatalf("workers = %+v, %v; want none", all, err)
	}
}
