package herder

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strings"
)

const (
	// serialMax and nameMax bound an explicit serial and name.
	serialMax = 32
	nameMax   = 63
	// macRetries bounds the collision retry. Each attempt draws 48 random
	// bits, so exhausting this many means the random source is broken, not
	// that the address space filled up.
	macRetries = 64
)

// Allocate resolves every requested device to a final identity: supplied
// serials and names are preserved, supplied MACs are canonicalized, and
// anything missing is derived. Order and index follow the request.
//
// randSource supplies the bytes for generated MACs; nil means the operating
// system cryptographic random source. Every failure here is a bad request:
// the caller reports invalid_request in phase validate.
func Allocate(devices []RequestDevice, randSource io.Reader) ([]Identity, error) {
	if randSource == nil {
		randSource = rand.Reader
	}
	out := make([]Identity, len(devices))

	// Pass 1: validate and canonicalize what the caller supplied, and
	// reserve all of it. Reserving before generating is what lets a
	// generated identity avoid a value a later request entry supplied.
	reserved := newIdentitySet()
	supplied := make([]Identity, len(devices))
	for i, d := range devices {
		if err := checkModel(d.Model); err != nil {
			return nil, err
		}
		id := Identity{Index: i, Model: d.Model}
		if d.MAC != "" {
			mac, err := canonicalMAC(d.MAC)
			if err != nil {
				return nil, err
			}
			if !reserved.addMAC(mac) {
				return nil, failf(CodeInvalidRequest, PhaseValidate, "duplicate MAC %s", mac)
			}
			id.MAC = mac
		}
		if d.Serial != "" {
			if err := checkSerial(d.Serial); err != nil {
				return nil, err
			}
			if !reserved.addSerial(d.Serial) {
				return nil, failf(CodeInvalidRequest, PhaseValidate, "duplicate serial %q", d.Serial)
			}
			id.Serial = d.Serial
		}
		if d.Name != "" {
			if err := checkName(d.Name); err != nil {
				return nil, err
			}
			if !reserved.addName(d.Name) {
				return nil, failf(CodeInvalidRequest, PhaseValidate, "duplicate name %q", d.Name)
			}
			id.Name = d.Name
		}
		supplied[i] = id
	}

	// Pass 2: finish the entries whose MAC the caller supplied. A supplied
	// MAC is never replaced -- only canonicalized -- so a derived serial or
	// name that is already taken is a request the caller must fix.
	final := newIdentitySet()
	for i, id := range supplied {
		if id.MAC == "" {
			continue
		}
		hex := macHex(id.MAC)
		if id.Serial == "" {
			id.Serial = derivedSerial(hex)
			if reserved.hasSerial(id.Serial) || !final.addSerial(id.Serial) {
				return nil, failf(CodeInvalidRequest, PhaseValidate,
					"MAC %s derives serial %s, which is already taken", id.MAC, id.Serial)
			}
		}
		if id.Name == "" {
			id.Name = derivedName(id.Model, hex)
			if reserved.hasName(id.Name) || !final.addName(id.Name) {
				return nil, failf(CodeInvalidRequest, PhaseValidate,
					"MAC %s derives name %s, which is already taken", id.MAC, id.Name)
			}
		}
		out[i] = id
	}

	// Pass 3: generate the missing MACs, retrying while the candidate or
	// anything it derives collides.
	for i, id := range supplied {
		if id.MAC != "" {
			continue
		}
		resolved, err := generate(id, reserved, final, randSource)
		if err != nil {
			return nil, err
		}
		out[i] = resolved
	}
	return out, nil
}

// generate draws MACs until one and its derived fields are free, then claims
// them in final.
func generate(id Identity, reserved, final *identitySet, randSource io.Reader) (Identity, error) {
	for attempt := 0; attempt < macRetries; attempt++ {
		var raw [6]byte
		if _, err := io.ReadFull(randSource, raw[:]); err != nil {
			return Identity{}, wrapf(err, CodeInvalidRequest, PhaseValidate,
				"read random bytes for a generated MAC: %v", err)
		}
		raw[0] &^= 0x01 // unicast
		raw[0] |= 0x02  // locally administered
		mac := net.HardwareAddr(raw[:]).String()
		if reserved.hasMAC(mac) || final.hasMAC(mac) {
			continue
		}
		hex := macHex(mac)
		candidate := id
		candidate.MAC = mac
		if candidate.Serial == "" {
			candidate.Serial = derivedSerial(hex)
			if reserved.hasSerial(candidate.Serial) || final.hasSerial(candidate.Serial) {
				continue
			}
		}
		if candidate.Name == "" {
			candidate.Name = derivedName(candidate.Model, hex)
			if reserved.hasName(candidate.Name) || final.hasName(candidate.Name) {
				continue
			}
		}
		final.addMAC(candidate.MAC)
		final.addSerial(candidate.Serial)
		final.addName(candidate.Name)
		return candidate, nil
	}
	return Identity{}, failf(CodeInvalidRequest, PhaseValidate,
		"could not allocate a free MAC for device %d after %d attempts", id.Index, macRetries)
}

// identitySet tracks the claimed values of one allocation stage.
type identitySet struct {
	macs, serials, names map[string]bool
}

func newIdentitySet() *identitySet {
	return &identitySet{macs: map[string]bool{}, serials: map[string]bool{}, names: map[string]bool{}}
}

