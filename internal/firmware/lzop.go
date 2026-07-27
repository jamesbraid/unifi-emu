package firmware

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

var lzopMagic = []byte{0x89, 'L', 'Z', 'O', 0, '\r', '\n', 0x1a, '\n'}

var errExpandedLimit = errors.New("expanded byte limit exceeded")

// decompressLZOP follows u-root's lzop strategy: use the reference lzop
// executable instead of maintaining another implementation of its container
// format. The LZO stream and all framing/checksums are therefore handled by
// lzop itself.
func decompressLZOP(data []byte, maxExpanded int64) ([]byte, error) {
	path, err := exec.LookPath("lzop")
	if err != nil {
		return nil, fmt.Errorf("decode lzop: install the lzop executable: %w", err)
	}
	cmd := exec.Command(path, "-dc")
	cmd.Stdin = bytes.NewReader(data)
	output := &limitBuffer{remaining: maxExpanded}
	var stderr limitBuffer
	stderr.remaining = 8 << 10
	cmd.Stdout = output
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(output.err, errExpandedLimit) {
			return nil, fmt.Errorf("lzop expanded bytes exceed limit %d", maxExpanded)
		}
		return nil, fmt.Errorf("decode lzop: %w: %s", err, bytes.TrimSpace(stderr.buf.Bytes()))
	}
	return output.buf.Bytes(), nil
}

type limitBuffer struct {
	buf       bytes.Buffer
	remaining int64
	err       error
}

func (w *limitBuffer) Write(p []byte) (int, error) {
	if w.remaining < int64(len(p)) {
		allowed := max(w.remaining, 0)
		if allowed > 0 {
			_, _ = w.buf.Write(p[:allowed])
			w.remaining -= allowed
		}
		w.err = errExpandedLimit
		return int(allowed), w.err
	}
	n, err := w.buf.Write(p)
	w.remaining -= int64(n)
	return n, err
}

var _ io.Writer = (*limitBuffer)(nil)
