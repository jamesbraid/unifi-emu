package herder

import (
	"encoding/json"
	"errors"
	"io"
)

// requestVersion is the only device-request version this protocol accepts.
const requestVersion = 1

// Request is the one strict JSON document the herder reads from --devices.
type Request struct {
	Version int             `json:"version"`
	Devices []RequestDevice `json:"devices"`
}

// RequestDevice is one requested device. Only model is required; the herder
// fills the rest. There is deliberately no ip field: Docker and the selected
// runtime decide the address after the container joins the caller's network,
// so a supplied one could only be a lie, and the strict decoder rejects it.
type RequestDevice struct {
	Model  string `json:"model"`
	MAC    string `json:"mac"`
	Serial string `json:"serial"`
	Name   string `json:"name"`
}

// DecodeRequest reads exactly one device request from r. Unknown fields, a
// trailing document, an unsupported version and an empty device list are all
// errors: a request the herder half-understands would start a fleet the
// caller did not ask for.
func DecodeRequest(r io.Reader) (Request, error) {
	var req Request
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			return Request{}, failf(CodeInvalidRequest, PhaseValidate, "the request is empty")
		}
		return Request{}, wrapf(err, CodeInvalidRequest, PhaseValidate, "decode request: %v", err)
	}
	// The input ends at EOF and carries one document. Anything after it --
	// a second request, a stray token -- would otherwise vanish silently.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Request{}, failf(CodeInvalidRequest, PhaseValidate,
				"the request carries more than one JSON document")
		}
		return Request{}, wrapf(err, CodeInvalidRequest, PhaseValidate,
			"trailing input after the request: %v", err)
	}
	if req.Version != requestVersion {
		return Request{}, failf(CodeInvalidRequest, PhaseValidate,
			"request version %d is not supported (want %d)", req.Version, requestVersion)
	}
	if len(req.Devices) == 0 {
		return Request{}, failf(CodeInvalidRequest, PhaseValidate, "the request lists no devices")
	}
	return req, nil
}
