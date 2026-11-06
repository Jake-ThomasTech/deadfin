// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Channel is one synthetic tuner channel. Each profile models a real source
// shape that Jellyfin treats differently: ATSC 1.0 broadcast (interlaced
// MPEG-2 with AC-3), cable or IPTV (H.264 with AAC), ATSC 3.0 style HEVC, and
// "lite" (tiny H.264) for scale runs.
type Channel struct {
	Number  string
	Name    string
	Pattern string // lavfi video source name, e.g. testsrc2
	Size    string // WxH
	Rate    string // frame rate
	Tone    int    // sine frequency, so audio tracks differ per channel
	Profile string // "atsc1", "h264", "hevc", "lite"
	Group   string // M3U group-title and guide schedule family
	HD      bool
	Fault   string // default fault spec for this channel (faults.go)
	NoGuide bool   // no XMLTV programmes (real lineups have unmapped channels)
	HLS     string // FAST-style HLS plan (fast.go); such channels are M3U only

	// Provider personas (persona.go).
	GuideID  string        // guide id override; "" = derived from Name
	NoID     bool          // no guide id at all: no tvg-id, no XMLTV channel
	CallSign string        // added to the XMLTV display names
	Sched    int           // schedule index + 1, so variants share a schedule; 0 = position
	Shift    time.Duration // timeshift of the programmes (+1 channels)
}

// DefaultChannels is the built-in lineup. GuideNumber uses the ATSC major.minor
// form so channel-number parsing and sorting are exercised.
func DefaultChannels() []Channel {
	return []Channel{
		{Number: "2.1", Name: "FAKE-ATSC", Pattern: "smptehdbars", Size: "1920x1080", Rate: "30000/1001", Tone: 440, Profile: "atsc1", Group: "Broadcast", HD: true},
		{Number: "2.2", Name: "FAKE-SD", Pattern: "testsrc", Size: "720x480", Rate: "30000/1001", Tone: 550, Profile: "atsc1", Group: "Broadcast"},
		{Number: "4.1", Name: "FAKE-H264", Pattern: "testsrc2", Size: "1280x720", Rate: "30000/1001", Tone: 660, Profile: "h264", Group: "Cable", HD: true},
		{Number: "5.1", Name: "FAKE-NEWS", Pattern: "mandelbrot", Size: "1280x720", Rate: "30", Tone: 770, Profile: "h264", Group: "News", HD: true},
		{Number: "7.1", Name: "FAKE-HEVC", Pattern: "testsrc2", Size: "1920x1080", Rate: "60", Tone: 880, Profile: "hevc", Group: "Broadcast", HD: true},
		{Number: "9.1", Name: "FAKE-SPORTS", Pattern: "rgbtestsrc", Size: "1280x720", Rate: "30", Tone: 990, Profile: "h264", Group: "Sports", HD: true},
	}
}

// FaultChannels are always-faulty channels at 900.x, so a test can tune a
// known failure without restarting the tuner. See faults.go for the syntax.
func FaultChannels() []Channel {
	f := func(minor int, name, profile, spec string) Channel {
		return Channel{
			Number: "900." + strconv.Itoa(minor), Name: "FAULT-" + name, Pattern: "testsrc2",
			Size: "1280x720", Rate: "30", Tone: 300 + 10*minor, Profile: profile, Group: "Faults", HD: true, Fault: spec,
		}
	}
	return []Channel{
		f(1, "SLOWSTART", "h264", "slowstart=8s"),
		f(2, "STALL", "h264", "stall=3s/20s"),
		f(3, "DIE", "h264", "die=30s"),
		f(4, "CODEC", "atsc1", "codec=atsc1|h264,every=60s"),
		f(5, "BITRATE", "h264", "bitrate=1M|25M,every=30s"),
		f(6, "AUDIO", "atsc1", "audio=2|6,every=45s"),
		f(7, "RESOLUTION", "h264", "size=1280x720|1920x1080,every=60s"),
		f(8, "HANG", "h264", "hang"),
		f(9, "DEAD", "h264", "status=503"),
	}
}

var generatedGroups = []string{"Broadcast", "Cable", "News", "Sports", "Kids", "Movies"}

// GenerateChannels returns n procedural channels numbered 1000.1 upward, for
// scale runs. They use the cheap "lite" profile; every 20th has no guide.
func GenerateChannels(n int) []Channel {
	out := make([]Channel, 0, n)
	for i := range n {
		out = append(out, Channel{
			Number:  fmt.Sprintf("%d.%d", 1000+i/10, 1+i%10),
			Name:    fmt.Sprintf("GEN-%05d", i),
			Pattern: "testsrc2", Size: "426x240", Rate: "15",
			Tone:    200 + i%1800,
			Profile: "lite",
			Group:   generatedGroups[i%len(generatedGroups)],
			NoGuide: i%20 == 19,
		})
	}
	return out
}

