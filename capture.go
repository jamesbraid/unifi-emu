package emu

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// Capturing the exchange is how a claim about the protocol earns a basis. The
// emulator otherwise logs only what went wrong, so a device that adopts
// cleanly leaves no record of what it sent or what the controller sent back --
// which is exactly the material needed to describe a payload shape as observed
// rather than asserted.
//
// Set SIM_CAPTURE to a path and every decoded inform and every decoded
// controller reply is appended there as one JSON object per line. Unset, the
// capture costs one nil check per inform and nothing else.
//
// The file holds whatever the device reported and whatever the controller
// pushed, including the authentication key the two agreed on. Treat it as
// sensitive and scrub it before sharing.

const captureEnv = "SIM_CAPTURE"

type captureSink struct {
	mu sync.Mutex
	f  *os.File
}

var (
	captureOnce sync.Once
	capture     *captureSink
)

// captureTo returns the process-wide sink, opening the file on first use. A
// path that cannot be opened disables capture with one log line rather than
// failing the run: a fleet that is informing correctly should not stop because
// its observer could not start.
func captureTo() *captureSink {
	captureOnce.Do(func() {
		path := os.Getenv(captureEnv)
		if path == "" {
			return
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			log.Printf("capture: %v (continuing without it)", err)
			return
		}
		capture = &captureSink{f: f}
		log.Printf("capture: writing the inform exchange to %s", path)
	})
	return capture
}

// captureRecord is one direction of one exchange.
type captureRecord struct {
	Time      time.Time       `json:"time"`
	MAC       string          `json:"mac"`
	Model     string          `json:"model"`
	Direction string          `json:"direction"` // "inform" or "response"
	Body      json.RawMessage `json:"body"`
}

// record appends one decoded payload. Body is stored as raw JSON so the
// capture holds exactly what crossed the wire, not this package's idea of it:
// a field the emulator does not model still lands in the file.
func (c *captureSink) record(mac, model, direction string, body []byte) {
	if c == nil {
		return
	}
	rec := captureRecord{
		Time:      time.Now().UTC(),
		MAC:       mac,
		Model:     model,
		Direction: direction,
		Body:      json.RawMessage(body),
	}
	line, err := json.Marshal(rec)
	if err != nil {
		// The payload was not valid JSON, which is itself worth seeing.
		line, err = json.Marshal(captureRecord{
			Time: rec.Time, MAC: mac, Model: model, Direction: direction,
			Body: json.RawMessage(`null`),
		})
		if err != nil {
			return
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.f.Write(append(line, '\n'))
}
