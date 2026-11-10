package server

import (
	"encoding/binary"
	"net"
)

type pcapFilter struct {
	host  net.IP
	port  uint16
	proto string
}

// match decides whether an Ethernet/cooked frame is kept. data starts at the IP header (callers strip link headers).
func (f pcapFilter) matchIP(ip []byte) bool {
	if len(ip) < 20 {
		return false
	}
	var src, dst net.IP
	var proto byte
	var l4 []byte
	switch ip[0] >> 4 {
	case 4:
		ihl := int(ip[0]&0xf) * 4
		if ihl < 20 || len(ip) < ihl {
			return false
		}
		proto, src, dst, l4 = ip[9], net.IP(ip[12:16]), net.IP(ip[16:20]), ip[ihl:]
	case 6:
		if len(ip) < 40 {
			return false
		}
		proto, src, dst, l4 = ip[6], net.IP(ip[8:24]), net.IP(ip[24:40]), ip[40:]
	default:
		return false
	}
	switch f.proto {
	case "tcp":
		if proto != 6 {
			return false
		}
	case "udp":
		if proto != 17 {
			return false
		}
	case "icmp":
		if proto != 1 && proto != 58 {
			return false
		}
	}
	if f.host != nil && !f.host.Equal(src) && !f.host.Equal(dst) {
		return false
	}
	if f.port != 0 {
		if (proto != 6 && proto != 17) || len(l4) < 4 {
			return false
		}
		if binary.BigEndian.Uint16(l4[0:2]) != f.port && binary.BigEndian.Uint16(l4[2:4]) != f.port {
			return false
		}
	}
	return true
}

// pcapHeader is the global pcap file header (microsecond timestamps, LINKTYPE_ETHERNET, snaplen 262144).
func pcapHeader() []byte {
	h := make([]byte, 24)
	binary.LittleEndian.PutUint32(h[0:], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(h[4:], 2)
	binary.LittleEndian.PutUint16(h[6:], 4)
	binary.LittleEndian.PutUint32(h[16:], 262144)
	binary.LittleEndian.PutUint32(h[20:], 1)
	return h
}

func pcapRecord(sec, usec uint32, frame []byte) []byte {
	r := make([]byte, 16+len(frame))
	binary.LittleEndian.PutUint32(r[0:], sec)
	binary.LittleEndian.PutUint32(r[4:], usec)
	binary.LittleEndian.PutUint32(r[8:], uint32(len(frame)))
	binary.LittleEndian.PutUint32(r[12:], uint32(len(frame)))
	copy(r[16:], frame)
	return r
}
