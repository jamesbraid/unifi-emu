package herder

import (
	"bufio"
	"context"
	"io"
	"sync"
)

// logSink serializes device output onto one stderr stream. Every device
// container writes through it, so interleaved fleets stay readable and no
// two writers tear a line apart.
type logSink struct {
	mu     sync.Mutex
	dst    io.Writer
	redact *redactor
}

func newLogSink(dst io.Writer, redact *redactor) *logSink {
	return &logSink{dst: dst, redact: redact}
}

// write emits one prefixed, redacted line.
func (s *logSink) write(prefix string, line []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Errors here are the herder's own stderr failing, which is not
	// something a device run can recover from or usefully report.
	_, _ = io.WriteString(s.dst, "["+prefix+"] "+s.redact.redact(string(line))+"\n")
}

// stream copies one container's output into the sink until it ends.
//
// Docker logs are opaque byte streams: this splits on newlines only to attach
// the public request index, never to interpret progress. A trailing partial
// line is emitted as it stands rather than held back, and nothing requires
// valid UTF-8.
func (s *logSink) stream(prefix string, src io.Reader) {
	reader := bufio.NewReader(src)
	for {
		line, err := reader.ReadBytes('\n')
		if n := len(line); n > 0 {
			if line[n-1] == '\n' {
				line = line[:n-1]
			}
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
			s.write(prefix, line)
		}
		if err != nil {
			return
		}
	}
}

// follow streams inst's output in the background until it closes or ctx ends.
func (s *logSink) follow(ctx context.Context, prefix string, inst Instance) {
	body, err := inst.Logs(ctx)
	if err != nil {
		s.write(prefix, []byte("log stream unavailable: "+s.redact.redact(err.Error())))
		return
	}
	defer body.Close()
	s.stream(prefix, body)
}
