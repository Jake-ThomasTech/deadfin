// SPDX-License-Identifier: GPL-2.0-only

package tuner_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

func parse(t *testing.T, playlist string, options tuner.ParseOptions) []tuner.Channel {
	t.Helper()
	var channels []tuner.Channel
	err := tuner.ParseM3U(strings.NewReader(playlist), "m3u_prefix", "host-1", options, func(channel tuner.Channel) error {
		channels = append(channels, channel)
		return nil
	})
	if err != nil {
		t.Fatalf("ParseM3U: %v", err)
	}
	return channels
}

func parseOne(t *testing.T, extInf, url string) tuner.Channel {
	t.Helper()
	channels := parse(t, "#EXTM3U\n#EXTINF:"+extInf+"\n"+url+"\n", tuner.ParseOptions{})
	if len(channels) != 1 {
		t.Fatalf("parsed %d channels, want 1", len(channels))
	}
	return channels[0]
}

// Each case cites the M3uParser.cs lines it checks. The pinned tree has no
// M3uParser unit tests to port.
func TestParseM3UChannelFields(t *testing.T) {
	for _, tc := range []struct {
		name, extInf, url    string
		wantName, wantNumber string
		wantImage, wantGroup string
		wantTunerChannelID   string
	}{
		{
			// Attributes: tvg-logo, group-title, tvg-id, tvg-chno (128-149, 176-181).
			name:     "full attributes",
			extInf:   `-1 tvg-id="wxyz.us" tvg-name="WXYZ" tvg-logo="http://l.test/a.png" tvg-chno="7.1" group-title="Local",WXYZ HD`,
			url:      "http://h.test/v7.1",
			wantName: "WXYZ HD", wantNumber: "7.1", wantImage: "http://l.test/a.png", wantGroup: "Local", wantTunerChannelID: "wxyz.us",
		},
		{
			// logo is the fallback for tvg-logo (128-135).
			name:   "logo fallback",
			extInf: `-1 logo="http://l.test/b.png",Name`, url: "http://h.test/x",
			wantName: "Name", wantImage: "http://l.test/b.png",
		},
		{
			// channel-id is the guide key without tvg-id (145-149).
			name:   "channel-id key",
			extInf: `-1 channel-id="abc",Name`, url: "http://h.test/x",
			wantName: "Name", wantTunerChannelID: "abc",
		},
		{
			// A numeric tvg-id is the number when tvg-chno is absent (189-195).
			name:   "numeric tvg-id number",
			extInf: `-1 tvg-id="12",Name`, url: "http://h.test/x",
			wantName: "Name", wantNumber: "12", wantTunerChannelID: "12",
		},
		{
			// channel-id is the number only when tvg-id is present (189-200).
			name:   "channel-id number needs tvg-id",
			extInf: `-1 tvg-id="name.id" channel-id="33",Name`, url: "http://h.test/x",
			wantName: "Name", wantNumber: "33", wantTunerChannelID: "name.id",
		},
		{
			name:   "channel-id alone is no number",
			extInf: `-1 channel-id="33",Name`, url: "http://h.test/x",
			wantName: "Name", wantTunerChannelID: "33",
		},
		{
			// SatIP name "84. VOX" (209-224, 284-298).
			name:   "satip dot",
			extInf: `0,84. VOX Schweiz`, url: "http://h.test/x",
			wantName: "VOX Schweiz", wantNumber: "84",
		},
		{
			name:   "satip dash",
			extInf: `0,84.0 - VOX Schweiz`, url: "http://h.test/x",
			wantName: "VOX Schweiz", wantNumber: "84.0",
		},
		{
			// "5 USA": the name keeps the SatIP split; the name loses "5"
			// as upstream does (284-298).
			name:   "leading number in name",
			extInf: `-1,5 USA`, url: "http://h.test/x",
			wantName: "USA", wantNumber: "5",
		},
		{
			// tvg-chno "0" is invalid; the URL file name is the fallback
			// (227-252, 267-277).
			name:   "zero chno falls back to url",
			extInf: `-1 tvg-chno="0",Name`, url: "http://h.test/stream/101.ts",
			wantName: "Name", wantNumber: "101",
		},
		{
			name:   "minus one chno",
			extInf: `-1 tvg-chno="-1",Name`, url: "http://h.test/stream/abc.ts",
			wantName: "Name",
		},
		{
			name:   "non-numeric chno",
			extInf: `-1 tvg-chno="abc",Name`, url: "http://h.test/x/202",
			wantName: "Name", wantNumber: "202",
		},
		{
			// Name falls back to tvg-name, then tvg-id (300-317).
			name:   "name from tvg-name",
			extInf: `-1 tvg-name="From Attr" tvg-id="id.1"`, url: "http://h.test/x",
			wantName: "From Attr", wantTunerChannelID: "id.1",
		},
		{
			name:   "name from tvg-id",
			extInf: `-1 tvg-id="id.1"`, url: "http://h.test/x",
			wantName: "id.1", wantTunerChannelID: "id.1",
		},
		{
			// Only the text after the last comma is the name (281-282).
			name:   "comma in title",
			extInf: `-1 tvg-chno="3",News, Weather`, url: "http://h.test/x",
			wantName: "Weather", wantNumber: "3",
		},
		{
			// Attribute keys match case-insensitively (36, 324).
			name:   "attribute case",
			extInf: `-1 TVG-LOGO="http://l.test/c.png" Tvg-Chno="8",Name`, url: "http://h.test/x",
			wantName: "Name", wantNumber: "8", wantImage: "http://l.test/c.png",
		},
		{
			// A number with surrounding space is trimmed (237-240).
			name:   "invariant number with spaces",
			extInf: `-1 tvg-chno=" 9 ",Name`, url: "http://h.test/x",
			wantName: "Name", wantNumber: "9",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOne(t, tc.extInf, tc.url)
			if got.Name != tc.wantName || got.Number != tc.wantNumber || got.ImageURL != tc.wantImage ||
				got.Group != tc.wantGroup || got.TunerChannelID != tc.wantTunerChannelID {
				t.Fatalf("got name=%q number=%q image=%q group=%q key=%q\nwant name=%q number=%q image=%q group=%q key=%q",
					got.Name, got.Number, got.ImageURL, got.Group, got.TunerChannelID,
					tc.wantName, tc.wantNumber, tc.wantImage, tc.wantGroup, tc.wantTunerChannelID)
			}
			if got.Path != tc.url || got.TunerHostID != "host-1" || got.Type != tuner.ChannelTypeTV {
				t.Fatalf("got path=%q host=%q type=%q", got.Path, got.TunerHostID, got.Type)
			}
		})
	}
}

