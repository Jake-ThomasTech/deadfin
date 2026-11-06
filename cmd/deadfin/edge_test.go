// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"log/slog"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/edge"
	"github.com/deadfin-org/deadfin/internal/playback"
)

func TestEdgeEnvironment(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	env := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	off, err := edgeEnvironment(env(nil), logger)
	if err != nil || off.signer != nil || off.readGrant(clock.NewReal()) != nil || off.hubTarget().Signer != nil {
		t.Fatalf("no settings = %+v, %v; want the edge off", off, err)
	}
	if got, err := edgeEnvironment(env(map[string]string{envEdgeURL: "http://edge:8097"}), logger); err != nil || got.signer != nil {
		t.Fatalf("URL without a key = %+v, %v; want the edge off", got, err)
	}
	if _, err := edgeEnvironment(env(map[string]string{envEdgeKey: "short"}), logger); err == nil {
		t.Fatal("a short key was accepted")
	}
	if _, err := edgeEnvironment(env(map[string]string{envEdgeKey: "shared-edge-key-0123456789", envEdgeURL: "edge:8097"}), logger); err == nil {
		t.Fatal("a URL with no scheme was accepted")
	}
	readOnly, err := edgeEnvironment(env(map[string]string{envEdgeKey: "shared-edge-key-0123456789"}), logger)
	if err != nil || readOnly.signer == nil || readOnly.hubTarget().Signer != nil {
		t.Fatalf("key without URL = %+v, %v; want grants without worker mirrors", readOnly, err)
	}
	on, err := edgeEnvironment(env(map[string]string{envEdgeKey: "shared-edge-key-0123456789", envEdgeURL: "https://edge.example"}), logger)
	if err != nil || on.hubTarget().URL != "https://edge.example" || on.hubTarget().Signer == nil {
		t.Fatalf("key and URL = %+v, %v", on, err)
	}
	fake := clock.NewFake(time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC))
	item := dto.UUID{0x61}
	token := on.readGrant(fake)(item, "play", "/cache/transcodes/abc.m3u8", "/cache/transcodes/abc%d.ts")
	grant, err := on.signer.VerifyRead(token, fake.Now())
	if err != nil || grant.ItemID != item || grant.PlaySessionID != "play" || grant.Family != (edge.Family{Stem: "abc", Pattern: "abc%d.ts"}) {
		t.Fatalf("read grant = %+v, %v", grant, err)
	}
}

// TestServerEdgeDirectGrants: the server signs a direct grant for each
// source that the static route serves from a library file, and none when
// the edge is off.
func TestServerEdgeDirectGrants(t *testing.T) {
	if (serverEdge{}).directGrants(clock.NewFake(time.Unix(0, 0))) != nil {
		t.Fatal("an edge with no key signs direct grants")
	}
	signer, err := edge.NewSigner("shared-edge-key-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	grants := serverEdge{signer: signer}.directGrants(clock.NewFake(now))
	item := dto.UUID{0x21}
	size := int64(1234)
	dvd := playback.VideoTypeDVD
	file := playback.MediaSourceInfo{Protocol: playback.MediaProtocolFile, ID: "src-1", Path: "/media/a.mkv", Size: &size, Container: "mkv", SupportsDirectPlay: true}
	excluded := []func(*playback.MediaSourceInfo){
		func(s *playback.MediaSourceInfo) { s.Protocol = playback.MediaProtocolHTTP },
		func(s *playback.MediaSourceInfo) { s.Path = "relative.mkv" },
		func(s *playback.MediaSourceInfo) { s.LiveStreamID = "live" },
		func(s *playback.MediaSourceInfo) { s.IsInfiniteStream = true },
		func(s *playback.MediaSourceInfo) { s.SupportsDirectPlay = false },
		func(s *playback.MediaSourceInfo) { s.VideoType = &dvd },
	}
	sources := []playback.MediaSourceInfo{file}
	for _, change := range excluded {
		source := file
		change(&source)
		sources = append(sources, source)
	}
	tokens := grants.DirectGrants(item, "play-1", sources)
	if len(tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(tokens))
	}
	grant, err := signer.VerifyDirect(tokens[0], now)
	if err != nil {
		t.Fatal(err)
	}
	grant.Expires = grant.Expires.UTC()
	want := edge.DirectGrant{ItemID: item, MediaSourceID: "src-1", PlaySessionID: "play-1", Path: "/media/a.mkv", Size: size, Container: "mkv", Expires: now.Add(edgeReadGrantLifetime)}
	if grant != want {
		t.Fatalf("grant = %+v, want %+v", grant, want)
	}
}

func TestEdgePathMappings(t *testing.T) {
	mappings, err := pathMappings([]string{"/media=/mnt/media", "/tv=/mnt/tv"})
	if err != nil || len(mappings) != 2 || mappings[1] != (edge.PathMapping{Server: "/tv", Edge: "/mnt/tv"}) {
		t.Fatalf("pathMappings = %v, %v", mappings, err)
	}
	for _, bad := range []string{"/media", "media=/mnt", "/media=mnt"} {
		if _, err := pathMappings([]string{bad}); err == nil {
			t.Fatalf("pathMappings(%q) accepted", bad)
		}
	}
}
