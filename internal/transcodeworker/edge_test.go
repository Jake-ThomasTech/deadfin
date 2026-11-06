// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/edge"
)

// TestEdgeServesSegmentsThroughAServerRestart is the #619 path: the worker
// mirrors each completed segment to the stream edge. While the server
// process is down, the edge serves the segments the worker makes to a
// client that holds a read grant, and waits for one that is not made yet.
// The next server process adopts the job, as in #618.
func TestEdgeServesSegmentsThroughAServerRestart(t *testing.T) {
	token := mustToken(t)
	rig := newRestartRig(t, token)
	signer, err := edge.NewSigner("shared-edge-key-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	upstream, _ := url.Parse(rig.server.URL)
	edgeHandler, err := edge.New(edge.Config{Signer: signer, Upstream: upstream, StoreDirectory: filepath.Join(rig.root, "edge"), Clock: rig.clock, WaitTimeout: testTimeout})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(edgeHandler)
	t.Cleanup(func() {
		front.Close()
		edgeHandler.Close()
	})
	rig.edge = EdgeTarget{URL: front.URL, Signer: signer}

	first := rig.start(t, true)
	worker := rig.agent(t, token)
	first.waitEvent(t, EventRegistered)
	request := rig.hlsRequest(t)
	if err := first.manager.StartHLS(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	process := worker.process(t)
	job := worker.heldJob(t)
	if job.spec.EdgeURL != front.URL || job.spec.EdgeGrant == "" {
		t.Fatalf("job spec edge = %q, grant %q", job.spec.EdgeURL, job.spec.EdgeGrant)
	}
	item := dto.UUID{0x61, 0x19}
	grant := signer.SignRead(edge.ReadGrant{ItemID: item, PlaySessionID: "play", Family: edge.Family{Stem: "abc", Pattern: "abc%d.ts"}, Expires: rig.clock.Now().Add(time.Hour)})
	segmentURL := func(index string) string {
		return front.URL + "/videos/" + item.Dashed() + "/hls1/main/" + index + ".ts?PlaySessionId=play&" + edge.GrantQuery + "=" + url.QueryEscape(grant)
	}
	fetch := func(index string) (int, string) {
		response, err := http.Get(segmentURL(index))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, tsText(body)
	}
	mirrored := func(name string) bool {
		_, err := os.Stat(filepath.Join(rig.root, "edge", "abc", name))
		return err == nil
	}

	process.write(t, "abc0.ts", "zero")
	process.write(t, "abc1.ts", "on")
	eventually(t, worker.poll, func() bool {
		_, err := os.Stat(filepath.Join(rig.dir, "abc0.ts"))
		return err == nil && mirrored("abc0.ts")
	})
	if code, body := fetch("0"); code != http.StatusOK || body != "zero" {
		t.Fatalf("segment 0 through the edge = %d %q", code, body)
	}

	// The server process stops. The worker keeps mirroring to the edge.
	first.stop(t, rig)
	eventually(t, nil, job.detached)
	process.write(t, "abc1.ts", "one")
	process.write(t, "abc2.ts", "tw")
	for step := 0; step < 20 && !mirrored("abc1.ts"); step++ {
		worker.poll()
		time.Sleep(10 * time.Millisecond)
	}
	if code, body := fetch("1"); code != http.StatusOK || body != "one" {
		t.Fatalf("segment 1 through the edge while the server is down = %d %q", code, body)
	}
	if _, err := os.Stat(filepath.Join(rig.dir, "abc1.ts")); err == nil {
		if data, _ := os.ReadFile(filepath.Join(rig.dir, "abc1.ts")); tsText(data) == "one" {
			t.Fatal("the server received a segment while it was down")
		}
	}
	type result struct {
		code int
		body string
	}
	waiting := make(chan result, 1)
	go func() {
		code, body := fetch("2")
		waiting <- result{code, body}
	}()
	process.write(t, "abc3.ts", "three")
	for step := 0; step < 20 && !mirrored("abc2.ts"); step++ {
		worker.poll()
		time.Sleep(10 * time.Millisecond)
	}
	if got := <-waiting; got.code != http.StatusOK || got.body != "tw" {
		t.Fatalf("segment 2, requested before the worker made it = %d %q", got.code, got.body)
	}

	// The next server process adopts the job; the server's copy catches up.
	second := rig.start(t, true)
	reconnect(t, worker, second, EventJobAdopted)
	eventually(t, worker.poll, func() bool {
		data, err := os.ReadFile(filepath.Join(rig.dir, "abc2.ts"))
		return !job.detached() && err == nil && tsText(data) == "tw"
	})
	if got := readSegment(t, second.manager, request.PlaylistPath, 1); got != "one" {
		t.Fatalf("segment 1 from the adopted job = %q", got)
	}
	noLaunch(t, worker)
}
