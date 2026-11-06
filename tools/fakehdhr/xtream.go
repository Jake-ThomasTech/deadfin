// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Xtream Codes style panel. Upstream has no Xtream client: users add
// get.php as an M3U tuner and xmltv.php as an XMLTV provider. Every URL
// carries the account's username and password, in the query (get.php,
// xmltv.php, player_api.php) and in the path of every stream
// (/live/U/P/<id>.ts). That makes the panel the test source for credential
// redaction. The credentials are synthetic and set by flags; the default
// password is a marker string that tests search for.
//
// Real panels ban client IPs for crawling, failed logins and probing. The
// fake does the same (-xtream-ban-after, -xtream-ban-for), so a test can
// assert that a server backs off instead of hammering a provider: after
// banAfter failed logins inside banFor, every request from that IP gets
// 403 until banFor has passed since the last failure.
//
// m3u_plus mixes live channels with VOD (/movie/U/P/<id>.mp4) and series
// episodes (/series/U/P/<id>.mkv), as real panels do. Upstream imports
// them all as channels.
type xtream struct {
	user, pass string
	movies     int
	episodes   int
	banAfter   int           // failed logins that ban the client IP; 0 = never
	banFor     time.Duration // ban length, and the window that counts failures

	mu    sync.Mutex
	fails map[string][]time.Time // client IP -> recent failed logins
}

