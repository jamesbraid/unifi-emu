// Package discovery implements the UniFi L2 device-discovery protocol: the
// UDP :10001 "Ubiquiti Discovery" packet a device broadcasts so a controller
// finds it on the local segment. The wire format was recovered by
// disassembling the controller's own parser; see docs/PROTOCOL.md for the
// per-field provenance. This is the device side — build an Announcement and
// broadcast Marshal()'s bytes; Parse decodes a packet the way the controller
// does, skipping unknown TLV types.
package discovery

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// Port is the UDP port both the request and the response use.
const Port = 10001

// Version is the discovery packet version in the first header byte.
type Version uint8

const (
	V1 Version = 1
	V2 Version = 2
)

// TLV type codes, verified against the controller parser except where noted.
const (
	tlvMAC      = 0x01 // 6 bytes
	tlvMACIP    = 0x02 // 6-byte MAC + 4-byte IPv4
	tlvFirmware = 0x03 // string
	tlvUptime   = 0x0a // uint32 seconds
	tlvHostname = 0x0b // string
	tlvPlatform = 0x0c // string (model code)
	tlvESSID    = 0x0d // string
	tlvWmode    = 0x0e // uint32
	tlvSeq      = 0x12 // uint32 (v2)
	tlvSrcMAC   = 0x13 // 6 bytes (v2)
	tlvModel    = 0x15 // string (v2)
	tlvNetmask  = 0x35 // 4-byte IPv4
)

// Address is a MAC paired with an IPv4 address (TLV 0x02), repeatable.
type Address struct {
	MAC net.HardwareAddr
	IP  net.IP
}

// Announcement is a device's discovery packet, decoded to the identity fields
// the controller's parser names. Zero-valued fields are omitted by Marshal.
type Announcement struct {
	Version   Version
	Command   uint8
	MAC       net.HardwareAddr
	Addresses []Address
	Firmware  string
	Uptime    uint32
	Hostname  string
	Platform  string
	ESSID     string
	Wmode     uint32
	Seq       uint32
	SourceMAC net.HardwareAddr
	Model     string
	Netmask   net.IP
}

func putTLV(buf []byte, typ byte, val []byte) []byte {
	var hdr [3]byte
	hdr[0] = typ
	binary.BigEndian.PutUint16(hdr[1:], uint16(len(val)))
	return append(append(buf, hdr[:]...), val...)
}

func putU32(buf []byte, typ byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return putTLV(buf, typ, b[:])
}

// Marshal renders the packet: a 4-byte header (version, command, 2-byte body
// length) followed by the TLV body. Only set fields are emitted.
func (a *Announcement) Marshal() ([]byte, error) {
	if a.Version == 0 {
		return nil, errors.New("discovery: Version is required")
	}
	var body []byte
	if len(a.MAC) == 6 {
		body = putTLV(body, tlvMAC, a.MAC)
	}
	for _, ad := range a.Addresses {
		if len(ad.MAC) != 6 {
			return nil, fmt.Errorf("discovery: address MAC must be 6 bytes, got %d", len(ad.MAC))
		}
		ip := ad.IP.To4()
		if ip == nil {
			return nil, fmt.Errorf("discovery: address IP %v is not IPv4", ad.IP)
		}
		body = putTLV(body, tlvMACIP, append(append([]byte{}, ad.MAC...), ip...))
	}
	if a.Firmware != "" {
		body = putTLV(body, tlvFirmware, []byte(a.Firmware))
	}
	if a.Uptime != 0 {
		body = putU32(body, tlvUptime, a.Uptime)
	}
	if a.Hostname != "" {
		body = putTLV(body, tlvHostname, []byte(a.Hostname))
	}
	if a.Platform != "" {
		body = putTLV(body, tlvPlatform, []byte(a.Platform))
	}
	if a.ESSID != "" {
		body = putTLV(body, tlvESSID, []byte(a.ESSID))
	}
	if a.Wmode != 0 {
		body = putU32(body, tlvWmode, a.Wmode)
	}
	if a.Version == V2 {
		if a.Seq != 0 {
			body = putU32(body, tlvSeq, a.Seq)
		}
		if len(a.SourceMAC) == 6 {
			body = putTLV(body, tlvSrcMAC, a.SourceMAC)
		}
		if a.Model != "" {
			body = putTLV(body, tlvModel, []byte(a.Model))
		}
	}
	if ip := a.Netmask.To4(); ip != nil {
		body = putTLV(body, tlvNetmask, ip)
	}
	if len(body) > 0xffff {
		return nil, fmt.Errorf("discovery: body too long (%d bytes)", len(body))
	}
	out := []byte{byte(a.Version), a.Command, 0, 0}
	binary.BigEndian.PutUint16(out[2:], uint16(len(body)))
	return append(out, body...), nil
}

// Parse decodes a discovery packet. Unknown TLV types are skipped, matching the
// controller. A truncated header or TLV is an error.
func Parse(pkt []byte) (*Announcement, error) {
	if len(pkt) < 4 {
		return nil, fmt.Errorf("discovery: packet too short: %d bytes", len(pkt))
	}
	a := &Announcement{Version: Version(pkt[0]), Command: pkt[1]}
	bodyLen := int(binary.BigEndian.Uint16(pkt[2:4]))
	if 4+bodyLen > len(pkt) {
		return nil, fmt.Errorf("discovery: body length %d exceeds packet (%d bytes)", bodyLen, len(pkt))
	}
	b := pkt[4 : 4+bodyLen]
	for len(b) > 0 {
		if len(b) < 3 {
			return nil, errors.New("discovery: truncated TLV header")
		}
		typ := b[0]
		n := int(binary.BigEndian.Uint16(b[1:3]))
		if 3+n > len(b) {
			return nil, fmt.Errorf("discovery: TLV %d length %d overruns body", typ, n)
		}
		v := b[3 : 3+n]
		switch typ {
		case tlvMAC:
			if n == 6 {
				a.MAC = net.HardwareAddr(append([]byte{}, v...))
			}
		case tlvMACIP:
			if n == 10 {
				a.Addresses = append(a.Addresses, Address{
					MAC: net.HardwareAddr(append([]byte{}, v[:6]...)),
					IP:  net.IP(append([]byte{}, v[6:10]...)),
				})
			}
		case tlvFirmware:
			a.Firmware = string(v)
		case tlvUptime:
			if n == 4 {
				a.Uptime = binary.BigEndian.Uint32(v)
			}
		case tlvHostname:
			a.Hostname = string(v)
		case tlvPlatform:
			a.Platform = string(v)
		case tlvESSID:
			a.ESSID = string(v)
		case tlvWmode:
			if n == 4 {
				a.Wmode = binary.BigEndian.Uint32(v)
			}
		case tlvSeq:
			if n == 4 {
				a.Seq = binary.BigEndian.Uint32(v)
			}
		case tlvSrcMAC:
			if n == 6 {
				a.SourceMAC = net.HardwareAddr(append([]byte{}, v...))
			}
		case tlvModel:
			a.Model = string(v)
		case tlvNetmask:
			if n == 4 {
				a.Netmask = net.IP(append([]byte{}, v...))
			}
		}
		b = b[3+n:]
	}
	return a, nil
}