// discoverResponse mirrors the fields upstream reads from /discover.json
// (Jellyfin.LiveTv/TunerHosts/HdHomerun/DiscoverResponse.cs). A ModelNumber
// containing "hdtc" makes upstream offer on-tuner transcode profiles.
type discoverResponse struct {
	FriendlyName    string `json:"FriendlyName"`
	ModelNumber     string `json:"ModelNumber"`
	FirmwareName    string `json:"FirmwareName"`
	FirmwareVersion string `json:"FirmwareVersion"`
	DeviceID        string `json:"DeviceID"`
	DeviceAuth      string `json:"DeviceAuth"`
	BaseURL         string `json:"BaseURL"`
	LineupURL       string `json:"LineupURL"`
	TunerCount      int    `json:"TunerCount"`
}

// lineupEntry mirrors Jellyfin.LiveTv/TunerHosts/HdHomerun/Channels.cs.
type lineupEntry struct {
	GuideNumber string `json:"GuideNumber"`
	GuideName   string `json:"GuideName"`
	VideoCodec  string `json:"VideoCodec"`
	AudioCodec  string `json:"AudioCodec"`
	URL         string `json:"URL"`
	HD          int    `json:"HD,omitempty"`
	Favorite    int    `json:"Favorite,omitempty"`
	DRM         int    `json:"DRM,omitempty"`
}

func (c Channel) codecs() (video, audio string) {
	switch c.Profile {
	case "h264", "lite":
		return "H264", "AAC"
	case "hevc":
		return "HEVC", "AC3"
	default:
		return "MPEG2", "AC3"
	}
}

// guideID is the XMLTV channel id and the M3U tvg-id.
func (c Channel) guideID() string {
	if c.GuideID != "" {
		return c.GuideID
	}
	return strings.ToLower(c.Name) + ".fake"
}

func buildLineup(base string, channels []Channel, drmNumber string) []lineupEntry {
	out := make([]lineupEntry, 0, len(channels))
	for _, c := range channels {
		if c.HLS != "" {
			continue // an HDHomeRun carries no internet channels
		}
		v, a := c.codecs()
		e := lineupEntry{
			GuideNumber: c.Number,
			GuideName:   c.Name,
			VideoCodec:  v,
			AudioCodec:  a,
			URL:         base + "/auto/v" + c.Number,
		}
		if c.HD {
			e.HD = 1
		}
		if c.Number == drmNumber {
			e.DRM = 1
		}
		out = append(out, e)
	}
	return out
}

// buildM3U writes the lineup as an M3U tuner source with the attributes
// upstream's M3uParser reads: tvg-id, tvg-name, tvg-logo, tvg-chno,
// group-title. Quirks add real-world mess: entries without tvg-id, and
// duplicate entries for the same channel from a second "source".
func buildM3U(base string, channels []Channel, q quirks, urlFor func(Channel) string) string {
	if urlFor == nil {
		urlFor = func(c Channel) string { return base + "/auto/v" + c.Number }
	}
	var b strings.Builder
	b.WriteString("#EXTM3U url-tvg=\"" + base + "/xmltv.xml\"\n")
	for i, c := range channels {
		id := fmt.Sprintf("tvg-id=%q ", c.guideID())
		if c.NoID || q.has("missing-ids") && i%10 == 9 {
			id = ""
		}
		entry := fmt.Sprintf("#EXTINF:-1 %stvg-name=%q tvg-logo=%q tvg-chno=%q group-title=%q,%s\n%s\n",
			id, c.Name, base+"/logo/"+c.Number+".svg", c.Number, c.Group, c.Name, urlFor(c))
		b.WriteString(entry)
		if q.has("dupes") && i%50 == 49 {
			u := urlFor(c)
			sep := "?"
			if strings.Contains(u, "?") {
				sep = "&"
			}
			fmt.Fprintf(&b, "#EXTINF:-1 %stvg-name=%q tvg-chno=%q group-title=%q,%s\n%s%sdup=1\n",
				id, c.Name, c.Number, c.Group, c.Name, u, sep)
		}
	}
	return b.String()
}

func logoSVG(c Channel) string {
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256" viewBox="0 0 256 256">`+
		`<rect width="256" height="256" rx="32" fill="hsl(%d,60%%,40%%)"/>`+
		`<text x="128" y="120" font-family="sans-serif" font-size="56" fill="#fff" text-anchor="middle">%s</text>`+
		`<text x="128" y="180" font-family="sans-serif" font-size="26" fill="#fff" text-anchor="middle">%s</text></svg>`,
		c.Tone%360, c.Number, strings.TrimPrefix(strings.TrimPrefix(c.Name, "FAKE-"), "FAULT-"))
}