// banned reports whether ip is banned at now, and records a failure first
// when failed is set.
func (x *xtream) banned(ip string, now time.Time, failed bool) bool {
	if x.banAfter == 0 {
		return false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.fails == nil {
		x.fails = map[string][]time.Time{}
	}
	recent := x.fails[ip][:0]
	for _, t := range x.fails[ip] {
		if now.Sub(t) < x.banFor {
			recent = append(recent, t)
		}
	}
	if failed {
		recent = append(recent, now)
	}
	x.fails[ip] = recent
	return len(recent) >= x.banAfter
}

const xtreamLiveBase = 10000 // stream id of live channel i is xtreamLiveBase+i

func (s *server) xtreamAuth(w http.ResponseWriter, r *http.Request, user, pass string) bool {
	if s.xt == nil {
		http.NotFound(w, r)
		return false
	}
	s.xtRequests.Add(1)
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	now := s.clock()
	if s.xt.banned(ip, now, false) {
		s.banRejects.Add(1)
		http.Error(w, "banned", http.StatusForbidden)
		return false
	}
	if user != s.xt.user || pass != s.xt.pass {
		s.authRejects.Add(1)
		s.xt.banned(ip, now, true)
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *server) xtreamQueryAuth(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	return s.xtreamAuth(w, r, q.Get("username"), q.Get("password"))
}

// xtreamGet serves get.php?username=U&password=P&type=m3u_plus&output=ts.
func (s *server) xtreamGet(w http.ResponseWriter, r *http.Request) {
	if !s.xtreamQueryAuth(w, r) {
		return
	}
	ext := "ts"
	if r.URL.Query().Get("output") == "m3u8" || r.URL.Query().Get("output") == "hls" {
		ext = "m3u8"
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl")
	_, _ = io.WriteString(w, s.xtreamM3U(ext, r.URL.Query().Get("type") == "m3u_plus"))
}

func (s *server) xtreamM3U(ext string, plus bool) string {
	u, p := s.xt.user, s.xt.pass
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	entry := func(id, name, logo, group, url string) {
		if plus {
			fmt.Fprintf(&b, "#EXTINF:-1 tvg-id=%q tvg-name=%q tvg-logo=%q group-title=%q,%s\n%s\n", id, name, logo, group, name, url)
		} else {
			fmt.Fprintf(&b, "#EXTINF:-1,%s\n%s\n", name, url)
		}
	}
	for i, c := range s.channels {
		if c.HLS != "" {
			continue
		}
		id := c.guideID()
		if c.NoID {
			id = ""
		}
		entry(id, c.Name, s.base+"/logo/"+c.Number+".svg", c.Group,
			fmt.Sprintf("%s/live/%s/%s/%d.%s", s.base, u, p, xtreamLiveBase+i, ext))
	}
	for i := range s.xt.movies {
		entry("", fmt.Sprintf("Fake Movie %05d (2026)", i), "", "VOD | "+generatedGroups[i%len(generatedGroups)],
			fmt.Sprintf("%s/movie/%s/%s/%d.mp4", s.base, u, p, 500000+i))
	}
	for i := range s.xt.episodes {
		entry("", fmt.Sprintf("Fake Series %03d S%02dE%02d", i/40, 1+i%40/10, 1+i%10), "", "SERIES | Fake",
			fmt.Sprintf("%s/series/%s/%s/%d.mkv", s.base, u, p, 800000+i))
	}
	return b.String()
}

// xtreamGuide serves xmltv.php?username=U&password=P.
func (s *server) xtreamGuide(w http.ResponseWriter, r *http.Request) {
	if !s.xtreamQueryAuth(w, r) {
		return
	}
	s.guide(w, r)
}

// xtreamAPI serves player_api.php, modeled on the field shapes users
// document for real panels (Fermata discussion #434). Shapes vary by
// panel; the "xtream-types" quirk mixes string and number ids, empty
// epg_channel_id and null icons, so a client must parse loosely.
//
//	(no action)            account check; like real panels it echoes the password
//	get_live_categories    [{category_id, category_name, parent_id}]
//	get_live_streams       [{num, name, stream_type, stream_id, ...}], &category_id=N filters
//	get_short_epg,         per-channel JSON guide. Providers rate-limit these;
//	get_simple_data_table  a client should read xmltv.php instead. Counted.
func (s *server) xtreamAPI(w http.ResponseWriter, r *http.Request) {
	if !s.xtreamQueryAuth(w, r) {
		return
	}
	q := r.URL.Query()
	mixed := s.quirks.has("xtream-types")
	id := func(n int) any {
		if mixed && n%2 == 1 {
			return n
		}
		return strconv.Itoa(n)
	}
	switch q.Get("action") {
	case "":
		writeJSON(w, map[string]any{
			"user_info": map[string]any{
				"username": s.xt.user, "password": s.xt.pass, "message": "", "auth": 1, "status": "Active",
				"exp_date": "1893456000", "is_trial": "0", "active_cons": "0", "created_at": "1767225600",
				"max_connections": strconv.Itoa(s.tuners.limit), "allowed_output_formats": []string{"m3u8", "ts"},
			},
			"server_info": map[string]any{
				"url": strings.TrimPrefix(s.base, "http://"), "port": "80", "https_port": "0", "server_protocol": "http",
				"rtmp_port": "0", "timezone": "UTC", "timestamp_now": s.clock().Unix(), "time_now": s.clock().UTC().Format("2006-01-02 15:04:05"), "process": true,
			},
		})
	case "get_live_categories":
		out := []map[string]any{}
		for i, g := range xtreamGroups(s.channels) {
			out = append(out, map[string]any{"category_id": id(i + 1), "category_name": g, "parent_id": 0})
		}
		writeJSON(w, out)
	case "get_live_streams":
		groups := xtreamGroups(s.channels)
		want := q.Get("category_id")
		out := []map[string]any{}
		for i, c := range s.channels {
			if c.HLS != "" {
				continue
			}
			cat := slices.Index(groups, c.Group) + 1
			if want != "" && want != strconv.Itoa(cat) {
				continue
			}
			epg, icon := any(c.guideID()), any(s.base+"/logo/"+c.Number+".svg")
			if c.NoID || mixed && i%7 == 3 {
				epg, icon = "", nil
			}
			out = append(out, map[string]any{
				"num": i + 1, "name": c.Name, "stream_type": "live", "stream_id": id(xtreamLiveBase + i),
				"stream_icon": icon, "epg_channel_id": epg, "added": "1767225600", "category_id": id(cat),
				"custom_sid": "", "tv_archive": 0, "direct_source": "", "tv_archive_duration": 0,
			})
		}
		writeJSON(w, out)
	case "get_short_epg", "get_simple_data_table":
		s.jsonEPGCalls.Add(1)
		writeJSON(w, map[string]any{"epg_listings": []any{}})
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

// xtreamGroups returns the live channel groups in first-seen order; a
// group's category_id is its index + 1.
func xtreamGroups(channels []Channel) []string {
	var out []string
	for _, c := range channels {
		if c.HLS == "" && !slices.Contains(out, c.Group) {
			out = append(out, c.Group)
		}
	}
	return out
}

// xtreamLive serves /live/U/P/<id>.ts as a tune of channel id-10000.
func (s *server) xtreamLive(w http.ResponseWriter, r *http.Request) {
	if !s.xtreamAuth(w, r, r.PathValue("user"), r.PathValue("pass")) {
		return
	}
	file := r.PathValue("file")
	id, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSuffix(file, ".ts"), ".m3u8"))
	i := id - xtreamLiveBase
	if err != nil || i < 0 || i >= len(s.channels) || s.channels[i].HLS != "" {
		http.NotFound(w, r)
		return
	}
	s.tune(w, r, s.channels[i])
}

// xtreamVOD answers VOD and series URLs. They exist for import scale and
// credential redaction; playing them is out of scope.
func (s *server) xtreamVOD(w http.ResponseWriter, r *http.Request) {
	if !s.xtreamAuth(w, r, r.PathValue("user"), r.PathValue("pass")) {
		return
	}
	http.Error(w, "vod not served by fakehdhr", http.StatusNotFound)
}
