package discovery

import (
	"bytes"
	"net"
	"testing"
)

func sampleV2() *Announcement {
	mac, _ := net.ParseMAC("00:27:22:00:00:02")
	return &Announcement{
		Version:   V2,
		Command:   6,
		MAC:       mac,
		Addresses: []Address{{MAC: mac, IP: net.IPv4(192, 168, 1, 50)}},
		Firmware:  "MOCA.ar934x.v1.2.3",
		Uptime:    3600,
		Hostname:  "moca-adapter",
		Platform:  "MOCA2",
		ESSID:     "test-ssid",
		Wmode:     3,
		Seq:       7,
		SourceMAC: mac,
		Model:     "MOCA2A",
		Netmask:   net.IPv4(255, 255, 255, 0),
	}
}

func TestMarshalHeader(t *testing.T) {
	pkt, err := sampleV2().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if pkt[0] != byte(V2) || pkt[1] != 6 {
		t.Errorf("header version/command = %d/%d, want 2/6", pkt[0], pkt[1])
	}
	bodyLen := int(pkt[2])<<8 | int(pkt[3])
	if bodyLen != len(pkt)-4 {
		t.Errorf("declared body length %d, want %d", bodyLen, len(pkt)-4)
	}
}

func TestRoundTrip(t *testing.T) {
	in := sampleV2()
	pkt, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out, err := Parse(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.MAC, in.MAC) || out.Firmware != in.Firmware ||
		out.Uptime != in.Uptime || out.Hostname != in.Hostname ||
		out.Platform != in.Platform || out.Model != in.Model || out.Seq != in.Seq ||
		out.ESSID != in.ESSID || out.Wmode != in.Wmode {
		t.Errorf("round trip lost fields:\n in=%+v\nout=%+v", in, out)
	}
	if len(out.Addresses) != 1 || !out.Addresses[0].IP.Equal(in.Addresses[0].IP) ||
		!bytes.Equal(out.Addresses[0].MAC, in.Addresses[0].MAC) {
		t.Errorf("address round trip failed: %+v", out.Addresses)
	}
	if !out.Netmask.Equal(in.Netmask) {
		t.Errorf("netmask = %v, want %v", out.Netmask, in.Netmask)
	}
}

func TestParseSkipsUnknownTLV(t *testing.T) {
	// version=1, command=0, body = [type 0x7f len 2 value 0xdead] + MAC TLV.
	mac := []byte{0, 0x27, 0x22, 0, 0, 2}
	body := []byte{0x7f, 0, 2, 0xde, 0xad, tlvMAC, 0, 6}
	body = append(body, mac...)
	pkt := append([]byte{1, 0, byte(len(body) >> 8), byte(len(body))}, body...)
	a, err := Parse(pkt)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !bytes.Equal(a.MAC, mac) {
		t.Errorf("MAC = %x, want the known TLV parsed past the unknown one", a.MAC)
	}
}

func TestParseRejectsTruncation(t *testing.T) {
	if _, err := Parse([]byte{1, 0}); err == nil {
		t.Error("Parse of a 2-byte packet: want an error")
	}
	// Header claims 10 body bytes, only 2 present.
	if _, err := Parse([]byte{1, 0, 0, 10, 0x01, 0x00}); err == nil {
		t.Error("Parse with an overrunning body length: want an error")
	}
}

func TestParseRejectsTruncatedTLV(t *testing.T) {
	// Outer header declares 10 body bytes, but the TLV inside claims a length
	// that exceeds what's present: type=0x01, length=0x0008, but only 2 bytes follow.
	pkt := []byte{1, 0, 0, 5, 0x01, 0x00, 0x08, 0x00, 0x00}
	if _, err := Parse(pkt); err == nil {
		t.Error("Parse with overrunning TLV length: want an error")
	}
}

func TestMarshalRejectsWrongLengthMAC(t *testing.T) {
	// Set MAC to wrong length; Marshal should error.
	a := &Announcement{
		Version: V1,
		Command: 0,
		MAC:     net.HardwareAddr{0, 1, 2, 3}, // only 4 bytes
	}
	if _, err := a.Marshal(); err == nil {
		t.Error("Marshal with wrong-length MAC: want an error")
	}
}
