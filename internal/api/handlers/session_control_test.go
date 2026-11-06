// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/auth"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/sessioncontrol"
	"github.com/deadfin-org/deadfin/internal/ws"
)

func TestSessionControlRoutesAuthorizeAndEmitJellyfinCommands(t *testing.T) {
	h := newP104Harness(t)
	admin := h.createUser(t, "Remote administrator", "admin password", json.RawMessage(`{"IsAdministrator":true,"EnableRemoteControlOfOtherUsers":true,"EnableAllDevices":true}`), json.RawMessage(`{}`))
	alice := h.createUser(t, "Remote Alice", "alice password", json.RawMessage(`{"IsAdministrator":false,"EnableRemoteControlOfOtherUsers":false,"EnableAllDevices":true}`), json.RawMessage(`{}`))
	bob := h.createUser(t, "Remote Bob", "bob password", json.RawMessage(`{"IsAdministrator":false,"EnableRemoteControlOfOtherUsers":false,"EnableAllDevices":true}`), json.RawMessage(`{}`))
	h.identities["control-admin"] = auth.Identity{Authenticated: true, Administrator: true, UserID: admin.ID, Client: "Web", DeviceID: "admin-browser", Version: "12"}
	h.identities["control-alice"] = auth.Identity{Authenticated: true, UserID: alice.ID, Client: "Web", DeviceID: "alice-browser", Version: "12"}
	h.identities["control-bob"] = auth.Identity{Authenticated: true, UserID: bob.ID, Client: "Web", DeviceID: "bob-browser", Version: "12"}

	target, err := h.sessions.LogActivity(t.Context(), session.Activity{
		Client: "Swiftfin", Version: "1", DeviceID: "apple-tv", DeviceName: "Apple TV", UserID: bob.ID,
	})
	if err != nil {
		t.Fatalf("create target session: %v", err)
	}
	targetID := target.ID
	if err := h.sessions.ReportCapabilities(t.Context(), session.CapabilityReport{
		ID: &targetID, Capabilities: session.Capabilities{SupportsMediaControl: true},
	}); err != nil {
		t.Fatalf("enable target media control: %v", err)
	}

	sender := &capturingSessionSender{}
	handlers.RegisterSessionControl(h.rt, sessioncontrol.New(h.sessions, h.users, sender), nil)
	itemID := p104UUID(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	calls := []struct {
		path string
		body string
	}{
		{path: "/Sessions/" + target.ID + "/Playing?PlayCommand=playnow&ItemIds=" + itemID.Dashed() + "&StartPositionTicks=500&MediaSourceId=source"},
		{path: "/Sessions/" + target.ID + "/Playing/pause?SeekPositionTicks=700"},
		{path: "/Sessions/" + target.ID + "/Command/volumeup"},
		{path: "/Sessions/" + target.ID + "/Message", body: `{"Text":"Hello","TimeoutMs":2500}`},
		{path: "/Sessions/" + target.ID + "/Viewing?ItemType=Movie&ItemId=" + itemID.Dashed() + "&ItemName=Example"},
	}
	for _, call := range calls {
		response := p104Call(h.rt, http.MethodPost, call.path, call.body, "application/json", "", "control-admin", "")
		assertStatus(t, response, http.StatusNoContent)
	}

	got := sender.commands
	if len(got) != 5 {
		t.Fatalf("routed commands = %#v, want five", got)
	}
	play, ok := got[0].data.(dto.PlayRequest)
	if !ok || got[0].messageType != ws.MessagePlay || play.PlayCommand != "PlayNow" ||
		!reflect.DeepEqual(play.ItemIDs, []dto.UUID{itemID}) || play.ControllingUserID != admin.ID ||
		play.StartPositionTicks == nil || *play.StartPositionTicks != 500 || play.MediaSourceID == nil || *play.MediaSourceID != "source" {
		t.Fatalf("Play command = %#v", got[0])
	}
	playstate, ok := got[1].data.(dto.PlaystateRequest)
	if !ok || got[1].messageType != ws.MessagePlaystate || playstate.Command != "Pause" ||
		playstate.ControllingUserID == nil || *playstate.ControllingUserID != admin.ID.String() ||
		playstate.SeekPositionTicks == nil || *playstate.SeekPositionTicks != 700 {
		t.Fatalf("Playstate command = %#v", got[1])
	}
	volume := got[2].data.(dto.GeneralCommand)
	if volume.Name != "VolumeUp" || volume.ControllingUserID != admin.ID || len(volume.Arguments) != 0 {
		t.Fatalf("GeneralCommand = %#v", volume)
	}
	message := got[3].data.(dto.GeneralCommand)
	if message.Name != "DisplayMessage" || !reflect.DeepEqual(message.Arguments, map[string]string{
		"Header": "Message from Server", "Text": "Hello", "TimeoutMs": "2500",
	}) {
		t.Fatalf("Message command = %#v", message)
	}
	view := got[4].data.(dto.GeneralCommand)
	if view.Name != "DisplayContent" || !reflect.DeepEqual(view.Arguments, map[string]string{
		"ItemId": itemID.Dashed(), "ItemName": "Example", "ItemType": "Movie",
	}) {
		t.Fatalf("Viewing command = %#v", view)
	}

	before := len(sender.commands)
	forbidden := p104Call(h.rt, http.MethodPost, "/Sessions/"+target.ID+"/Playing/Pause", "", "", "", "control-alice", "")
	assertStatus(t, forbidden, http.StatusForbidden)
	if len(sender.commands) != before {
		t.Fatal("forbidden remote command reached controller")
	}

	own := p104Call(h.rt, http.MethodPost, "/Sessions/"+target.ID+"/Playing/Pause", "", "", "", "control-bob", "")
	assertStatus(t, own, http.StatusNoContent)
	missing := p104Call(h.rt, http.MethodPost, "/Sessions/missing/Playing/Pause", "", "", "", "control-admin", "")
	assertStatus(t, missing, http.StatusNotFound)
	invalid := p104Call(h.rt, http.MethodPost, "/Sessions/"+target.ID+"/Playing/NotACommand", "", "", "", "control-admin", "")
	assertStatus(t, invalid, http.StatusBadRequest)
}

type capturedSessionCommand struct {
	sessionID   string
	messageType ws.MessageType
	data        any
}

type capturingSessionSender struct {
	commands []capturedSessionCommand
}

func (sender *capturingSessionSender) Send(_ context.Context, sessionID string, messageType ws.MessageType, data any) error {
	sender.commands = append(sender.commands, capturedSessionCommand{sessionID: sessionID, messageType: messageType, data: data})
	return nil
}
