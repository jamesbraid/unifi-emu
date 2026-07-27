package firmware

import (
	"testing"
)

func TestAnalyzeClassifiesFirmwareVocabularyWithOffsets(t *testing.T) {
	files := []decodedFile{{
		Path: "firmware/kernel/usr/bin/mcad",
		Data: []byte("noise\x00fw_caps\x00cfgversion\x00cfgversion_effective\x00setparam\x00" +
			"poe_caps=%u\x00not_relevant\x00"),
	}}
	got := Analyze(files, 4)
	want := map[string]int64{
		"capability:fw_caps":                6,
		"inform_field:cfgversion":           14,
		"inform_field:cfgversion_effective": 25,
		"command:setparam":                  46,
		"capability:poe_caps":               55,
	}
	for key, offset := range want {
		found := false
		for _, observation := range got {
			if observation.Kind+":"+observation.Value == key && observation.Offset == offset &&
				observation.Artifact == "firmware/kernel/usr/bin/mcad" {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s at %d: %+v", key, offset, got)
		}
	}
	for _, observation := range got {
		if observation.Value == "not_relevant" {
			t.Fatalf("unclassified string leaked into evidence: %+v", observation)
		}
	}
}

func TestAnalyzeDoesNotMatchCapabilityInsideIdentifier(t *testing.T) {
	got := Analyze([]decodedFile{{Path: "mcad", Data: []byte("prefixfw_capssuffix\x00")}}, 4)
	if len(got) != 0 {
		t.Fatalf("substring produced false evidence: %+v", got)
	}
}

func TestAnalyzeIncludesNonAgentFiles(t *testing.T) {
	got := Analyze([]decodedFile{
		{Path: "root/etc/board.json", Data: []byte("hw_caps\x00")},
		{Path: "root/usr/bin/mcad", Data: []byte("fw_caps\x00")},
	}, 4)
	if len(got) != 2 {
		t.Fatalf("observations = %+v, want evidence from every extracted file", got)
	}
}

func TestAnalyzeDoesNotPromoteFunctionNamesToCapabilityFields(t *testing.T) {
	got := Analyze([]decodedFile{{
		Path: "mcad",
		Data: []byte("ubnthal_get_wifi_caps\x00param_for_tuning_caps\x00sys_error_caps\x00"),
	}}, 4)
	if len(got) != 0 {
		t.Fatalf("function identifiers produced false capability evidence: %+v", got)
	}
}
