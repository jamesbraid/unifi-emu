package herder

import (
	"strings"
	"testing"
)

func decodeRequestString(t *testing.T, body string) (Request, error) {
	t.Helper()
	return DecodeRequest(strings.NewReader(body))
}

func TestDecodeRequestAcceptsTheDocumentedShape(t *testing.T) {
	got, err := decodeRequestString(t, `{
	  "version": 1,
	  "devices": [
	    {"model": "USM8P"},
	    {"model": "UXGENT", "mac": "02:00:00:00:10:01",
	     "serial": "EMU020000001001", "name": "gateway-under-test"}
	  ]
	}`)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if got.Version != 1 || len(got.Devices) != 2 {
		t.Fatalf("request = %#v", got)
	}
	if got.Devices[1].Serial != "EMU020000001001" || got.Devices[1].Name != "gateway-under-test" {
		t.Fatalf("second device = %#v", got.Devices[1])
	}
}

func TestDecodeRequestRejectsUnknownField(t *testing.T) {
	_, err := decodeRequestString(t, `{"version":1,"devices":[{"model":"USM8P","vlan":7}]}`)
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

// The request schema has no ip: Docker and the runtime decide the address,
// so a caller trying to pin one must be told, not silently ignored.
func TestDecodeRequestRejectsIPField(t *testing.T) {
	_, err := decodeRequestString(t, `{"version":1,"devices":[{"model":"USM8P","ip":"172.28.0.9"}]}`)
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestDecodeRequestRejectsTrailingDocument(t *testing.T) {
	_, err := decodeRequestString(t,
		`{"version":1,"devices":[{"model":"USM8P"}]}`+"\n"+`{"version":1,"devices":[{"model":"UGW3"}]}`)
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestDecodeRequestRejectsTrailingGarbage(t *testing.T) {
	_, err := decodeRequestString(t, `{"version":1,"devices":[{"model":"USM8P"}]} oops`)
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestDecodeRequestAllowsTrailingWhitespace(t *testing.T) {
	if _, err := decodeRequestString(t, `{"version":1,"devices":[{"model":"USM8P"}]}`+"\n\n  "); err != nil {
		t.Fatalf("trailing whitespace rejected: %v", err)
	}
}

func TestDecodeRequestRejectsUnsupportedVersion(t *testing.T) {
	_, err := decodeRequestString(t, `{"version":2,"devices":[{"model":"USM8P"}]}`)
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestDecodeRequestRejectsEmptyDeviceList(t *testing.T) {
	_, err := decodeRequestString(t, `{"version":1,"devices":[]}`)
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestDecodeRequestRejectsEmptyInput(t *testing.T) {
	_, err := decodeRequestString(t, "")
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func TestDecodeRequestRejectsNonObject(t *testing.T) {
	_, err := decodeRequestString(t, `[{"model":"USM8P"}]`)
	assertFailure(t, err, CodeInvalidRequest, PhaseValidate)
}

func assertFailure(t *testing.T, err error, code Code, phase Phase) {
	t.Helper()
	if err == nil {
		t.Fatalf("want failure %s in phase %s, got nil", code, phase)
	}
	f, ok := asFailure(err)
	if !ok {
		t.Fatalf("error %v is not a *Failure", err)
	}
	if f.Code != code || f.Phase != phase {
		t.Fatalf("failure = %s/%s, want %s/%s (detail: %s)", f.Code, f.Phase, code, phase, f.Detail)
	}
}
