// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// Provider personas model the same channels as two or three IPTV providers
// list them, for multi-source channel merging tests. Run one fakehdhr per
// persona (-persona a, b, c on different ports). Each persona renames
// channels its own way, uses its own guide ids, drops some channels and
// adds variants, as real providers do:
//
//	a  "US| FAKE NEWS HD": prefix, quality suffix, canonical guide ids
//	b  "[VIP] FAKE NEWS" / "FAKE NEWS FHD": its own ids (FakeNews.us); drops
//	   1 in 10; adds "+1" timeshift channels (distinct logical channels)
//	   and "4K" copies (the same logical channel, another source)
//	c  "UK: Fake News", or the call sign "WABC-DT" for 1 in 5; shares a's ids
//	   for 1 in 3, its own for 1 in 3, none for the rest; drops 1 in 10;
//	   adds "+1" channels that overlap b's
//
// /truth.json gives the canonical identity of every channel, so a matcher's
// precision and recall can be scored.
type truthEntry struct {
	Number    string `json:"number"`
	Name      string `json:"name"`
	TvgID     string `json:"tvg_id"`
	Canonical string `json:"canonical"` // logical channel: base guide id, plus "+1" for timeshift
	Variant   string `json:"variant,omitempty"`
}

// callSign is a stable, fake US-style call sign for base channel i.
func callSign(i int) string {
	n := i*7919 + 13
	b := []byte{'W', 0, 0, 0}
	for k := 1; k < 4; k++ {
		b[k] = byte('A' + n%26)
		n /= 26
	}
	return string(b)
}

// baseName is the channel name without the generator prefix, words spaced.
func baseName(c Channel) string {
	n := strings.TrimPrefix(strings.TrimPrefix(c.Name, "FAKE-"), "GEN-")
	if !strings.HasPrefix(c.Name, "GEN-") {
		n = "FAKE " + n
	}
	return strings.ReplaceAll(n, "-", " ")
}

func titleCase(s string) string {
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

func camel(s string) string {
	return strings.ReplaceAll(titleCase(s), " ", "")
}

// applyPersona rewrites a lineup as persona p lists it. HLS and fault
// channels pass through unchanged and are not part of the truth.
func applyPersona(p string, in []Channel) ([]Channel, []truthEntry, error) {
	if p == "" {
		return in, nil, nil
	}
	if p != "a" && p != "b" && p != "c" {
		return nil, nil, fmt.Errorf("persona %q: want a, b or c", p)
	}
	var out []Channel
	var truth []truthEntry
	add := func(c Channel, canonical, variant string) {
		out = append(out, c)
		id := c.guideID()
		if c.NoID {
			id = ""
		}
		truth = append(truth, truthEntry{Number: c.Number, Name: c.Name, TvgID: id, Canonical: canonical, Variant: variant})
	}
	for i, c := range in {
		if c.HLS != "" || c.Fault != "" {
			out = append(out, c)
			continue
		}
		canonical := c.guideID()
		base := baseName(c)
		c.CallSign = callSign(i)
		c.Sched = i + 1
		switch p {
		case "a":
			c.GuideID = canonical // renaming must not change the guide id
			c.Name = "US| " + base
			if i%3 == 0 {
				c.Name += " HD"
			}
			add(c, canonical, "")
		case "b":
			if i%10 == 7 {
				continue
			}
			c.GuideID = camel(base) + ".us"
			if i%4 == 0 {
				c.Name = "[VIP] " + base
			} else {
				c.Name = base + " FHD"
			}
			add(c, canonical, "")
			if i%20 == 3 {
				v := c
				v.Number, v.Name, v.GuideID, v.Shift = c.Number+"91", base+" +1", camel(base)+"Plus1.us", time.Hour
				add(v, canonical+"+1", "+1")
			}
			if i%15 == 5 {
				v := c
				v.Number, v.Name, v.Profile = c.Number+"92", base+" 4K", "hevc"
				v.GuideID = camel(base) + "4K.us"
				add(v, canonical, "4k")
			}
		case "c":
			if i%10 == 2 {
				continue
			}
			c.Name = "UK: " + titleCase(base)
			if i%5 == 4 {
				c.Name = c.CallSign + "-DT"
			}
			switch i % 3 {
			case 0: // the same guide source as provider a
				c.GuideID = canonical
			case 1:
				c.GuideID = strings.ToUpper(c.CallSign) + ".uk"
			default:
				c.NoID = true
			}
			add(c, canonical, "")
			if i%20 == 3 {
				v := c
				v.Number, v.Name, v.GuideID, v.NoID, v.Shift = c.Number+"91", "UK: "+titleCase(base)+" +1", strings.ToUpper(c.CallSign)+"P1.uk", false, time.Hour
				add(v, canonical+"+1", "+1")
			}
		}
	}
	return out, truth, nil
}

func (s *server) truthJSON(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.truth)
}
