// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"log"
	"net"
)

// HDHomeRun discovery protocol (libhdhomerun hdhomerun_pkt.h). A packet is
// type(2, BE) + length(2, BE) + TLV payload + CRC32(IEEE, little-endian).
const (
	pktDiscoverReq = 0x0002
	pktDiscoverRpy = 0x0003

	tagDeviceType = 0x01
	tagDeviceID   = 0x02
	tagTunerCount = 0x10
	tagLineupURL  = 0x27
	tagBaseURL    = 0x2A

	deviceTypeTuner = 0x00000001
)

func framePacket(typ uint16, payload []byte) []byte {
	pkt := make([]byte, 4, 4+len(payload)+4)
	binary.BigEndian.PutUint16(pkt[0:], typ)
	binary.BigEndian.PutUint16(pkt[2:], uint16(len(payload)))
	pkt = append(pkt, payload...)
	return binary.LittleEndian.AppendUint32(pkt, crc32.ChecksumIEEE(pkt))
}

func appendTLV(b []byte, tag byte, value []byte) []byte {
	b = append(b, tag)
	if len(value) < 0x80 {
		b = append(b, byte(len(value)))
	} else {
		b = append(b, byte(len(value)&0x7F|0x80), byte(len(value)>>7))
	}
	return append(b, value...)
}

func discoverReply(deviceID uint32, tuners int, base string) []byte {
	var p []byte
	p = appendTLV(p, tagDeviceType, binary.BigEndian.AppendUint32(nil, deviceTypeTuner))
	p = appendTLV(p, tagDeviceID, binary.BigEndian.AppendUint32(nil, deviceID))
	p = appendTLV(p, tagTunerCount, []byte{byte(tuners)})
	p = appendTLV(p, tagBaseURL, []byte(base))
	p = appendTLV(p, tagLineupURL, []byte(base+"/lineup.json"))
	return framePacket(pktDiscoverRpy, p)
}

// isDiscoverRequest checks the frame type and CRC. Upstream sends a fixed
// request with wildcard device type and id (HdHomerunHost.cs DiscoverDevices).
func isDiscoverRequest(pkt []byte) bool {
	if len(pkt) < 8 || binary.BigEndian.Uint16(pkt) != pktDiscoverReq {
		return false
	}
	n := int(binary.BigEndian.Uint16(pkt[2:]))
	if len(pkt) < 4+n+4 {
		return false
	}
	return crc32.ChecksumIEEE(pkt[:4+n]) == binary.LittleEndian.Uint32(pkt[4+n:])
}

// serveDiscovery answers UDP discovery on addr until ctx ends. Note that
// upstream fetches http://<sender-ip>/discover.json after a reply, so
// discovery only completes end to end when the HTTP server listens on port
// 80 of the replying address. Manual entry of http://host:port works on any
// port.
func serveDiscovery(ctx context.Context, addr string, deviceID uint32, tuners int, base string) error {
	conn, err := net.ListenPacket("udp4", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	reply := discoverReply(deviceID, tuners, base)
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if !isDiscoverRequest(buf[:n]) {
			continue
		}
		if _, err := conn.WriteTo(reply, from); err != nil {
			log.Printf("discovery reply to %s: %v", from, err)
		}
	}
}
