// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/events"
	"github.com/deadfin-org/deadfin/internal/session"
)

const (
	knownProxyPeer = "10.10.50.80:44000"
	remoteClient   = "203.0.113.7"
	lanClient      = "10.10.20.15"
)

// proxiedRequest sends a request from remote with X-Forwarded-For set to
// forwardedFor, when it is not empty.
func proxiedRequest(handler http.Handler, method, path string, body []byte, remote, forwardedFor, device, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.RemoteAddr = remote
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if forwardedFor != "" {
		request.Header.Set("X-Forwarded-For", forwardedFor)
	}
	authorization := fmt.Sprintf(`MediaBrowser Client="Deadfin Test", Device="Deadfin %s", DeviceId="%s", Version="1.0"`, device, device)
	if token != "" {
		authorization += `, Token="` + token + `"`
	}
	request.Header.Set("Authorization", authorization)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestKnownProxyForwardsTheClientAddress is the #864 acceptance: with
// KnownProxies set, X-Forwarded-For from a listed proxy gives the client
// address that the setup gate, the session, the activity log, the
// local/remote bitrate rule and the QuickConnect limits see.
func TestKnownProxyForwardsTheClientAddress(t *testing.T) {
	app := newCompositionApplication(t, filepath.Join(t.TempDir(), "program-data"), withSetupRandom(bytes.NewReader(make([]byte, 32))))
	handler := app.Router()
	updateNetwork := func(apply func(*config.NetworkConfiguration)) {
		t.Helper()
		if err := app.manager.UpdateNetwork(t.Context(), func(network *config.NetworkConfiguration) error {
			apply(network)
			return nil
		}); err != nil {
			t.Fatalf("UpdateNetwork: %v", err)
		}
	}

	// 1. With no KnownProxies the header is ignored: the transport peer is
	// the client.
	if got := proxiedRequest(handler, http.MethodGet, "/Startup/User", nil, knownProxyPeer, lanClient, "setup", "").Code; got != http.StatusForbidden {
		t.Fatalf("proxied wizard request with no KnownProxies = %d, want 403", got)
	}

	// 5. D-53: while setup is pending, a request from a known proxy is not
	// local, whatever X-Forwarded-For says.
	updateNetwork(func(network *config.NetworkConfiguration) {
		network.KnownProxies = []string{"10.10.50.80", "10.10.50.81"}
	})
	for _, forwardedFor := range []string{lanClient, "127.0.0.1"} {
		if got := proxiedRequest(handler, http.MethodGet, "/Startup/User", nil, knownProxyPeer, forwardedFor, "setup", "").Code; got != http.StatusForbidden {
			t.Fatalf("proxied wizard request for %s from a known proxy = %d, want 403", forwardedFor, got)
		}
	}

	configureComposedAdministrator(t, handler)
	updateNetwork(func(network *config.NetworkConfiguration) {
		network.LocalNetworkSubnets = []string{"10.10.20.0/24"}
	})
	if err := app.manager.UpdateSystem(t.Context(), func(system *config.ServerConfiguration) error {
		system.RemoteClientBitrateLimit = 1_000_000
		return nil
	}); err != nil {
		t.Fatalf("UpdateSystem: %v", err)
	}
	movieID := seedPlaystateTracerMovie(t, app, 20*60*10_000_000)
	login := []byte(`{"Username":"captain","Pw":"correct horse battery staple"}`)

	// 2. A remote client through the proxy: the session and the auth log
	// hold its address, and the remote bitrate limit applies.
	signIn := proxiedRequest(handler, http.MethodPost, "/Users/AuthenticateByName", login, knownProxyPeer, remoteClient, "tv", "")
	if signIn.Code != http.StatusOK {
		t.Fatalf("proxied login = %d %s", signIn.Code, signIn.Body.String())
	}
	token := compositionAccessToken(t, signIn)
	remoteEndPoint := func(device string) string {
		t.Helper()
		snapshot, found := app.sessions.SessionByID(session.IDFor("Deadfin Test", device))
		if !found {
			t.Fatalf("no session for %s", device)
		}
		return snapshot.RemoteEndPoint
	}
	if got := remoteEndPoint("tv"); got != remoteClient {
		t.Fatalf("session remote end point after a proxied login = %q, want %s", got, remoteClient)
	}
	// The activity log writes entries on its own worker; wait for the
	// committed entry before the read.
	committed := app.activity.Subscribe(events.SubscriptionOptions{Buffer: 16})
	t.Cleanup(func() { _ = committed.Close(context.Background()) })
	if got := proxiedRequest(handler, http.MethodPost, "/Users/AuthenticateByName", []byte(`{"Username":"captain","Pw":"wrong"}`), knownProxyPeer, remoteClient, "intruder", "").Code; got != http.StatusUnauthorized {
		t.Fatalf("proxied failed login = %d, want 401", got)
	}
	for failed := false; !failed; {
		select {
		case entry, ok := <-committed.Events():
			if !ok {
				t.Fatal("activity subscription closed before the failed login entry")
			}
			failed = entry.Type == "AuthenticationFailed"
		case <-t.Context().Done():
			t.Fatal("no failed login entry committed")
		}
	}
	activity := proxiedRequest(handler, http.MethodGet, "/System/ActivityLog/Entries", nil, knownProxyPeer, lanClient, "tv", token)
	if activity.Code != http.StatusOK || !strings.Contains(activity.Body.String(), `"AuthenticationFailed"`) || !strings.Contains(activity.Body.String(), remoteClient) {
		t.Fatalf("activity log = %d %s, want a failed login from %s", activity.Code, activity.Body.String(), remoteClient)
	}

	directPlay := func(forwardedFor string) bool {
		t.Helper()
		response := proxiedRequest(handler, http.MethodPost, "/Items/"+movieID.Dashed()+"/PlaybackInfo", []byte(`{"MaxStreamingBitrate":100000000,"DeviceProfile":{"DirectPlayProfiles":[{"Container":"mkv","Type":"Video","VideoCodec":"h264"}],"TranscodingProfiles":[{"Container":"ts","Type":"Video","VideoCodec":"h264","AudioCodec":"aac","Protocol":"hls","Context":"Streaming"}]}}`), knownProxyPeer, forwardedFor, "tv", token)
		var info struct {
			MediaSources []struct{ SupportsDirectPlay bool }
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &info) != nil || len(info.MediaSources) == 0 {
			t.Fatalf("PlaybackInfo for %s = %d %s", forwardedFor, response.Code, response.Body.String())
		}
		return info.MediaSources[0].SupportsDirectPlay
	}
	endpoint := func(remote, forwardedFor string) bool {
		t.Helper()
		response := proxiedRequest(handler, http.MethodGet, "/System/Endpoint", nil, remote, forwardedFor, "tv", token)
		var info struct{ IsInNetwork bool }
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &info) != nil {
			t.Fatalf("GET /System/Endpoint for %s via %s = %d %s", forwardedFor, remote, response.Code, response.Body.String())
		}
		return info.IsInNetwork
	}
	if endpoint(knownProxyPeer, remoteClient) {
		t.Fatalf("%s through the proxy is in network", remoteClient)
	}
	if directPlay(remoteClient) {
		t.Fatal("the 8.5 Mbps movie plays direct for a remote client over the 1 Mbps remote limit")
	}

	// 2. A LAN client through the proxy is local: no remote limit.
	if !endpoint(knownProxyPeer, lanClient) {
		t.Fatalf("%s through the proxy is not in network", lanClient)
	}
	if got := remoteEndPoint("tv"); got != lanClient {
		t.Fatalf("session remote end point = %q, want %s", got, lanClient)
	}
	if !directPlay(lanClient) {
		t.Fatal("the movie does not play direct for a LAN client")
	}

	// 3. A peer that is not a known proxy cannot pass an address.
	if endpoint("198.51.100.20:5000", lanClient) {
		t.Fatal("X-Forwarded-For from an unlisted peer was trusted")
	}
	if got := remoteEndPoint("tv"); got != "198.51.100.20" {
		t.Fatalf("session remote end point = %q, want the unlisted peer", got)
	}

	// 4. A chain through two known proxies gives the first unknown hop.
	if endpoint(knownProxyPeer, "198.51.100.9, 10.10.50.81") {
		t.Fatal("the chained client is in network")
	}
	if got := remoteEndPoint("tv"); got != "198.51.100.9" {
		t.Fatalf("session remote end point = %q, want 198.51.100.9", got)
	}

	// 6. D-55: the QuickConnect limit counts each client, not the proxy.
	initiate := func(forwardedFor string, attempt int) int {
		return proxiedRequest(handler, http.MethodPost, "/QuickConnect/Initiate", nil, knownProxyPeer, forwardedFor, fmt.Sprintf("qc-%s-%d", forwardedFor, attempt), "").Code
	}
	for attempt := range 16 {
		if got := initiate(remoteClient, attempt); got != http.StatusOK {
			t.Fatalf("QuickConnect initiate %d from %s = %d, want 200", attempt, remoteClient, got)
		}
	}
	if got := initiate(remoteClient, 16); got != http.StatusTooManyRequests {
		t.Fatalf("17th QuickConnect initiate from %s = %d, want 429", remoteClient, got)
	}
	if got := initiate(lanClient, 0); got != http.StatusOK {
		t.Fatalf("QuickConnect initiate from %s behind the same proxy = %d, want 200", lanClient, got)
	}
}
