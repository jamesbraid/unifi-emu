package herder

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
)

// fixedRand feeds Allocate a scripted byte sequence so MAC generation and its
// collision retry are deterministic under test.
type fixedRand struct {
	chunks [][]byte
	calls  int
}

func (f *fixedRand) Read(p []byte) (int, error) {
	if f.calls >= len(f.chunks) {
		return 0, io.EOF
	}
	n := copy(p, f.chunks[f.calls])
	f.calls++
	if n < len(p) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func mac(b ...byte) []byte { return b }

func TestAllocateDerivesEveryFieldFromAGeneratedMAC(t *testing.T) {
	r := &fixedRand{chunks: [][]byte{mac(0x00, 0x11, 0x22, 0x33, 0x44, 0x55)}}
	got, err := Allocate([]RequestDevice{{Model: "USM8P"}}, r)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	want := Identity{
		Index: 0, Model: "USM8P", MAC: "02:11:22:33:44:55",
		Serial: "EMU021122334455", Name: "emu-usm8p-021122334455",
	}
	if got[0] != want {
		t.Fatalf("identity = %#v, want %#v", got[0], want)
	}
}

// A generated MAC must be locally administered and unicast: bit 0 of the
// first octet cleared, bit 1 set, whatever the random source produced.
func TestGeneratedMACIsLocalUnicast(t *testing.T) {
	r := &fixedRand{chunks: [][]byte{mac(0xff, 0xff, 0xff, 0xff, 0xff, 0xff)}}
	got, err := Allocate([]RequestDevice{{Model: "UGW3"}}, r)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if !strings.HasPrefix(got[0].MAC, "fe:") {
		t.Fatalf("MAC = %s, want the multicast bit cleared and the local bit set", got[0].MAC)
	}
}

func TestGeneratedMACRetriesOnCollision(t *testing.T) {
	// The first generated MAC collides with the supplied one, so the second
	// chunk must be used instead of failing or duplicating.
	r := &fixedRand{chunks: [][]byte{
		mac(0x02, 0x00, 0x00, 0x00, 0x10, 0x01),
		mac(0x02, 0x00, 0x00, 0x00, 0x10, 0x02),
	}}
	got, err := Allocate([]RequestDevice{
		{Model: "UXGENT", MAC: "02:00:00:00:10:01"},
		{Model: "USM8P"},
	}, r)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got[1].MAC != "02:00:00:00:10:02" {
		t.Fatalf("second MAC = %s, want the retry candidate", got[1].MAC)
	}
	if r.calls != 2 {
		t.Fatalf("random source read %d times, want 2 (one collision retry)", r.calls)
	}
}

func TestGeneratedMACRetriesOnDerivedNameCollision(t *testing.T) {
	// Device 0 supplies the name the first candidate for device 1 would
	// derive, so generation must retry rather than emit a duplicate name.
	r := &fixedRand{chunks: [][]byte{
		mac(0x02, 0x11, 0x22, 0x33, 0x44, 0x55),
		mac(0x02, 0x11, 0x22, 0x33, 0x44, 0x56),
	}}
	got, err := Allocate([]RequestDevice{
		{Model: "UGW3", MAC: "02:aa:bb:cc:dd:ee", Name: "emu-usm8p-021122334455"},
		{Model: "USM8P"},
	}, r)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got[1].Name != "emu-usm8p-021122334456" {
		t.Fatalf("second name = %s, want the retry candidate", got[1].Name)
	}
}

func TestAllocateFailsWhenTheRandomSourceIsExhausted(t *testing.T) {
	_, err := Allocate([]RequestDevice{{Model: "USM8P"}}, &fixedRand{})
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestSuppliedMACIsCanonicalizedNotRegenerated(t *testing.T) {
	got, err := Allocate([]RequestDevice{{Model: "UXGENT", MAC: "02-00-00-00-10-01"}}, failingRand{})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got[0].MAC != "02:00:00:00:10:01" {
		t.Fatalf("MAC = %s, want lowercase colon-separated canonical form", got[0].MAC)
	}
	if got[0].Serial != "EMU020000001001" {
		t.Fatalf("serial = %s, want it derived from the canonical MAC", got[0].Serial)
	}
}

func TestSuppliedMACAcceptsUppercaseColonForm(t *testing.T) {
	got, err := Allocate([]RequestDevice{{Model: "UXGENT", MAC: "02:AA:BB:CC:DD:EE"}}, failingRand{})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got[0].MAC != "02:aa:bb:cc:dd:ee" {
		t.Fatalf("MAC = %s, want lowercase", got[0].MAC)
	}
}

// net.ParseMAC also accepts 8- and 20-byte addresses; only six-byte results
// are device MACs.
func TestSuppliedMACRejectsNonSixByteAddresses(t *testing.T) {
	_, err := Allocate([]RequestDevice{{Model: "UXGENT", MAC: "02:00:00:00:00:00:00:01"}}, failingRand{})
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestSuppliedMACRejectsGloballyAdministered(t *testing.T) {
	_, err := Allocate([]RequestDevice{{Model: "UXGENT", MAC: "00:27:22:e0:00:01"}}, failingRand{})
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestSuppliedMACRejectsMulticast(t *testing.T) {
	_, err := Allocate([]RequestDevice{{Model: "UXGENT", MAC: "03:00:00:00:10:01"}}, failingRand{})
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestSuppliedMACRejectsGarbage(t *testing.T) {
	_, err := Allocate([]RequestDevice{{Model: "UXGENT", MAC: "not-a-mac"}}, failingRand{})
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestDuplicateSuppliedMACsAreRejectedAcrossSpellings(t *testing.T) {
	_, err := Allocate([]RequestDevice{
		{Model: "UXGENT", MAC: "02:00:00:00:10:01"},
		{Model: "USM8P", MAC: "02-00-00-00-10-01"},
	}, failingRand{})
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestDuplicateSuppliedSerialsAreRejected(t *testing.T) {
	_, err := Allocate([]RequestDevice{
		{Model: "UXGENT", MAC: "02:00:00:00:10:01", Serial: "SAME"},
		{Model: "USM8P", MAC: "02:00:00:00:10:02", Serial: "SAME"},
	}, failingRand{})
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestDuplicateSuppliedNamesAreRejected(t *testing.T) {
	_, err := Allocate([]RequestDevice{
		{Model: "UXGENT", MAC: "02:00:00:00:10:01", Name: "same"},
		{Model: "USM8P", MAC: "02:00:00:00:10:02", Name: "same"},
	}, failingRand{})
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

// A supplied MAC is never replaced. If the serial it derives is already
// taken, the request is wrong and says so, rather than quietly getting a
// different MAC than the caller asked for.
func TestSuppliedMACWithCollidingDerivedSerialIsRejected(t *testing.T) {
	_, err := Allocate([]RequestDevice{
		{Model: "USM8P", MAC: "02:00:00:00:10:02", Serial: "EMU020000001001"},
		{Model: "UXGENT", MAC: "02:00:00:00:10:01"},
	}, failingRand{})
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestSuppliedMACWithCollidingDerivedNameIsRejected(t *testing.T) {
	_, err := Allocate([]RequestDevice{
		{Model: "USM8P", MAC: "02:00:00:00:10:02", Name: "emu-uxgent-020000001001"},
		{Model: "UXGENT", MAC: "02:00:00:00:10:01"},
	}, failingRand{})
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestSuppliedSerialAcceptsPrintableASCII(t *testing.T) {
	got, err := Allocate([]RequestDevice{
		{Model: "USM8P", MAC: "02:00:00:00:10:01", Serial: " ~printable/serial "},
	}, failingRand{})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got[0].Serial != " ~printable/serial " {
		t.Fatalf("serial = %q, want it preserved verbatim", got[0].Serial)
	}
}

func TestSuppliedSerialRejectsControlCharacters(t *testing.T) {
	for _, bad := range []string{"AB\tCD", "AB\nCD", "AB\x00CD", "AB\x7fCD"} {
		_, err := Allocate([]RequestDevice{{Model: "USM8P", MAC: "02:00:00:00:10:01", Serial: bad}}, failingRand{})
		assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
	}
}

func TestSuppliedSerialRejectsOverlongValue(t *testing.T) {
	_, err := Allocate([]RequestDevice{
		{Model: "USM8P", MAC: "02:00:00:00:10:01", Serial: strings.Repeat("A", 33)},
	}, failingRand{})
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestSuppliedSerialRejectsEmptyString(t *testing.T) {
	// An explicitly empty serial is not "absent": absent means the key is
	// missing, and JSON cannot tell them apart here, so the herder treats an
	// empty string as a request to derive one.
	got, err := Allocate([]RequestDevice{{Model: "USM8P", MAC: "02:00:00:00:10:01", Serial: ""}}, failingRand{})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got[0].Serial != "EMU020000001001" {
		t.Fatalf("serial = %q, want the derived value", got[0].Serial)
	}
}

func TestSuppliedNameMustBeAnRFC1123Label(t *testing.T) {
	bad := []string{
		"Gateway",               // uppercase
		"-leading",              // leading dash
		"trailing-",             // trailing dash
		"under_score",           // invalid byte
		"a.b",                   // dotted name, not one label
		strings.Repeat("a", 64), // 64 characters
	}
	for _, name := range bad {
		_, err := Allocate([]RequestDevice{{Model: "USM8P", MAC: "02:00:00:00:10:01", Name: name}}, failingRand{})
		assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
	}
}

func TestSuppliedNameIsPreserved(t *testing.T) {
	got, err := Allocate([]RequestDevice{
		{Model: "USM8P", MAC: "02:00:00:00:10:01", Name: "gateway-under-test"},
	}, failingRand{})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got[0].Name != "gateway-under-test" {
		t.Fatalf("name = %q, want it preserved", got[0].Name)
	}
}

func TestModelMustBeNonEmptyPrintableASCIIAndKeepsItsCase(t *testing.T) {
	if _, err := Allocate([]RequestDevice{{Model: ""}}, failingRand{}); err == nil {
		t.Fatal("empty model accepted")
	}
	if _, err := Allocate([]RequestDevice{{Model: "US\tM8P", MAC: "02:00:00:00:10:01"}}, failingRand{}); err == nil {
		t.Fatal("model with a control character accepted")
	}
	got, err := Allocate([]RequestDevice{{Model: "UxGeNt", MAC: "02:00:00:00:10:01"}}, failingRand{})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got[0].Model != "UxGeNt" {
		t.Fatalf("model = %q, want the exact case preserved", got[0].Model)
	}
}

func TestDerivedNameNormalizesTheModel(t *testing.T) {
	cases := map[string]string{
		"USM8P":                 "emu-usm8p-020000001001",
		"UDM-Pro":               "emu-udm-pro-020000001001",
		"U6+Lite":               "emu-u6-lite-020000001001",
		"--??--":                "emu-device-020000001001",
		"A..B":                  "emu-a-b-020000001001",
		strings.Repeat("Z", 60): "emu-" + strings.Repeat("z", 46) + "-020000001001",
	}
	for model, want := range cases {
		got, err := Allocate([]RequestDevice{{Model: model, MAC: "02:00:00:00:10:01"}}, failingRand{})
		if err != nil {
			t.Fatalf("Allocate(%q): %v", model, err)
		}
		if got[0].Name != want {
			t.Errorf("model %q derived name %q, want %q", model, got[0].Name, want)
		}
		if len(got[0].Name) > 63 {
			t.Errorf("model %q derived a %d-character name", model, len(got[0].Name))
		}
		if !rfc1123Label.MatchString(got[0].Name) {
			t.Errorf("model %q derived an invalid label %q", model, got[0].Name)
		}
	}
}

func TestAllocatePreservesRequestOrderAndIndexes(t *testing.T) {
	r := &fixedRand{chunks: [][]byte{
		mac(0x02, 0, 0, 0, 0, 1), mac(0x02, 0, 0, 0, 0, 2), mac(0x02, 0, 0, 0, 0, 3),
	}}
	got, err := Allocate([]RequestDevice{{Model: "A"}, {Model: "B"}, {Model: "C"}}, r)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	for i, want := range []string{"A", "B", "C"} {
		if got[i].Index != i || got[i].Model != want {
			t.Fatalf("device %d = %#v", i, got[i])
		}
	}
}

func TestAllocateUsesCryptoRandomBytesByDefault(t *testing.T) {
	// Two allocations from the process default source must not agree: a
	// constant or counter source would make fleets collide across runs.
	first, err := Allocate([]RequestDevice{{Model: "USM8P"}}, nil)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	second, err := Allocate([]RequestDevice{{Model: "USM8P"}}, nil)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if first[0].MAC == second[0].MAC {
		t.Fatalf("two allocations produced the same MAC %s", first[0].MAC)
	}
}

var rfc1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// failingRand proves a code path never reaches the random source.
type failingRand struct{}

func (failingRand) Read([]byte) (int, error) {
	return 0, errors.New("random source must not be used for a supplied MAC")
}

var _ = bytes.MinRead
