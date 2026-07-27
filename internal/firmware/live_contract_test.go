package firmware

import (
	"os"
	"strings"
	"testing"
)

func TestParseLiveSnapshotRejectsTrailingJSON(t *testing.T) {
	if _, _, err := ParseLiveSnapshot(strings.NewReader(`{"fw_caps":3} {"fw_caps":4}`)); err == nil {
		t.Fatal("accepted multiple JSON values as one live snapshot")
	}
}

func TestParseCapabilityDefinitionsRejectsTrailingJSON(t *testing.T) {
	input := `{"bitmaps":{"TEST":{"field":"fw_caps","bits":{"ONE":1}}}} {}`
	if _, err := ParseCapabilityDefinitions(strings.NewReader(input)); err == nil {
		t.Fatal("accepted multiple JSON values as one capability definition")
	}
}

func TestParseCapabilityDefinitionsAcceptsUnsigned64Bit(t *testing.T) {
	definitions, err := ParseCapabilityDefinitions(strings.NewReader(`{
		"bitmaps":{"TEST":{"field":"fw_caps","bits":{"TOP":9223372036854775808}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := definitions["fw_caps"][0].Bits["TOP"]; got != uint64(1)<<63 {
		t.Fatalf("TOP = %d", got)
	}
}

func TestParseCapabilityDefinitionsRejectsNegativeOutsideInt32(t *testing.T) {
	_, err := ParseCapabilityDefinitions(strings.NewReader(`{
		"bitmaps":{"TEST":{"field":"fw_caps","bits":{"BAD":-2147483649}}}
	}`))
	if err == nil {
		t.Fatal("accepted a negative capability constant outside Java int range")
	}
}

func TestParseCapabilityDefinitionsRejectsNonBitConstants(t *testing.T) {
	for name, value := range map[string]string{"zero": "0", "multi-bit": "3"} {
		t.Run(name, func(t *testing.T) {
			input := `{"bitmaps":{"TEST":{"field":"fw_caps","bits":{"BAD":` + value + `}}}}`
			if _, err := ParseCapabilityDefinitions(strings.NewReader(input)); err == nil {
				t.Fatalf("accepted non-bit constant %s", value)
			}
		})
	}
}

func TestDecodeCapabilityValuesNormalizesSignedJavaMask(t *testing.T) {
	definitions, err := ParseCapabilityDefinitions(strings.NewReader(`{
		"bitmaps":{"TEST":{"field":"fw_caps","bits":{"TOP":-2147483648}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	values := DecodeCapabilityValues([]LiveField{{
		Path: "/fw_caps", Type: "number", Value: "-2147483648",
	}}, definitions)
	if len(values) != 1 || strings.Join(values[0].SetBits, ",") != "TOP" ||
		values[0].UnknownMask != "" || values[0].RawValue != "-2147483648" {
		t.Fatalf("decoded signed mask = %+v", values)
	}
}

func TestCapabilityDecoderAcceptsRepositoryDefinitions(t *testing.T) {
	file, err := os.Open("../../capability_bits.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	definitions, err := ParseCapabilityDefinitions(file)
	if err != nil {
		t.Fatal(err)
	}
	values := DecodeCapabilityValues([]LiveField{{
		Path: "/feature_caps", Type: "number", Value: "-2147483647",
	}}, definitions)
	if len(values) != 1 {
		t.Fatalf("feature_caps values = %+v", values)
	}
	if got := strings.Join(values[0].SetBits, ","); got !=
		"UNIFI_SWITCH_CAP_L3_ROUTING,UNIFI_SWITCH_CAP_SMA_PORT" {
		t.Fatalf("feature_caps set bits = %q", got)
	}
	if values[0].UnknownMask != "" {
		t.Fatalf("feature_caps unknown mask = %q", values[0].UnknownMask)
	}
}
