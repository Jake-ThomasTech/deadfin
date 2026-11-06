// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/store/storetest"
	"github.com/deadfin-org/deadfin/internal/ws"
	"golang.org/x/net/websocket"
)

// TestSessionsAndEventsAcrossReplicas is the #663 acceptance: two replicas
// on one store. A dashboard on replica A lists a session that replica B
// owns, a remote-control command from A reaches the client's WebSocket on
// B, and a user data change on A reaches that WebSocket. The store is
// PostgreSQL when DEADFIN_TEST_POSTGRES_DSN is set, else one SQLite file.
func TestSessionsAndEventsAcrossReplicas(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	options := []applicationOption{withReplicaOnSQLite()}
	if os.Getenv("DEADFIN_TEST_POSTGRES_DSN") != "" {
		options = []applicationOption{withDatabaseURL(storetest.PostgresSchemaURL(t))}
	}
	programData := filepath.Join(t.TempDir(), "program-data")
	listenerA, listenerB := newReplicaServer(t), newReplicaServer(t)
	appA := newCompositionApplication(t, programData, append(options, withReplicaURL(listenerA.server.URL))...)
	listenerA.serve(appA)
	configureComposedAdministrator(t, appA.Router())
	movieID := seedPlaystateTracerMovie(t, appA, 20*60*10_000_000)
	appB := newCompositionApplication(t, programData, append(options, withReplicaURL(listenerB.server.URL))...)
	listenerB.serve(appB)
	waitFor(t, func() bool {
		return appA.replicas.registry.Refresh(t.Context()) == nil && appA.replicas.registry.HasPeer() &&
			appB.replicas.registry.Refresh(t.Context()) == nil && appB.replicas.registry.HasPeer()
	})

	// The TV signs in on B, reports that it takes remote control and
	// opens its WebSocket there. The dashboard signs in on A.
	tvToken := replicaLogin(t, listenerB, "tv")
	if code, body := replicaCall(t, listenerB, http.MethodPost, "/Sessions/Capabilities/Full", "tv", tvToken,
		`{"PlayableMediaTypes":["Video"],"SupportedCommands":["DisplayMessage"],"SupportsMediaControl":true}`); code != http.StatusNoContent {
		t.Fatalf("TV capabilities = %d %s", code, body)
	}
	tvSocket := replicaSocket(t, listenerB, "tv", tvToken)
	dashboardToken := replicaLogin(t, listenerA, "dashboard")
	tvSession, dashboardSession := session.IDFor("Deadfin Test", "tv"), session.IDFor("Deadfin Test", "dashboard")
	if _, found := appB.sessions.SessionByID(tvSession); !found {
		t.Fatal("B does not hold the TV's session")
	}
	if _, found := appA.sessions.SessionByID(tvSession); found {
		t.Fatal("A holds a copy of the TV's session")
	}

	// GET /Sessions on either replica lists both sessions once.
	for _, listener := range []*replicaServer{listenerA, listenerB} {
		code, body := replicaCall(t, listener, http.MethodGet, "/Sessions", "dashboard", dashboardToken, "")
		var listed []dto.SessionInfoDto
		if code != http.StatusOK || json.Unmarshal(body, &listed) != nil {
			t.Fatalf("GET /Sessions = %d %s", code, body)
		}
		ids := make([]string, 0, len(listed))
		for _, value := range listed {
			if value.ID != nil {
				ids = append(ids, *value.ID)
			}
		}
		slices.Sort(ids)
		want := []string{tvSession, dashboardSession}
		slices.Sort(want)
		if !slices.Equal(ids, want) {
			t.Fatalf("GET /Sessions through %s = %v, want %v", listener.server.URL, ids, want)
		}
	}

	// The dashboard's Sessions feed on A holds the TV's session.
	dashboardSocket := replicaSocket(t, listenerA, "dashboard", dashboardToken)
	if err := websocket.JSON.Send(dashboardSocket, ws.InboundEnvelope{MessageType: ws.MessageSessionsStart, Data: json.RawMessage(`"0,1500"`)}); err != nil {
		t.Fatal(err)
	}
	feed := receiveSocketMessage(t, dashboardSocket, ws.MessageSessions)
	if !strings.Contains(string(feed), tvSession) {
		t.Fatalf("A's Sessions feed has no TV session: %s", feed)
	}

	// A remote-control command from A reaches the TV on B.
	if code, body := replicaCall(t, listenerA, http.MethodPost, "/Sessions/"+tvSession+"/Message", "dashboard", dashboardToken,
		`{"Header":"Deadfin","Text":"hello from A"}`); code != http.StatusNoContent {
		t.Fatalf("message from A = %d %s", code, body)
	}
	command := receiveSocketMessage(t, tvSocket, ws.MessageGeneralCommand)
	if !strings.Contains(string(command), "hello from A") {
		t.Fatalf("TV command = %s", command)
	}
	if _, found := appB.sessions.SessionByID(dashboardSession); found {
		t.Fatal("the command left a copy of the dashboard's session on B")
	}

	// A user data change on A, the play count of a playback start,
	// reaches the TV's WebSocket on B: both belong to the same user.
	start := `{"ItemId":"` + movieID.Dashed() + `","CanSeek":true,"PositionTicks":0,"PlayMethod":"DirectPlay"}`
	if code, body := replicaCall(t, listenerA, http.MethodPost, "/Sessions/Playing", "dashboard", dashboardToken, start); code != http.StatusNoContent {
		t.Fatalf("playback start on A = %d %s", code, body)
	}
	changed := receiveSocketMessage(t, tvSocket, ws.MessageUserDataChanged)
	if !strings.Contains(string(changed), movieID.String()) && !strings.Contains(string(changed), movieID.Dashed()) {
		t.Fatalf("TV user data change = %s", changed)
	}
}

