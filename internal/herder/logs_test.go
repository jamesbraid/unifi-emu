package herder

import (
	"strings"
	"testing"
)

func TestLogLinesArePrefixedWithTheRequestIndexSet(t *testing.T) {
	var out strings.Builder
	sink := newLogSink(&out, newRedactor(RuntimeConfig{}))
	sink.stream("0,1,2", strings.NewReader("booting\nfleet ready\n"))
	want := "[0,1,2] booting\n[0,1,2] fleet ready\n"
	if out.String() != want {
		t.Fatalf("stderr = %q, want %q", out.String(), want)
	}
}

// A container that dies mid-line still has something to say, so the partial
// line is emitted rather than held back waiting for a newline that never
// arrives.
func TestTrailingPartialLineIsStillEmitted(t *testing.T) {
	var out strings.Builder
	sink := newLogSink(&out, newRedactor(RuntimeConfig{}))
	sink.stream("1", strings.NewReader("panic: nil map"))
	if out.String() != "[1] panic: nil map\n" {
		t.Fatalf("stderr = %q", out.String())
	}
}

// Docker logs are opaque byte streams: invalid UTF-8 must pass through, not
// break the copy or get replaced.
func TestNonUTF8OutputIsCopiedThrough(t *testing.T) {
	var out strings.Builder
	sink := newLogSink(&out, newRedactor(RuntimeConfig{}))
	sink.stream("0", strings.NewReader("raw \xff\xfe bytes\n"))
	if !strings.Contains(out.String(), "\xff\xfe") {
		t.Fatalf("stderr = %q, want the raw bytes preserved", out.String())
	}
}

func TestCarriageReturnsAreTrimmedWithTheNewline(t *testing.T) {
	var out strings.Builder
	sink := newLogSink(&out, newRedactor(RuntimeConfig{}))
	sink.stream("0", strings.NewReader("windows line\r\n"))
	if out.String() != "[0] windows line\n" {
		t.Fatalf("stderr = %q", out.String())
	}
}

// Operators need device logs to debug a runtime that will not start, but a
// public job must not learn which image produced them.
func TestDeviceLogsAreRedacted(t *testing.T) {
	cfg := RuntimeConfig{Version: 1, Models: map[string]ModelRuntime{
		"UXGENT": {Image: "forge.example/emu/uxgent@" + testDigest},
	}}
	var out strings.Builder
	sink := newLogSink(&out, newRedactor(cfg))
	sink.stream("1", strings.NewReader("pulled forge.example/emu/uxgent@"+testDigest+" ok\n"))
	if strings.Contains(out.String(), "forge.example") {
		t.Fatalf("stderr leaks the runtime reference: %q", out.String())
	}
	if !strings.Contains(out.String(), redactedRuntime) {
		t.Fatalf("stderr = %q, want the placeholder", out.String())
	}
}

// Two containers writing at once must not tear each other's lines apart.
func TestConcurrentDeviceOutputIsSerialized(t *testing.T) {
	var out strings.Builder
	sink := newLogSink(&out, newRedactor(RuntimeConfig{}))
	done := make(chan struct{}, 2)
	for _, prefix := range []string{"0", "1"} {
		go func() {
			defer func() { done <- struct{}{} }()
			sink.stream(prefix, strings.NewReader(strings.Repeat("line\n", 200)))
		}()
	}
	<-done
	<-done
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line != "[0] line" && line != "[1] line" {
			t.Fatalf("torn line %q", line)
		}
	}
}
