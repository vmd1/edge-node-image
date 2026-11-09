//go:build linux

package server

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// capture reads Ethernet frames from an AF_PACKET socket for the whole context lifetime, keeps those that pass the filter and
// writes a pcap stream. Stops at maxBytes.
func capture(ctx context.Context, iface string, f pcapFilter, maxBytes int64, emit func([]byte) error) error {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if iface != "any" {
		ifc, err := net.InterfaceByName(iface)
		if err != nil {
			return err
		}
		if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: ifc.Index}); err != nil {
			return err
		}
	}
	tv := unix.NsecToTimeval(int64(250 * time.Millisecond))
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return err
	}
	if err := emit(pcapHeader()); err != nil {
		return err
	}
	var sent int64 = 24
	buf := make([]byte, 65536)
	for ctx.Err() == nil {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		fr := buf[:n]
		// strip the Ethernet header (and one VLAN tag) to find the IP header for the filter
		if len(fr) < 14 {
			continue
		}
		off, et := 14, binary.BigEndian.Uint16(fr[12:14])
		if et == 0x8100 && len(fr) >= 18 {
			off, et = 18, binary.BigEndian.Uint16(fr[16:18])
		}
		if et != 0x0800 && et != 0x86dd {
			continue
		}
		if !f.matchIP(fr[off:]) {
			continue
		}
		now := time.Now()
		rec := pcapRecord(uint32(now.Unix()), uint32(now.Nanosecond()/1000), fr)
		if sent+int64(len(rec)) > maxBytes {
			return nil
		}
		if err := emit(rec); err != nil {
			return err
		}
		sent += int64(len(rec))
	}
	return nil
}