// URL schemes: http, https, rtsp, rtp, udp only (M3uParser.cs:96-102,
// 257-265). Blank lines, other # lines and an entry without EXTINF are
// skipped (77-114).
func TestParseM3USkipsInvalidEntries(t *testing.T) {
	playlist := strings.Join([]string{
		"#EXTM3U",
		"",
		"http://h.test/no-extinf",
		"#EXTINF:-1,Good HTTP",
		"#EXTVLCOPT:http-user-agent=X",
		"http://h.test/1",
		"#EXTINF:-1,Bad scheme",
		"file:///etc/passwd",
		"#EXTINF:-1,Relative",
		"/relative/path",
		"#EXTINF:-1,UDP",
		"udp://@239.0.0.1:1234",
		"#EXTINF:-1,RTSP",
		"rtsp://cam.test/live",
		"#EXTINF:-1,HTTPS",
		"  https://h.test/2  ",
	}, "\n")
	channels := parse(t, playlist, tuner.ParseOptions{})
	var names []string
	for _, channel := range channels {
		names = append(names, channel.Name)
	}
	if got, want := strings.Join(names, "|"), "Good HTTP|UDP|RTSP|HTTPS"; got != want {
		t.Fatalf("channels = %q, want %q", got, want)
	}
	if channels[3].Path != "https://h.test/2" {
		t.Fatalf("path not trimmed: %q", channels[3].Path)
	}
}

// The id is the prefix plus md5(stream URL) as a .NET Guid N string
// (M3uParser.cs:104; M3UTunerHost.cs:64-67): MD5 over UTF-16LE text,
// Guid mixed-endian bytes. The vectors come from
// uuid.UUID(bytes_le=md5(s.encode("utf-16-le"))).hex.
func TestM3UChannelIDs(t *testing.T) {
	for in, want := range map[string]string{
		"":                                   "m3u_d98c1dd4008f04b2e9800998ecf8427e",
		"http://127.0.0.1:5004/playlist.m3u": "m3u_86a3fdd94c07df72b8adb7a97313356b",
	} {
		if got := tuner.M3UChannelIDPrefixFor(in); got != want {
			t.Fatalf("M3UChannelIDPrefixFor(%q) = %q, want %q", in, got, want)
		}
	}
	channels := parse(t, "#EXTINF:-1,A\nhttp://127.0.0.1:5004/playlist.m3u\n", tuner.ParseOptions{})
	if got, want := channels[0].ID, "m3u_prefix86a3fdd94c07df72b8adb7a97313356b"; got != want {
		t.Fatalf("channel id = %q, want %q", got, want)
	}
}

