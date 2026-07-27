package firmware

import (
	"strings"
	"testing"
)

func TestParseCatalogueSelectsAndDeduplicatesImages(t *testing.T) {
	const checksum = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	input := `{"_embedded":{"firmware":[
		{"product":"unifi-firmware","platform":"U7PRO","version":"v8.6.11+18870","file_size":4,
		 "sha256_checksum":"` + checksum + `","_links":{"data":{"href":"https://example/U7PRO.bin"}}},
		{"product":"unifi-firmware","platform":"U7PROXG","version":"v8.6.11+18870","file_size":4,
		 "sha256_checksum":"` + checksum + `","_links":{"data":{"href":"https://example/U7PRO.bin"}}},
		{"product":"other","platform":"IGNORE","version":"v1","file_size":1,
		 "sha256_checksum":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	]}}`
	records, err := ParseCatalogue(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	selected, err := SelectRecords(records, Selection{Platforms: []string{"U7PROXG", "U7PRO"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 {
		t.Fatalf("selected %d images, want one checksum-deduplicated image", len(selected))
	}
	if got := strings.Join(selected[0].Platforms, ","); got != "U7PRO,U7PROXG" {
		t.Fatalf("platform aliases = %q", got)
	}
	if selected[0].Version != "8.6.11.18870" {
		t.Fatalf("version = %q", selected[0].Version)
	}
}

func TestSelectRecordsRejectsInvalidOrUnmatchedSelection(t *testing.T) {
	records := []FirmwareRecord{{
		Product: "unifi-firmware", Platform: "UAP", Version: "1.2.3",
		SHA256: strings.Repeat("a", 64), Size: 12, URL: "https://example/uap.bin",
	}}
	if _, err := SelectRecords(records, Selection{SHA256: "bad"}); err == nil {
		t.Fatal("accepted malformed checksum")
	}
	if _, err := SelectRecords(records, Selection{Platforms: []string{"MISSING"}}); err == nil {
		t.Fatal("accepted selection with no matches")
	}
}

func TestSelectRecordsRejectsConflictingDuplicateMetadata(t *testing.T) {
	checksum := strings.Repeat("a", 64)
	records := []FirmwareRecord{
		{Product: "unifi-firmware", Platform: "ONE", Version: "1", SHA256: checksum, Size: 10, URL: "https://example/one"},
		{Product: "unifi-firmware", Platform: "TWO", Version: "2", SHA256: checksum, Size: 11, URL: "https://example/two"},
	}
	if _, err := SelectRecords(records, Selection{All: true}); err == nil {
		t.Fatal("accepted conflicting metadata for one checksum")
	}
}

func TestSelectRecordsRetainsURLAliasesForSameImage(t *testing.T) {
	checksum := strings.Repeat("a", 64)
	records := []FirmwareRecord{
		{Product: "unifi-firmware", Platform: "ONE", Version: "1", SHA256: checksum, Size: 10, URL: "https://b.example/image"},
		{Product: "unifi-firmware", Platform: "TWO", Version: "1", SHA256: checksum, Size: 10, URL: "https://a.example/image"},
	}
	selected, err := SelectRecords(records, Selection{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(selected[0].URLs, ","); got != "https://a.example/image,https://b.example/image" {
		t.Fatalf("URL aliases = %q", got)
	}
	if selected[0].URL != "https://a.example/image" {
		t.Fatalf("canonical URL = %q", selected[0].URL)
	}
}
