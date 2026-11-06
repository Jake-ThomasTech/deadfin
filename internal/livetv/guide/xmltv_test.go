// SPDX-License-Identifier: GPL-2.0-only

package guide_test

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/livetv/guide"
)

var (
	wideStart = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	wideEnd   = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

func strptr(value string) *string { return &value }

func intptr(value int) *int { return &value }

func readProgrammes(t *testing.T, document string, language *string) []guide.XMLTVProgramme {
	t.Helper()
	var programmes []guide.XMLTVProgramme
	err := guide.ReadXMLTV(context.Background(), strings.NewReader(document), language, guide.Listings{
		Programme: func(p guide.XMLTVProgramme) error {
			programmes = append(programmes, p)
			return nil
		},
		Start: wideStart,
		End:   wideEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	return programmes
}

func readOne(t *testing.T, children string, language *string) guide.XMLTVProgramme {
	t.Helper()
	programmes := readProgrammes(t, `<tv><programme channel="c" start="20260101120000 +0000" stop="20260101130000 +0000">`+children+`</programme></tv>`, language)
	if len(programmes) != 1 {
		t.Fatalf("got %d programmes, want 1", len(programmes))
	}
	return programmes[0]
}

func readChannels(t *testing.T, document string, language *string) []guide.XMLTVChannel {
	t.Helper()
	var channels []guide.XMLTVChannel
	err := guide.ReadXMLTV(context.Background(), strings.NewReader(document), language, guide.Listings{
		Channel: func(c guide.XMLTVChannel) error {
			channels = append(channels, c)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return channels
}

// TestParseXMLTVDate follows XmlTvReader.ParseDate (XmlTvReader.cs:1251-1315).
func TestParseXMLTVDate(t *testing.T) {
	cases := []struct {
		in   string
		want string // RFC 3339; empty = no parse
	}{
		{"20221104130000 +0000", "2022-11-04T13:00:00Z"},
		{"20221104130000", "2022-11-04T13:00:00Z"},
		{"20260308020000 -0500", "2026-03-08T02:00:00-05:00"},
		{"20260308020000 +0530", "2026-03-08T02:00:00+05:30"},
		{"20260308020000 +1345", "2026-03-08T02:00:00+13:45"},
		{"2022", "2022-01-01T00:00:00Z"},
		{"202211", "2022-11-01T00:00:00Z"},
		{"2022110415", "2022-11-04T15:00:00Z"},
		// The offset counts only when it is exactly five characters.
		{"20221104130000 +01", "2022-11-04T13:00:00Z"},
		{"20221104130000 0100", "2022-11-04T13:00:00Z"},
		// No regex match: zone names, punctuation, too few digits.
		{"200007281733 BST", ""},
		{"2022-11-04", ""},
		{"202", ""},
		{"", ""},
		{"20221304000000", ""},
	}
	for _, tc := range cases {
		got, ok := guide.ParseXMLTVDate(tc.in)
		if tc.want == "" {
			if ok {
				t.Errorf("ParseXMLTVDate(%q) = %v, want no parse", tc.in, got)
			}
			continue
		}
		if !ok || got.Format(time.RFC3339) != tc.want {
			t.Errorf("ParseXMLTVDate(%q) = %v, %v; want %s", tc.in, got.Format(time.RFC3339), ok, tc.want)
		}
	}
}

// TestLanguageSelection follows the expected behaviour in the comments of
// ProcessNode (XmlTvReader.cs:1100-1120).
func TestLanguageSelection(t *testing.T) {
	titles := `<title lang="es">Spanish</title><title lang="en">English</title><title>No Language</title><title lang="">Empty</title>`
	cases := []struct {
		language *string
		want     string
	}{
		{nil, "No Language"},
		// The code, not the comment, is the oracle: "" equals lang="".
		{strptr(""), "Empty"},
		{strptr("es"), "Spanish"},
		{strptr("EN"), "English"},
		{strptr("fr"), "No Language"},
	}
	for _, tc := range cases {
		got := readOne(t, titles, tc.language)
		if got.Title == nil || *got.Title != tc.want {
			t.Errorf("language %v: title %v, want %q", tc.language, got.Title, tc.want)
		}
	}
	// With no unlabeled value, the first value wins.
	got := readOne(t, `<title lang="de">German</title><title lang="es">Spanish</title>`, strptr("en"))
	if *got.Title != "German" {
		t.Errorf("title %q, want German", *got.Title)
	}
	// A later run of the same element replaces the earlier one.
	got = readOne(t, `<title>First</title><desc>D</desc><title>Second</title>`, nil)
	if *got.Title != "Second" {
		t.Errorf("title %q, want Second", *got.Title)
	}
}

// TestMultipleNodes follows the expected behaviour in the comments of
// ProcessMultipleNodes (XmlTvReader.cs:1170-1190).
func TestMultipleNodes(t *testing.T) {
	categories := `<category lang="en">English</category><category lang="en">English 2</category>` +
		`<category lang="es">Spanish</category><category lang="es">Spanish 2</category>` +
		`<category lang="">Empty</category><category lang="">Empty 2</category>` +
		`<category>None</category><category>None 2</category>`
	cases := []struct {
		language *string
		want     []string
	}{
		{nil, []string{"None", "None 2"}},
		{strptr(""), []string{"Empty", "Empty 2"}},
		{strptr("es"), []string{"Spanish", "Spanish 2"}},
		{strptr("en"), []string{"English", "English 2"}},
		// Exact case; no match keeps all values.
		{strptr("EN"), []string{"English", "English 2", "Spanish", "Spanish 2", "Empty", "Empty 2", "None", "None 2"}},
	}
	for _, tc := range cases {
		got := readOne(t, categories, tc.language)
		if !reflect.DeepEqual(got.Categories, tc.want) {
			t.Errorf("language %v: categories %q, want %q", tc.language, got.Categories, tc.want)
		}
	}
}

func TestProgrammeElements(t *testing.T) {
	t.Run("xmltv_ns is zero-based", func(t *testing.T) {
		got := readOne(t, `<episode-num system="xmltv_ns">2 . 9/24 . 0/1</episode-num>`, nil)
		if !reflect.DeepEqual(got.Series, intptr(3)) || !reflect.DeepEqual(got.Episode, intptr(10)) || !got.HasEpisode {
			t.Errorf("series %v episode %v", got.Series, got.Episode)
		}
	})
	t.Run("xmltv_ns without season", func(t *testing.T) {
		got := readOne(t, `<episode-num system="xmltv_ns">.4.</episode-num>`, nil)
		if got.Series != nil || !reflect.DeepEqual(got.Episode, intptr(5)) {
			t.Errorf("series %v episode %v", got.Series, got.Episode)
		}
	})
	t.Run("SxxExx is one-based", func(t *testing.T) {
		got := readOne(t, `<episode-num system="SxxExx">S02E07</episode-num>`, nil)
		if !reflect.DeepEqual(got.Series, intptr(2)) || !reflect.DeepEqual(got.Episode, intptr(7)) {
			t.Errorf("series %v episode %v", got.Series, got.Episode)
		}
	})
	t.Run("system names are case-sensitive", func(t *testing.T) {
		got := readOne(t, `<episode-num system="XMLTV_NS">1.1.</episode-num>`, nil)
		if got.Series != nil || got.Episode != nil || !got.HasEpisode {
			t.Errorf("series %v episode %v has %v", got.Series, got.Episode, got.HasEpisode)
		}
	})
	t.Run("dd_progid", func(t *testing.T) {
		got := readOne(t, `<episode-num system="dd_progid">EP0001.0002</episode-num><episode-num system="dd_progid"> </episode-num>`, nil)
		if got.ProgramID != "EP0001.0002" {
			t.Errorf("program id %q", got.ProgramID)
		}
	})
	t.Run("previously-shown", func(t *testing.T) {
		cases := map[string]bool{
			`<previously-shown/>`:                        true,
			`<previously-shown start=""/>`:               true,
			`<previously-shown start="20250101000000"/>`: true,
			`<previously-shown start="20260101120000"/>`: false,
			`<previously-shown start="bad"/>`:            true,
		}
		for children, want := range cases {
			if got := readOne(t, children, nil); got.IsPreviouslyShown != want {
				t.Errorf("%s: previously shown %v, want %v", children, got.IsPreviouslyShown, want)
			}
		}
	})
	t.Run("new live premiere", func(t *testing.T) {
		got := readOne(t, `<new/><live/><premiere/>`, nil)
		if !got.IsNew || !got.IsLive || !got.Premiere {
			t.Errorf("%+v", got)
		}
	})
	t.Run("date", func(t *testing.T) {
		got := readOne(t, `<date>1999</date>`, nil)
		if got.CopyrightDate == nil || got.CopyrightDate.Year() != 1999 {
			t.Errorf("copyright %v", got.CopyrightDate)
		}
		if got := readOne(t, `<date>1999</date><date/>`, nil); got.CopyrightDate != nil {
			t.Errorf("empty date kept %v", got.CopyrightDate)
		}
		if got := readOne(t, `<date>1999</date><date>bad</date>`, nil); got.CopyrightDate == nil {
			t.Error("bad date cleared the copyright date")
		}
	})
	t.Run("rating", func(t *testing.T) {
		got := readOne(t, `<rating system="VCHIP"><value></value></rating><rating><value>TV-G</value><value>TV-PG</value></rating><rating><value>R</value></rating>`, nil)
		if !reflect.DeepEqual(got.FirstRating, strptr("TV-PG")) {
			t.Errorf("rating %v", got.FirstRating)
		}
		if got := readOne(t, `<rating><icon src="x"/></rating><rating><value>R</value></rating>`, nil); got.FirstRating != nil || !got.HasRating {
			t.Errorf("rating without value: %v %v", got.FirstRating, got.HasRating)
		}
	})
	t.Run("icon", func(t *testing.T) {
		got := readOne(t, `<icon src=""/><icon width="x"/><icon src="https://a/1.png"/><icon src="https://a/2.png"/>`, nil)
		if !reflect.DeepEqual(got.FirstIcon, strptr("https://a/1.png")) {
			t.Errorf("icon %v", got.FirstIcon)
		}
		// An icon with only a size counts, and has no source.
		if got := readOne(t, `<icon width="10"/><icon src="https://a/1.png"/>`, nil); got.FirstIcon != nil {
			t.Errorf("icon %v", got.FirstIcon)
		}
	})
	t.Run("image", func(t *testing.T) {
		got := readOne(t, `<image type="poster">p</image><image type="STILL">s1</image><image type="still">s2</image><image type="backdrop">b</image>`, nil)
		if !reflect.DeepEqual(got.StillPath, strptr("s1")) || !reflect.DeepEqual(got.BackdropPath, strptr("b")) {
			t.Errorf("still %v backdrop %v", got.StillPath, got.BackdropPath)
		}
	})
	t.Run("element names are matched lower-cased", func(t *testing.T) {
		got := readOne(t, `<Title>Upper</Title><DESC>D</DESC>`, nil)
		if !reflect.DeepEqual(got.Title, strptr("Upper")) || !reflect.DeepEqual(got.Description, strptr("D")) {
			t.Errorf("title %v desc %v", got.Title, got.Description)
		}
	})
}

// TestMinifiedSiblingsAreRead covers D-72: XmlTvReader skips once more after
// url, image, language and rating values, so with no whitespace between elements it drops the
// next sibling. The port reads it.
func TestMinifiedSiblingsAreRead(t *testing.T) {
	got := readOne(t, `<url>https://a</url><title>Kept</title><image type="still">s</image><desc>Also kept</desc>`, nil)
	if !reflect.DeepEqual(got.Title, strptr("Kept")) || !reflect.DeepEqual(got.Description, strptr("Also kept")) {
		t.Errorf("title %v desc %v", got.Title, got.Description)
	}
}

// TestProgrammeWindow follows GetProgramme (XmlTvReader.cs:191-207).
func TestProgrammeWindow(t *testing.T) {
	document := `<tv>
 <programme channel="A" start="20260101090000 +0000" stop="20260101100000 +0000"><title>ends at start</title></programme>
 <programme channel="a" start="20260101100000 +0000" stop="20260101110000 +0000"><title>inside</title></programme>
 <programme channel="A" start="20260101120000 +0000" stop="20260101130000 +0000"><title>starts at end</title></programme>
 <programme channel="B" start="20260101100000 +0000" stop="20260101110000 +0000"><title>other channel</title></programme>
 <programme channel="A" start="20260101100000 +0000"><title>no stop</title></programme>
 <programme start="20260101100000 +0000" stop="20260101110000 +0000"><title>no channel</title></programme>
</tv>`
	var titles []string
	err := guide.ReadXMLTV(context.Background(), strings.NewReader(document), nil, guide.Listings{
		WantProgrammes: func(id string) bool { return strings.EqualFold(id, "A") },
		Programme: func(p guide.XMLTVProgramme) error {
			titles = append(titles, *p.Title)
			return nil
		},
		Start: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ends at start", "inside"}; !reflect.DeepEqual(titles, want) {
		t.Errorf("titles %q, want %q", titles, want)
	}
}

// TestChannels follows GetChannel and SetChannelNumber
// (XmlTvReader.cs:77-140).
func TestChannels(t *testing.T) {
	document := `<tv>
 <channel id="one"><display-name lang="en">One</display-name><display-name>1-1</display-name><display-name>HD</display-name><icon width="20"/><icon src="https://a/1.png"/></channel>
 <channel id=""><display-name>No id</display-name></channel>
 <channel id="empty"><display-name></display-name></channel>
 <channel id="none"><icon src="x"/></channel>
 <channel id="two"><display-name>Two</display-name><display-name>2_1</display-name><icon src="https://a/2.png"/></channel>
 <programme channel="one" start="20260101100000" stop="20260101110000"/>
 <channel id="late"><display-name>Late</display-name></channel>
</tv>`
	got := readChannels(t, document, strptr("en"))
	want := []guide.XMLTVChannel{
		{ID: "one", DisplayName: "One", Number: "1.1"},
		{ID: "two", DisplayName: "Two", Number: "2.1", IconURL: "https://a/2.png"},
		{ID: "late", DisplayName: "Late"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("channels\n got %+v\nwant %+v", got, want)
	}
}

// TestFakehdhrQuirks reads the fakehdhr -quirks all guide
// (testdata/fakehdhr-quirks.xml; anchor 2026-03-08T00:00:00Z, one day, no
// fault channels).
func TestFakehdhrQuirks(t *testing.T) {
	file, err := os.Open("testdata/fakehdhr-quirks.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	document := string(data)

	channels := readChannels(t, document, strptr("en"))
	if len(channels) != 14 {
		t.Fatalf("%d channels, want 14", len(channels))
	}
	if channels[0].ID != "fake-atsc.fake" || channels[0].Number != "2.1" || channels[0].DisplayName != "FAKE-ATSC" {
		t.Errorf("first channel %+v", channels[0])
	}

	all := readProgrammes(t, document, strptr("en"))
	withStop := 0
	for _, line := range strings.Split(document, "\n") {
		if strings.Contains(line, "<programme ") && strings.Contains(line, " stop=") {
			withStop++
		}
	}
	// Programmes without stop have End = MinValue and are dropped.
	if len(all) != withStop {
		t.Errorf("%d programmes, want %d (those with a stop)", len(all), withStop)
	}
	offsets := map[int]bool{}
	entities := false
	for _, p := range all {
		_, offset := p.StartDate.Zone()
		offsets[offset] = true
		if p.Title != nil && strings.Contains(*p.Title, `& "Friends" <Live>`) {
			entities = true
		}
	}
	for _, seconds := range []int{0, -4 * 3600, -5 * 3600, 5*3600 + 1800, 13*3600 + 2700} {
		if !offsets[seconds] {
			t.Errorf("no programme with UTC offset %ds; have %v", seconds, offsets)
		}
	}
	if !entities {
		t.Error("no programme title with decoded entities")
	}
}

func BenchmarkReadXMLTV(b *testing.B) {
	file, err := os.Open("testdata/fakehdhr-guide.xml.gz")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	unzip, err := gzip.NewReader(file)
	if err != nil {
		b.Fatal(err)
	}
	data, err := io.ReadAll(unzip)
	if err != nil {
		b.Fatal(err)
	}
	language := "en"
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		count := 0
		err := guide.ReadXMLTV(context.Background(), strings.NewReader(string(data)), &language, guide.Listings{
			WantProgrammes: func(string) bool { return true },
			Programme: func(guide.XMLTVProgramme) error {
				count++
				return nil
			},
			Start: wideStart,
			End:   wideEnd,
		})
		if err != nil || count == 0 {
			b.Fatal(err, count)
		}
	}
}
