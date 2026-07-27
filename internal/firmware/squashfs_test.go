package firmware

import (
	"errors"
	"testing"
)

func TestParseSquashFSRejectsTruncatedImageWithoutPanicking(t *testing.T) {
	data := []byte{'h', 's', 'q', 's'}
	if _, err := parseSquashFS(data, DefaultLimits()); err == nil {
		t.Fatal("accepted truncated SquashFS image")
	}
}

func TestReadSquashFSContentDoesNotReadEmptyFile(t *testing.T) {
	content, err := readSquashFSContent(errorReader{}, "empty", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != 0 {
		t.Fatalf("empty file content = %q", content)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("unexpected read")
}
