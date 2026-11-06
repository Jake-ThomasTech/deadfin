// SPDX-License-Identifier: GPL-2.0-only

package replica

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/session"
)

// TestSessionListAndCommandsSpanReplicas covers #663: GET /Sessions lists
// the sessions of every replica, and a command for a session of another
// replica reaches the replica that holds it.
func TestSessionListAndCommandsSpanReplicas(t *testing.T) {
	leakCheck(t)
	c := newCluster(t)
	a, b := c.start("a"), c.start("b")
	c.refresh(a, b)

	if code, answer, _ := call(t, a, http.MethodGet, "/Sessions/Playing", "tok-phone", ""); code != http.StatusOK || answer.Replica != "a" {
		t.Fatalf("phone on a = %d %+v", code, answer)
	}
	if code, answer, _ := call(t, b, http.MethodGet, "/Sessions/Playing", "tok-tv", ""); code != http.StatusOK || answer.Replica != "b" {
		t.Fatalf("tv on b = %d %+v", code, answer)
	}
	phone, tv := session.IDFor("app", "phone"), session.IDFor("app", "tv")

	for _, through := range []*testReplica{a, b} {
		ids := listSessions(t, through, "tok-phone")
		slices.Sort(ids)
		want := []string{phone, tv}
		slices.Sort(want)
		if !slices.Equal(ids, want) {
			t.Fatalf("GET /Sessions through %s = %v, want %v", through.name, ids, want)
		}
	}

	code, answer, _ := call(t, a, http.MethodPost, "/Sessions/"+tv+"/Message", "tok-phone", `{"Text":"hi"}`)
	if code != http.StatusOK || answer.Replica != "b" || !strings.HasPrefix(answer.Remote, "127.0.0.1:") {
		t.Fatalf("command from a for b's session = %d %+v", code, answer)
	}
	select {
	case message := <-b.messages:
		if message != `{"Text":"hi"}` {
			t.Fatalf("message on b = %q", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command did not reach b")
	}
	if _, found := b.sessions.SessionByID(phone); found {
		t.Fatal("the command left a copy of the phone's session on b")
	}
	if code, _, _ := call(t, a, http.MethodPost, "/Sessions/unknown/Message", "tok-phone", `{}`); code != http.StatusNotFound {
		t.Fatalf("command for an unknown session = %d", code)
	}
}

func listSessions(t *testing.T, replica *testReplica, token string) []string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, replica.server.URL+"/Sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", `MediaBrowser Client="app", Device="d", DeviceId="ignored", Version="1", Token="`+token+`"`)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var ids []string
	if err := json.NewDecoder(response.Body).Decode(&ids); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("GET /Sessions = %d, %v", response.StatusCode, err)
	}
	return ids
}

// TestEventsReachThePeers covers #663: a published event reaches every live
// peer, and the peer paths answer only a signed request.
func TestEventsReachThePeers(t *testing.T) {
	leakCheck(t)
	c := newCluster(t)
	a, b := c.start("a"), c.start("b")
	c.refresh(a, b)

	a.cluster.Publish(map[string]int{"n": 1})
	a.cluster.Publish(map[string]int{"n": 2})
	for _, want := range []string{`{"n":1}`, `{"n":2}`} {
		select {
		case event := <-b.events:
			if string(event) != want {
				t.Fatalf("event on b = %s, want %s", event, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %s did not reach b", want)
		}
	}
	select {
	case event := <-a.events:
		t.Fatalf("a received its own event %s", event)
	default:
	}

	response, err := http.Post(b.server.URL+PathEvents, "application/json", strings.NewReader(`[{"n":3}]`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unsigned event post = %d", response.StatusCode)
	}

	// Publish after Close does nothing.
	a.cluster.Close()
	a.cluster.Publish(map[string]int{"n": 4})
}

// TestSignerChecksTheScope: the local scope is signed, and a forward without
// it keeps the signature of a replica that predates it.
func TestSignerChecksTheScope(t *testing.T) {
	fake := clock.NewFake(testEpoch)
	signer, _ := NewSigner([]byte(testSecret), fake)
	request := httptest.NewRequest(http.MethodGet, "/Sessions", nil)
	signer.Sign(request.Header, request.Method, request.RequestURI, Forwarded{From: "a", Peer: "10.0.0.1:5", Local: true})
	if forwarded, ok := signer.Verify(request); !ok || !forwarded.Local {
		t.Fatalf("Verify = %+v, %v", forwarded, ok)
	}
	request.Header.Del(HeaderScope)
	if _, ok := signer.Verify(request); ok {
		t.Fatal("a forward without its signed scope passed")
	}
	plain := httptest.NewRequest(http.MethodGet, "/Sessions", nil)
	signer.Sign(plain.Header, plain.Method, plain.RequestURI, Forwarded{From: "a", Peer: "10.0.0.1:5"})
	if plain.Header.Get(HeaderScope) != "" {
		t.Fatal("a forward without the local scope has a scope header")
	}
	plain.Header.Set(HeaderScope, "local")
	if _, ok := signer.Verify(plain); ok {
		t.Fatal("an added scope passed")
	}
}
