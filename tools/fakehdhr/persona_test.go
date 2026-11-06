// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestPersonasAndTruth(t *testing.T) {
	base := append(DefaultChannels(), GenerateChannels(200)...)
	counts := map[string]int{}
	for _, p := range []string{"a", "b", "c"} {
		chans, truth, err := applyPersona(p, base)
		if err != nil {
			t.Fatal(err)
		}
		if len(chans) != len(truth) {
			t.Fatalf("%s: %d channels, %d truth entries", p, len(chans), len(truth))
		}
		seen := map[string]bool{}
		for i, c := range chans {
			if seen[c.Number] {
				t.Fatalf("%s: duplicate number %s", p, c.Number)
			}
			seen[c.Number] = true
			if truth[i].Variant == "+1" && c.Shift != time.Hour {
				t.Fatalf("%s: +1 channel %s not shifted", p, c.Name)
			}
		}
		counts[p] = len(chans)
	}
	if counts["a"] != 206 || counts["c"] >= counts["a"] {
		t.Fatalf("persona sizes = %v", counts)
	}
	if _, _, err := applyPersona("d", base); err == nil {
		t.Fatal("persona d accepted")
	}
}

func TestPersonaGuideShift(t *testing.T) {
	chans, truth, _ := applyPersona("b", DefaultChannels())
	var plus Channel
	for i, c := range chans {
		if truth[i].Variant == "+1" {
			plus = c
		}
	}
	anchor := testAnchor.Truncate(time.Hour)
	progs := channelProgrammes(plus.Sched-1, plus, "", anchor, anchor.Add(6*time.Hour), quirks{})
	if progs[0].Start != testAnchor.Truncate(time.Hour).Add(time.Hour).Format(xmltvTime) {
		t.Fatalf("+1 channel starts %s", progs[0].Start)
	}
}

// The prototype matcher below calibrates the §4.4 accuracy targets. It is
// not the Deadfin implementation; it shows that the staged rules reach the
// targets on the persona fixtures.

var (
	prefixRe  = regexp.MustCompile(`^(\[[^\]]*\]|[A-Za-z]{2,3}\s*[|:])\s*`)
	qualityRe = regexp.MustCompile(`(?i)\s+(hd|fhd|uhd|4k|hevc|sd)$`)
	callRe    = regexp.MustCompile(`^([kw][a-z]{3})( ?dt)?$`)
	punctRe   = regexp.MustCompile(`[^a-z0-9+ ]+`)
)

func normalizeName(n string) string {
	for prev := ""; prev != n; {
		prev = n
		n = prefixRe.ReplaceAllString(strings.TrimSpace(n), "")
		n = qualityRe.ReplaceAllString(n, "")
	}
	n = punctRe.ReplaceAllString(strings.ToLower(strings.ReplaceAll(n, "-", " ")), "")
	return strings.Join(strings.Fields(n), " ")
}

type srcEntry struct {
	provider string
	truth    truthEntry
	callSign string // from the provider's XMLTV display names, when it has a guide entry
	norm     string
	schedule string // a stand-in for the provider's programme sequence; "" without a guide
}