func TestFakeHDHRPlaylistAndLineupMatch(t *testing.T) {
	read := func(name string) []tuner.Channel {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return parse(t, string(data), tuner.ParseOptions{})
	}
	playlist := read("fakehdhr-playlist.m3u")
	lineup := read("fakehdhr-lineup.m3u")
	if len(playlist) != 23 {
		t.Fatalf("playlist channels = %d, want 23", len(playlist))
	}
	if len(lineup) != len(playlist) {
		t.Fatalf("lineup channels = %d, playlist = %d", len(lineup), len(playlist))
	}
	var hls int
	for index := range playlist {
		if playlist[index].ID != lineup[index].ID || playlist[index].Number != lineup[index].Number || playlist[index].Name != lineup[index].Name {
			t.Fatalf("channel %d differs: %+v vs %+v", index, playlist[index], lineup[index])
		}
		if strings.HasPrefix(playlist[index].Number, "800.") {
			hls++
		}
	}
	if hls != 8 {
		t.Fatalf("800.x HLS channels = %d, want 8", hls)
	}
	first := playlist[0]
	if first.Name != "FAKE-ATSC" || first.Number != "2.1" || first.TunerChannelID != "fake-atsc.fake" || first.Group != "Broadcast" {
		t.Fatalf("first channel = %+v", first)
	}
}

// With SkipVOD the movie and series entries of an Xtream m3u_plus playlist
// are dropped; without it the parser matches upstream and keeps them.
func TestParseM3USkipVOD(t *testing.T) {
	data, err := os.ReadFile("testdata/fakehdhr-xtream.m3u")
	if err != nil {
		t.Fatal(err)
	}
	all := parse(t, string(data), tuner.ParseOptions{})
	live := parse(t, string(data), tuner.ParseOptions{SkipVOD: true})
	if len(all)-len(live) != 5 {
		t.Fatalf("all=%d live=%d, want 5 VOD entries skipped", len(all), len(live))
	}
	for _, channel := range live {
		if strings.Contains(channel.Path, "/movie/") || strings.Contains(channel.Path, "/series/") {
			t.Fatalf("VOD entry kept: %s", channel.Path)
		}
	}
}

func TestParseM3URejectsHugeLine(t *testing.T) {
	playlist := "#EXTINF:-1," + strings.Repeat("x", 2<<20) + "\nhttp://h.test/a\n"
	err := tuner.ParseM3U(strings.NewReader(playlist), "p", "h", tuner.ParseOptions{}, func(tuner.Channel) error { return nil })
	if err == nil {
		t.Fatal("ParseM3U accepted a 2 MiB line")
	}
}

// syntheticPlaylist streams an m3u_plus-style playlist of n entries.
func syntheticPlaylist(n int) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for index := range n {
		fmt.Fprintf(&b, "#EXTINF:-1 tvg-id=\"ch%d.test\" tvg-name=\"Channel %d\" tvg-logo=\"http://logo.test/%d.png\" group-title=\"Group %d\",Channel %d\n", index, index, index, index%40, index)
		fmt.Fprintf(&b, "http://panel.test/live/user/pass/%d.ts\n", index)
	}
	return b.String()
}

// L13: allocations per entry stay flat from 1 k to 30 k entries; the parser
// holds only the current entry.
func BenchmarkParseM3U(b *testing.B) {
	for _, n := range []int{1_000, 30_000} {
		playlist := syntheticPlaylist(n)
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(playlist)))
			for b.Loop() {
				count := 0
				_ = tuner.ParseM3U(strings.NewReader(playlist), "m3u_p", "h", tuner.ParseOptions{}, func(tuner.Channel) error {
					count++
					return nil
				})
				if count != n {
					b.Fatalf("count = %d", count)
				}
			}
			b.ReportMetric(float64(testing.AllocsPerRun(1, func() {
				_ = tuner.ParseM3U(strings.NewReader(playlist), "m3u_p", "h", tuner.ParseOptions{}, func(tuner.Channel) error { return nil })
			}))/float64(n), "allocs/entry")
		})
	}
}
