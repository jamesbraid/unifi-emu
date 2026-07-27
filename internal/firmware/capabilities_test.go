package firmware

import (
	"strings"
	"testing"
)

func TestDecodeCapabilityValuesNamesSetAndUnknownBits(t *testing.T) {
	definitions, err := ParseCapabilityDefinitions(strings.NewReader(`{
		"bitmaps": {
			"UNIFI_TEST_CAP": {
				"field": "feature_caps",
				"bits": {"ONE": 1, "SIGN_BIT": -2147483648}
			}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	values := DecodeCapabilityValues([]LiveField{{
		Path: "/device/feature_caps", Type: "number", Value: "2147483653",
	}}, definitions)
	if len(values) != 1 {
		t.Fatalf("values = %+v", values)
	}
	if got := strings.Join(values[0].SetBits, ","); got != "ONE,SIGN_BIT" {
		t.Fatalf("set bits = %q", got)
	}
	if values[0].UnknownMask != "4" {
		t.Fatalf("unknown mask = %q", values[0].UnknownMask)
	}
}