func jaccard(a, b string) float64 {
	ta, tb := map[string]bool{}, map[string]bool{}
	for _, w := range strings.Fields(a) {
		ta[w] = true
	}
	for _, w := range strings.Fields(b) {
		tb[w] = true
	}
	inter := 0
	for w := range ta {
		if tb[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(ta)+len(tb)-inter)
}

func TestPrototypeMatcherAccuracy(t *testing.T) {
	base := append(DefaultChannels(), GenerateChannels(600)...)
	var es []srcEntry
	for _, p := range []string{"a", "b", "c"} {
		chans, truth, _ := applyPersona(p, base)
		for i, c := range chans {
			e := srcEntry{provider: p, truth: truth[i], norm: normalizeName(c.Name)}
			if !c.NoID {
				e.callSign = strings.ToLower(c.CallSign)
				e.schedule = fmt.Sprintf("%d%+v", c.Sched, c.Shift)
			}
			es = append(es, e)
		}
	}
	for _, useGuide := range []bool{true, false} {
		precision, recall, stage := prototypeMatch(es, useGuide)
		t.Logf("guide signal %v: %d source entries; merges by stage %v; pairwise precision %.4f recall %.4f", useGuide, len(es), stage, precision, recall)
		if useGuide && (precision < 0.99 || recall < 0.97) {
			t.Fatalf("precision %.4f recall %.4f below 0.99/0.97", precision, recall)
		}
	}
}

// prototypeMatch merges source entries in stages and scores the result
// pairwise against the persona truth.
func prototypeMatch(es []srcEntry, useGuide bool) (precision, recall float64, stage map[string]int) {
	parent := make([]int, len(es))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(i, j int) { parent[find(i)] = find(j) }
	stage = map[string]int{}

	// 1. The same guide id in two providers.
	byID := map[string]int{}
	for i, e := range es {
		if e.truth.TvgID == "" {
			continue
		}
		if j, ok := byID[e.truth.TvgID]; ok && es[j].provider != e.provider {
			union(i, j)
			stage["id"]++
		} else {
			byID[e.truth.TvgID] = i
		}
	}
	// 2. The same normalized name. A key that two channels of one provider
	// share is ambiguous: "X FHD" and "X 4K" are one channel, "FAKE SD" and
	// "FAKE HEVC" are two, and the names alone cannot tell. The guide can:
	// the same programme sequence means the same channel. An ambiguous key
	// without a guide is not used.
	perProvider := map[string]int{}
	for _, e := range es {
		perProvider[e.provider+"|"+e.norm]++
	}
	byName := map[string]int{}
	bySched := map[string]int{}
	for i, e := range es {
		if useGuide && e.schedule != "" {
			k := e.norm + "#" + e.schedule
			if j, ok := bySched[k]; ok {
				union(i, j)
				stage["name+guide"]++
			} else {
				bySched[k] = i
			}
		}
		if perProvider[e.provider+"|"+e.norm] > 1 {
			continue
		}
		if j, ok := byName[e.norm]; ok {
			union(i, j)
			stage["name"]++
		} else {
			byName[e.norm] = i
		}
	}
	// 3. A call-sign name against call signs from the guides.
	byCall := map[string][]int{}
	for i, e := range es {
		if e.callSign != "" {
			byCall[e.callSign] = append(byCall[e.callSign], i)
		}
	}
	for i, e := range es {
		if m := callRe.FindStringSubmatch(e.norm); m != nil {
			for _, j := range byCall[m[1]] {
				if es[j].provider != e.provider && find(i) != find(j) {
					union(i, j)
					stage["callsign"]++
				}
			}
		}
	}
	// 4. Fuzzy: a unique best token match at or above 0.75, never across "+1".
	for i, e := range es {
		best, bestJ, ties := 0.0, -1, 0
		for j, f := range es {
			if f.provider == e.provider || strings.Contains(e.norm, "+1") != strings.Contains(f.norm, "+1") {
				continue
			}
			switch s := jaccard(e.norm, f.norm); {
			case s > best:
				best, bestJ, ties = s, j, 0
			case s == best:
				ties++
			}
		}
		if best >= 0.75 && best < 1 && ties == 0 && find(i) != find(bestJ) {
			union(i, bestJ)
			stage["fuzzy"]++
		}
	}

	var tp, fp, fn int
	for i := range es {
		for j := i + 1; j < len(es); j++ {
			same := es[i].truth.Canonical == es[j].truth.Canonical
			merged := find(i) == find(j)
			switch {
			case same && merged:
				tp++
			case !same && merged:
				fp++

			case same && !merged:
				fn++

			}
		}
	}
	return float64(tp) / float64(tp+fp), float64(tp) / float64(tp+fn), stage
}
