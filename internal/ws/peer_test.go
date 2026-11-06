// SPDX-License-Identifier: GPL-2.0-only

package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/session"
	"golang.org/x/net/websocket"
)

// TestPeerEventsAndSessions covers #663: the server hands its session
// changes to the peers, sends a peer's events to its sockets, and the
// Sessions feed holds the peers' sessions too.
func TestPeerEventsAndSessions(t *testing.T) {
	fake := clock.NewFake(time.Unix(1_700_000_000, 0))
	sessions := session.New(fake)
	defer func() { _ = sessions.Close() }()
	adminID := testUUID(t, "33333333333333333333333333333333")
	local, err := sessions.LogActivity(t.Context(), session.Activity{
		Client: "Dashboard", Version: "12.0", DeviceID: "browser", DeviceName: "Browser", UserID: adminID,
	})
	if err != nil {
		t.Fatal(err)
	}

	relayed := make(chan PeerEvent, 64)
	peerID := "peer-session"
	queries := make(chan SessionsQuery, 4)
	server := New(Config{
		Clock: fake, Sessions: sessions,
		Relay: func(event PeerEvent) {
			select {
			case relayed <- event:
			default:
			}
		},
		PeerSessions: func(_ context.Context, query SessionsQuery) []dto.SessionInfoDto {
			queries <- query
			return []dto.SessionInfoDto{{ID: &peerID, LastActivityDate: dto.NewTime(fake.Now().Add(time.Minute))}}
		},
	})
	defer func() { _ = server.Close(t.Context()) }()

	if _, err := sessions.LogActivity(t.Context(), session.Activity{
		Client: "Swiftfin", Version: "1.0", DeviceID: "apple-tv", DeviceName: "Apple TV", UserID: adminID,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-relayed:
		if event.Kind != MessageSessions {
			t.Fatalf("relayed event = %+v, want a session change", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the session change did not reach the relay")
	}

	identity := auth.Identity{Authenticated: true, Administrator: true, UserID: adminID, Client: "Dashboard", Version: "12.0", DeviceID: "browser"}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		server.serveAuthenticated(w, request, identity)
	}))
	defer httpServer.Close()
	socket, err := websocket.Dial("ws"+strings.TrimPrefix(httpServer.URL, "http"), "", httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = socket.Close() }()
	if first := receiveTestEnvelope(t, socket); first.MessageType != MessageForceKeepAlive {
		t.Fatalf("first envelope = %+v", first)
	}

	added := testUUID(t, "44444444444444444444444444444444")
	server.ReceivePeerEvents([]PeerEvent{{Kind: MessageLibraryChanged, Library: &library.UpdateInfo{ItemsAdded: []dto.UUID{added}}}})
	message := receiveTestEnvelope(t, socket)
	var info library.UpdateInfo
	if message.MessageType != MessageLibraryChanged || json.Unmarshal(message.Data, &info) != nil || len(info.ItemsAdded) != 1 || info.ItemsAdded[0] != added {
		t.Fatalf("peer library change = %s %s", message.MessageType, message.Data)
	}

	sendTestEnvelope(t, socket, InboundEnvelope{MessageType: MessageSessionsStart, Data: json.RawMessage(`"0,1000"`)})
	message = receiveTestEnvelope(t, socket)
	var projected []dto.SessionInfoDto
	if message.MessageType != MessageSessions || json.Unmarshal(message.Data, &projected) != nil {
		t.Fatalf("Sessions envelope = %s %s", message.MessageType, message.Data)
	}
	if len(projected) != 3 || projected[0].ID == nil || *projected[0].ID != peerID {
		t.Fatalf("Sessions feed = %d sessions, first %v; want the peer's newest session first of 3", len(projected), projected[0].ID)
	}
	if query := <-queries; !query.All || query.UserID != adminID {
		t.Fatalf("peer query = %+v", query)
	}
	found := false
	for _, value := range projected {
		found = found || (value.ID != nil && *value.ID == local.ID)
	}
	if !found {
		t.Fatal("the Sessions feed lost the local session")
	}
}
