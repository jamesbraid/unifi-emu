package emu

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jamesbraid/unifi-emu/inform"
)

func ugwSpec() DeviceSpec {
	return DeviceSpec{MAC: "dc:9f:db:00:00:01", Model: "UGW3", IP: "10.0.0.1"}
}

func uswSpec() DeviceSpec {
	return DeviceSpec{MAC: "00:27:22:00:00:02", Model: "USWED74", IP: "10.0.0.3"}
}

func uapSpec() DeviceSpec {
	return DeviceSpec{MAC: "00:15:6d:00:00:01", Model: "U7MP", IP: "10.0.0.57"}
}

// fakeController is a minimal in-memory UniFi controller speaking the real
// inform wire format. Until adoptCalled is set it answers every inform with
// 404 (nothing queued), like a real controller does for pending devices. Once
// adoption is "clicked" it answers a default-key inform with set-adopt and
// adopted-key informs with a noop, exactly the real adoption handshake.
type fakeController struct {
	adoptKey   string
	server     *httptest.Server
	stateProbe func() DeviceState // optional; sampled at the first adopt-key inform

	informs    atomic.Int64
	sawDefault atomic.Bool
	sawAdopted atomic.Bool
	// adoptCalled is the "user clicked Adopt in the UI" switch.
	adoptCalled atomic.Bool
	// firstAdoptedState is the probed device state when the first adopt-key
	// inform arrived, -1 until then.
	firstAdoptedState atomic.Int32
	// firstAdoptedProtocolState is the state field in the first inform
	// encrypted with the adopted key.
	firstAdoptedProtocolState atomic.Int32
	lastPayload               atomic.Value // map[string]any of the latest inform
}

// newFakeController starts the controller. stateProbe, when non-nil, must be
// supplied here: anything stored on the struct after httptest.NewServer has
// spawned its goroutines would race with the handlers.
func newFakeController(t *testing.T, stateProbe func() DeviceState) *fakeController {
	t.Helper()
	fc := &fakeController{adoptKey: adoptKey, stateProbe: stateProbe}
	fc.firstAdoptedState.Store(-1)
	fc.firstAdoptedProtocolState.Store(-1)
	fc.server = httptest.NewServer(http.HandlerFunc(fc.handleInform))
	t.Cleanup(fc.server.Close)
	return fc
}

func (fc *fakeController) informURL() string { return fc.server.URL + "/inform" }