func (s *identitySet) addMAC(v string) bool    { return add(s.macs, v) }
func (s *identitySet) addSerial(v string) bool { return add(s.serials, v) }
func (s *identitySet) addName(v string) bool   { return add(s.names, v) }
func (s *identitySet) hasMAC(v string) bool    { return s.macs[v] }
func (s *identitySet) hasSerial(v string) bool { return s.serials[v] }
func (s *identitySet) hasName(v string) bool   { return s.names[v] }

// add claims v, reporting false when it was already claimed.
func add(set map[string]bool, v string) bool {
	if set[v] {
		return false
	}
	set[v] = true
	return true
}

// canonicalMAC parses a supplied MAC and returns its lowercase
// colon-separated form. net.ParseMAC also accepts 8- and 20-byte addresses,
// which are not device MACs, so the length is checked explicitly. An explicit
// MAC must be locally administered and unicast: a globally administered
// address could collide with real hardware on the runner's network, and a
// multicast address is not an endpoint address at all.
func canonicalMAC(s string) (string, error) {
	hw, err := net.ParseMAC(s)
	if err != nil {
		return "", wrapf(err, CodeInvalidRequest, PhaseValidate, "MAC %q: %v", s, err)
	}
	if len(hw) != 6 {
		return "", failf(CodeInvalidRequest, PhaseValidate,
			"MAC %q has %d bytes, want 6", s, len(hw))
	}
	if hw[0]&0x01 != 0 {
		return "", failf(CodeInvalidRequest, PhaseValidate, "MAC %q is multicast", s)
	}
	if hw[0]&0x02 == 0 {
		return "", failf(CodeInvalidRequest, PhaseValidate,
			"MAC %q is not locally administered", s)
	}
	return hw.String(), nil
}

// macHex is the MAC as 12 hexadecimal digits with no separators.
func macHex(mac string) string { return strings.ReplaceAll(mac, ":", "") }

// derivedSerial is EMU followed by the MAC in uppercase hexadecimal.
func derivedSerial(hex string) string { return "EMU" + strings.ToUpper(hex) }

// derivedName is emu-<normalized-model>-<mac-hex>. Only the model is
// truncated, so the MAC suffix -- the part that makes the name unique --
// always survives the 63-character DNS label limit.
func derivedName(model, hex string) string {
	const suffix = 1 + 12 // "-" + 12 hex digits
	budget := nameMax - len("emu-") - suffix
	norm := normalizeModel(model)
	if len(norm) > budget {
		norm = strings.Trim(norm[:budget], "-")
		if norm == "" {
			norm = "device"
		}
	}
	return "emu-" + norm + "-" + hex
}

// normalizeModel lowercases the model and reduces it to a DNS label body:
// every non-alphanumeric byte becomes a dash, runs of dashes collapse, and
// leading and trailing dashes go. A model made entirely of punctuation leaves
// nothing, so it becomes "device".
func normalizeModel(model string) string {
	var b strings.Builder
	dash := false
	for i := 0; i < len(model); i++ {
		c := model[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + ('a' - 'A'))
			dash = false
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "device"
	}
	return out
}

// checkModel requires a non-empty printable-ASCII model. Case is preserved:
// the controller and the public model registry both key on exact model codes.
func checkModel(model string) error {
	if model == "" {
		return failf(CodeInvalidRequest, PhaseValidate, "a device has no model")
	}
	if !printableASCII(model) {
		return failf(CodeInvalidRequest, PhaseValidate,
			"model %q contains a non-printable byte", model)
	}
	return nil
}

// checkSerial accepts 1-32 printable ASCII bytes. Control characters are out:
// a serial travels through container environment, JSON payloads and
// controller state, and none of those survive one intact.
func checkSerial(serial string) error {
	if len(serial) > serialMax {
		return failf(CodeInvalidRequest, PhaseValidate,
			"serial %q is %d bytes, want at most %d", serial, len(serial), serialMax)
	}
	if !printableASCII(serial) {
		return failf(CodeInvalidRequest, PhaseValidate,
			"serial %q contains a non-printable byte", serial)
	}
	return nil
}

// checkName requires one lowercase RFC 1123 DNS label. The name reaches
// Docker and the controller as a hostname, so anything a label cannot hold is
// rejected rather than mangled.
func checkName(name string) error {
	if len(name) > nameMax {
		return failf(CodeInvalidRequest, PhaseValidate,
			"name %q is %d bytes, want at most %d", name, len(name), nameMax)
	}
	if !dnsLabel(name) {
		return failf(CodeInvalidRequest, PhaseValidate,
			"name %q is not a lowercase RFC 1123 label", name)
	}
	return nil
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// dnsLabel matches [a-z0-9]([-a-z0-9]*[a-z0-9])? without a regexp so the
// rule reads as the rule rather than as a pattern.
func dnsLabel(s string) bool {
	if s == "" {
		return false
	}
	alnum := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
	}
	if !alnum(s[0]) || !alnum(s[len(s)-1]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !alnum(s[i]) && s[i] != '-' {
			return false
		}
	}
	return true
}

// String renders an identity for stderr diagnostics.
func (id Identity) String() string {
	return fmt.Sprintf("device %d %s %s", id.Index, id.Model, id.MAC)
}
