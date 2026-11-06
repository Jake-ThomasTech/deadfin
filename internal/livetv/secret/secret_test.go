// SPDX-License-Identifier: GPL-2.0-only

package secret_test

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/livetv/secret"
)

// marker is a synthetic password. It must never survive redaction.
const marker = "MARKERpw-7b1e"

func TestRedactMasksURLCredentials(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "get.php query",
			in:   "http://panel.test:8080/get.php?username=alice&password=" + marker + "&type=m3u_plus",
			want: "http://panel.test:8080/get.php?username=***&password=***&type=m3u_plus",
		},
		{
			name: "xmltv.php query, mixed key case",
			in:   "https://panel.test/xmltv.php?UserName=alice&PassWord=" + marker,
			want: "https://panel.test/xmltv.php?UserName=***&PassWord=***",
		},
		{
			name: "user info",
			in:   "http://alice:" + marker + "@tuner.test/lineup.m3u",
			want: "http://***:***@tuner.test/lineup.m3u",
		},
		{
			name: "xtream live path",
			in:   "http://panel.test/live/alice/" + marker + "/1234.ts",
			want: "http://panel.test/live/***/***/1234.ts",
		},
		{
			name: "token query",
			in:   "https://cdn.test/hls/master.m3u8?token=abc123&start=0",
			want: "https://cdn.test/hls/master.m3u8?token=***&start=0",
		},
		{
			name: "url inside an error string",
			in:   `Get "http://panel.test/get.php?username=alice&password=` + marker + `": dial tcp: refused`,
			want: `Get "http://panel.test/get.php?username=***&password=***": dial tcp: refused`,
		},
		{
			name: "no secrets",
			in:   "http://tuner.test/playlist.m3u",
			want: "http://tuner.test/playlist.m3u",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := secret.Redact(tc.in); got != tc.want {
				t.Fatalf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A channel URL from a get.php tuner carries the credentials in its path
// under a root that no key rule knows; the Set masks them by value.
func TestSetMasksChannelURLsByValue(t *testing.T) {
	set := secret.Collect([]string{"http://panel.test/get.php?username=alice&password=" + marker})
	got := set.Redact("opening http://edge.test/alice/" + marker + "/1234 failed")
	if strings.Contains(got, marker) || strings.Contains(got, "alice") {
		t.Fatalf("Redact = %q, still holds a credential", got)
	}
}

func TestSetMasksEscapedForms(t *testing.T) {
	password := "p@ss word/" + marker
	set := secret.Collect(nil, password)
	for _, in := range []string{
		"raw " + password,
		"query password=p%40ss+word%2F" + marker,
		"path /p@ss%20word%2F" + marker + "/",
	} {
		if got := set.Redact(in); strings.Contains(got, marker) {
			t.Fatalf("Redact(%q) = %q", in, got)
		}
	}
}

func TestSetMasksLongestSecretFirst(t *testing.T) {
	set := secret.Collect(nil, "abc", "abcdef")
	if got := set.Redact("x=abcdef y=abc"); got != "x=*** y=***" {
		t.Fatalf("Redact = %q", got)
	}
}

func TestErrKeepsChainAndMasksMessage(t *testing.T) {
	set := secret.Collect(nil, marker)
	base := fmt.Errorf("fetch http://panel.test/live/u/%s/1.ts: %w", marker, fs.ErrNotExist)
	err := set.Err(base)
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("Err().Error() = %q", err.Error())
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("errors.Is lost the wrapped chain")
	}
	if set.Err(nil) != nil {
		t.Fatal("Err(nil) != nil")
	}
	plain := errors.New("no secret here")
	if set.Err(plain) != plain {
		t.Fatal("Err wrapped an error that holds no secret")
	}
}

func TestEmptyValuesAreIgnored(t *testing.T) {
	set := secret.Collect([]string{"http://u:@tuner.test/x?password="}, "")
	if got := set.Redact("plain text"); got != "plain text" {
		t.Fatalf("Redact = %q", got)
	}
}