func (fc *fakeController) handleInform(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	pkt, err := inform.Decode(body, inform.DefaultKey)
	usedDefault := err == nil
	if !usedDefault {
		pkt, err = inform.Decode(body, fc.adoptKey)
	}
	if err != nil {
		http.Error(w, "undecryptable", http.StatusBadRequest)
		return
	}
	fc.informs.Add(1)
	if usedDefault {
		fc.sawDefault.Store(true)
	} else {
		fc.sawAdopted.Store(true)
		if fc.stateProbe != nil {
			fc.firstAdoptedState.CompareAndSwap(-1, int32(fc.stateProbe()))
		}
	}
	var m map[string]any
	if err := json.Unmarshal(pkt.Payload, &m); err == nil {
		fc.lastPayload.Store(m)
		if !usedDefault {
			if state, ok := m["state"].(float64); ok {
				fc.firstAdoptedProtocolState.CompareAndSwap(-1, int32(state))
			}
		}
	}

	if !fc.adoptCalled.Load() {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// The real controller encrypts its reply with the key the device used.
	key := fc.adoptKey
	reply := `{"_type":"noop","interval":1}`
	if usedDefault {
		key = inform.DefaultKey
		reply = `{"_type":"cmd","cmd":"set-adopt","key":"` + fc.adoptKey +
			`","uri":"` + fc.server.URL + `/inform"}`
	}
	enc, err := (&inform.Packet{MAC: pkt.MAC, Payload: []byte(reply)}).Encode(key)
	if err != nil {
		http.Error(w, "encode reply", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-binary")
	_, _ = w.Write(enc)
}

func deviceKey(d *device) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.key
}

func TestDeviceLoopFullHandshake(t *testing.T) {
	specs := map[string]DeviceSpec{
		"ugw": ugwSpec(),
		"usw": uswSpec(),
		"uap": uapSpec(),
	}
	for name, spec := range specs {
		t.Run(name, func(t *testing.T) {
			d, err := newDevice(spec, "http://placeholder.invalid/inform")
			if err != nil {
				t.Fatalf("newDevice: %v", err)
			}
			fc := newFakeController(t, d.State)
			d.informURL = fc.informURL()
			d.interval = 20 * time.Millisecond

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go d.run(ctx)

			// Phase 1: pending device, controller has nothing queued (404s).
			time.Sleep(150 * time.Millisecond)
			if got := fc.informs.Load(); got < 3 {
				t.Errorf("informs after 150ms = %d, want >= 3", got)
			}
			if !fc.sawDefault.Load() {
				t.Error("controller never saw a default-key inform")
			}
			if got := d.State(); got != StatePending {
				t.Errorf("state = %v, want PENDING while controller 404s", got)
			}
			if p, ok := fc.lastPayload.Load().(map[string]any); !ok || p["default"] != true {
				t.Errorf("last payload default flag = %v, want true while pending", p["default"])
			}

			// Phase 2: user clicks Adopt. Device must rotate keys mid-stream,
			// inform once with the adopt key while ADOPTING, then connect.
			fc.adoptCalled.Store(true)
			deadline := time.Now().Add(2 * time.Second)
			for d.State() != StateConnected {
				if time.Now().After(deadline) {
					t.Fatalf("state = %v after 2s, want CONNECTED", d.State())
				}
				time.Sleep(5 * time.Millisecond)
			}
			if !fc.sawAdopted.Load() {
				t.Error("controller never saw an adopt-key inform")
			}
			if got := deviceKey(d); got != adoptKey {
				t.Errorf("key = %q, want rotated key %q", got, adoptKey)
			}
			if got := DeviceState(fc.firstAdoptedState.Load()); got != StateAdopting {
				t.Errorf("first adopt-key inform sent in state %v, want ADOPTING", got)
			}
			if got := fc.firstAdoptedProtocolState.Load(); got != 4 {
				t.Errorf("first adopt-key inform protocol state = %d, want 4", got)
			}
		})
	}
}

func TestDeviceLoopSwitchesToNegotiatedGCM(t *testing.T) {
	var sawGCM atomic.Bool
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		call := calls.Add(1)
		key := inform.DefaultKey
		if call > 1 {
			key = adoptKey
			if len(body) >= 16 && binary.BigEndian.Uint16(body[14:16])&(1<<3) != 0 {
				sawGCM.Store(true)
			}
		}
		pkt, err := inform.Decode(body, key)
		if err != nil {
			http.Error(w, "decode request", http.StatusBadRequest)
			return
		}

		reply := `{"_type":"setparam","mgmt_cfg":"cfgversion=abc123\nauthkey=` +
			adoptKey + `\nuse_aes_gcm=true\n"}`
		if call > 1 {
			reply = `{"_type":"noop"}`
		}
		response := &inform.Packet{MAC: pkt.MAC, Payload: []byte(reply)}
		var enc []byte
		if call > 1 {
			enc, err = response.EncodeGCM(key)
		} else {
			enc, err = response.Encode(key)
		}
		if err != nil {
			http.Error(w, "encode response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-binary")
		_, _ = w.Write(enc)
	}))
	t.Cleanup(srv.Close)

	d, err := newDevice(uapSpec(), srv.URL+"/inform")
	if err != nil {
		t.Fatalf("newDevice: %v", err)
	}
	d.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for !sawGCM.Load() {
		if time.Now().After(deadline) {
			t.Fatal("device never switched to a GCM inform after use_aes_gcm=true")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDeviceLoopStopsOnContextCancel(t *testing.T) {
	fc := newFakeController(t, nil)
	d, err := newDevice(ugwSpec(), fc.informURL())
	if err != nil {
		t.Fatalf("newDevice: %v", err)
	}
	d.interval = time.Hour // only ctx.Done may wake the loop

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for fc.informs.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no inform within 2s; loop never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return within 2s of cancel")
	}
}

// lockedBuffer is a bytes.Buffer safe for concurrent log writes and test
// reads; the log package serializes each Write, the test reads in parallel.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The 404 verdict needs per-device evidence of the inform HTTP-status
// flow: the first 404 must speak up (pending is normal), repeats must
// stay silent, and the first 200 must say how many 404s preceded it.
func TestInformStatusTransitionsLogged(t *testing.T) {
	var logs lockedBuffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 { // two "pending, nothing queued" replies, then 200s
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		pkt, err := inform.Decode(body, inform.DefaultKey)
		if err != nil {
			http.Error(w, "undecryptable", http.StatusBadRequest)
			return
		}
		enc, err := (&inform.Packet{MAC: pkt.MAC, Payload: []byte(`{"_type":"noop","interval":1}`)}).Encode(inform.DefaultKey)
		if err != nil {
			http.Error(w, "encode reply", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-binary")
		_, _ = w.Write(enc)
	}))
	t.Cleanup(srv.Close)

	d, err := newDevice(ugwSpec(), srv.URL+"/inform")
	if err != nil {
		t.Fatalf("newDevice: %v", err)
	}
	d.interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for {
		out := logs.String()
		if strings.Contains(out, "inform: HTTP 200 after 2 x 404") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing 404->200 transition logs, got:\n%s", out)
		}
		time.Sleep(5 * time.Millisecond)
	}

	out := logs.String()
	mac := "dc:9f:db:00:00:01"
	first404 := "[" + mac + "] inform: HTTP 404 (nothing queued)"
	if !strings.Contains(out, first404) {
		t.Errorf("missing first-404 log %q, got:\n%s", first404, out)
	}
	if n := strings.Count(out, "HTTP 404"); n != 1 {
		t.Errorf("404 logged %d times, want exactly 1 (repeats stay silent):\n%s", n, out)
	}
	if !strings.Contains(out, "["+mac+"] inform: HTTP 200 after 2 x 404") {
		t.Errorf("missing 200-after-404s transition, got:\n%s", out)
	}
	time.Sleep(50 * time.Millisecond) // several more 200 informs must stay silent
	if n := strings.Count(logs.String(), "HTTP 200"); n != 1 {
		t.Errorf("200 logged %d times, want exactly 1 (repeats stay silent)", n)
	}
}
