// SPDX-License-Identifier: GPL-2.0-only

package hls

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckFamilyFile(t *testing.T) {
	box := func(kind string, payload int) []byte {
		data := make([]byte, 8+payload)
		data[3] = byte(8 + payload)
		copy(data[4:], kind)
		return data
	}
	packet := func() []byte {
		data := make([]byte, TSPacketSize)
		data[0] = TSSyncByte
		return data
	}
	twoPackets := append(packet(), packet()...)
	badSecond := append(packet(), bytes.Repeat([]byte{0}, TSPacketSize)...)
	largeBox := []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0, 0, 0, 0, 0, 0, 16}
	for _, test := range []struct {
		name string
		body []byte
		ok   bool
	}{
		{"abc0.ts", twoPackets, true},
		{"abc0.ts", twoPackets[:200], false},
		{"abc0.ts", badSecond, false},
		{"abc0.ts", nil, false},
		{"abc-1.mp4", append(box("ftyp", 8), box("moov", 0)...), true},
		{"abc0.mp4", append(box("moof", 4), box("mdat", 12)...), true},
		{"abc0.mp4", append(box("styp", 4), largeBox...), true},
		{"abc0.mp4", append(box("mdat", 4), box("moof", 0)...), false},
		{"abc0.mp4", append(box("moof", 4), 0, 0), false},
		{"abc0.mp4", append(box("moof", 4), 0, 0, 0, 99, 'm', 'd', 'a', 't'), false},
		{"abc0.mp4", append(box("moof", 0), 0, 0, 0, 8, 0, 'x', 'y', 'z'), false},
		{"abc.m3u8", []byte("#EXTM3U\n#EXTINF:4,\nabc0.ts\n"), true},
		{"abc.m3u8", []byte("<html>"), false},
		{"abc.m3u8", []byte("#EXTM3U\n\x00"), false},
		{"abc.m3u8.bak", []byte("#EXTM3U\n"), false},
	} {
		path := filepath.Join(t.TempDir(), "upload")
		if err := os.WriteFile(path, test.body, 0o600); err != nil {
			t.Fatal(err)
		}
		err := CheckFamilyFile(test.name, path)
		if test.ok && err != nil {
			t.Errorf("%s %q: %v", test.name, test.body, err)
		}
		if !test.ok && !errors.Is(err, ErrMalformedFile) {
			t.Errorf("%s %q: %v, want ErrMalformedFile", test.name, test.body, err)
		}
	}
}
