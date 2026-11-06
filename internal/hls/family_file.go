// SPDX-License-Identifier: GPL-2.0-only

package hls

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Family file checks (#624): the server and the stream edge check the
// structure of each completed family file that a transcode worker uploads,
// before it replaces the file, so a compromised worker cannot store
// arbitrary bytes as a segment.

// TSPacketSize is the MPEG-TS packet size; TSSyncByte opens each packet.
const (
	TSPacketSize = 188
	TSSyncByte   = 0x47
)

// maxPlaylistBytes bounds an uploaded playlist.
const maxPlaylistBytes = 4 << 20

// ErrMalformedFile reports a family file whose structure does not match
// its name.
var ErrMalformedFile = errors.New("malformed HLS family file")

// CheckFamilyFile checks the file at path, uploaded as name: an MPEG-TS
// segment (.ts), an fMP4 init or media segment (.mp4, .m4s) or a playlist
// (.m3u8). Any other name is malformed.
func CheckFamilyFile(name, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".ts":
		err = checkTransportStream(file, info.Size())
	case ".mp4", ".m4s":
		err = checkBoxes(file, info.Size())
	case ".m3u8":
		err = checkPlaylist(file, info.Size())
	default:
		err = errors.New("unknown family file type")
	}
	if err != nil {
		return fmt.Errorf("%w %s: %v", ErrMalformedFile, name, err)
	}
	return nil
}

// checkTransportStream requires whole 188-byte packets that each start with
// the sync byte.
func checkTransportStream(file io.Reader, size int64) error {
	if size == 0 || size%TSPacketSize != 0 {
		return fmt.Errorf("size %d is not a whole number of transport packets", size)
	}
	reader := bufio.NewReaderSize(file, 64*TSPacketSize)
	packet := make([]byte, TSPacketSize)
	for offset := int64(0); offset < size; offset += TSPacketSize {
		if _, err := io.ReadFull(reader, packet); err != nil {
			return err
		}
		if packet[0] != TSSyncByte {
			return fmt.Errorf("no sync byte at offset %d", offset)
		}
	}
	return nil
}

// fmp4FirstBoxes are the boxes that open an fMP4 init segment or media
// segment.
var fmp4FirstBoxes = map[string]bool{"ftyp": true, "styp": true, "moof": true, "sidx": true, "emsg": true, "prft": true}

// checkBoxes requires a sequence of ISO BMFF boxes with printable types that
// covers the file exactly and opens with an fMP4 box.
func checkBoxes(file io.ReaderAt, size int64) error {
	if size == 0 {
		return errors.New("empty file")
	}
	header := make([]byte, 16)
	for offset := int64(0); offset < size; {
		if size-offset < 8 {
			return fmt.Errorf("truncated box header at offset %d", offset)
		}
		if _, err := file.ReadAt(header[:8], offset); err != nil {
			return err
		}
		boxSize := int64(binary.BigEndian.Uint32(header[:4]))
		kind := header[4:8]
		for _, b := range kind {
			if b < 0x20 || b > 0x7e {
				return fmt.Errorf("box type at offset %d is not printable", offset)
			}
		}
		if offset == 0 && !fmp4FirstBoxes[string(kind)] {
			return fmt.Errorf("first box %q does not open an fMP4 segment", kind)
		}
		switch boxSize {
		case 0:
			boxSize = size - offset
		case 1:
			if size-offset < 16 {
				return fmt.Errorf("truncated large box header at offset %d", offset)
			}
			if _, err := file.ReadAt(header[8:16], offset+8); err != nil {
				return err
			}
			large := binary.BigEndian.Uint64(header[8:16])
			if large < 16 || large > uint64(size-offset) {
				return fmt.Errorf("box size %d at offset %d is out of range", large, offset)
			}
			boxSize = int64(large)
		}
		if boxSize < 8 || boxSize > size-offset {
			return fmt.Errorf("box size %d at offset %d is out of range", boxSize, offset)
		}
		offset += boxSize
	}
	return nil
}

// checkPlaylist requires a bounded UTF-8 text file that starts with #EXTM3U.
func checkPlaylist(file io.Reader, size int64) error {
	if size > maxPlaylistBytes {
		return fmt.Errorf("playlist of %d bytes is too large", size)
	}
	body, err := io.ReadAll(io.LimitReader(file, maxPlaylistBytes+1))
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(body, []byte("#EXTM3U")) {
		return errors.New("playlist does not start with #EXTM3U")
	}
	if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		return errors.New("playlist is not UTF-8 text")
	}
	return nil
}
