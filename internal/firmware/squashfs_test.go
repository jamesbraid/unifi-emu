package firmware

import "testing"

func TestParseSquashFSRejectsTruncatedImageWithoutPanicking(t *testing.T) {
	data := []byte{'h', 's', 'q', 's'}
	if _, err := parseSquashFS(data, DefaultLimits()); err == nil {
		t.Fatal("accepted truncated SquashFS image")
	}
}

func TestNestedWiFiSquashFSParserGapIsWarning(t *testing.T) {
	state := decoderState{limits: DefaultLimits()}
	state.walk("firmware.bin/wififw", []byte{'h', 's', 'q', 's'}, 0, "", 1)
	if len(state.result.Warnings) != 1 || len(state.result.Failures) != 0 {
		t.Fatalf("warnings = %v, failures = %v", state.result.Warnings, state.result.Failures)
	}
	if len(state.result.Artifacts) != 1 ||
		state.result.Artifacts[0].Path != "firmware.bin/wififw" {
		t.Fatalf("partial artifact evidence = %+v", state.result.Artifacts)
	}
}

func TestRootSquashFSParserGapRemainsFailure(t *testing.T) {
	for _, test := range []struct {
		name  string
		depth int
	}{
		{name: "firmware.bin", depth: 0},
		{name: "firmware.bin/rootfs", depth: 1},
	} {
		state := decoderState{limits: DefaultLimits()}
		state.walk(test.name, []byte{'h', 's', 'q', 's'}, 0, "", test.depth)
		if len(state.result.Failures) != 1 || len(state.result.Warnings) != 0 {
			t.Fatalf("%s: warnings = %v, failures = %v",
				test.name, state.result.Warnings, state.result.Failures)
		}
	}
}