func replicaAuthorization(device, token string) string {
	authorization := `MediaBrowser Client="Deadfin Test", Device="Deadfin ` + device + `", DeviceId="` + device + `", Version="1.0"`
	if token != "" {
		authorization += `, Token="` + token + `"`
	}
	return authorization
}

func replicaCall(t *testing.T, listener *replicaServer, method, path, device, token, body string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, listener.server.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Authorization", replicaAuthorization(device, token))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, data
}

func replicaLogin(t *testing.T, listener *replicaServer, device string) string {
	t.Helper()
	code, body := replicaCall(t, listener, http.MethodPost, "/Users/AuthenticateByName", device, "", `{"Username":"captain","Pw":"correct horse battery staple"}`)
	var result struct {
		AccessToken string `json:"AccessToken"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &result) != nil || result.AccessToken == "" {
		t.Fatalf("login of %s = %d %s", device, code, body)
	}
	return result.AccessToken
}

func replicaSocket(t *testing.T, listener *replicaServer, device, token string) *websocket.Conn {
	t.Helper()
	config, err := websocket.NewConfig("ws"+strings.TrimPrefix(listener.server.URL, "http")+"/socket", listener.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	config.Header.Set("Authorization", replicaAuthorization(device, token))
	socket, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatalf("WebSocket of %s: %v", device, err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	return socket
}

// receiveSocketMessage reads socket until a message of kind arrives and
// returns its data.
func receiveSocketMessage(t *testing.T, socket *websocket.Conn, kind ws.MessageType) json.RawMessage {
	t.Helper()
	if err := socket.SetReadDeadline(clock.NewReal().Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = socket.SetReadDeadline(time.Time{}) }()
	for {
		var message struct {
			MessageType ws.MessageType  `json:"MessageType"`
			Data        json.RawMessage `json:"Data"`
		}
		if err := websocket.JSON.Receive(socket, &message); err != nil {
			t.Fatalf("wait for %s: %v", kind, err)
		}
		if message.MessageType == kind {
			return message.Data
		}
	}
}
