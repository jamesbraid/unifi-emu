package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jamesbraid/unifi-emu/inform"
)

func TestRecorderPersistsDecodedInform(t *testing.T) {
	outDir := t.TempDir()
	recorder := newRecorder(outDir)
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)

	mac := [6]byte{0x00, 0x27, 0x22, 0xe0, 0x00, 0x01}
	wire, err := (&inform.Packet{
		MAC:     mac,
		Payload: []byte(`{"model":"UXGENT","version":"5.0.16","state":0}`),
	}).Encode(inform.DefaultKey)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}

	resp, err := http.Post(server.URL+"/inform", "application/x-binary", bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("post inform: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	raw, err := os.ReadFile(filepath.Join(outDir, "inform-000001.bin"))
	if err != nil {
		t.Fatalf("read raw capture: %v", err)
	}
	if !bytes.Equal(raw, wire) {
		t.Fatal("raw capture differs from request body")
	}

	decoded, err := os.ReadFile(filepath.Join(outDir, "inform-000001.json"))
	if err != nil {
		t.Fatalf("read decoded capture: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("decode captured JSON: %v", err)
	}
	if got := payload["model"]; got != "UXGENT" {
		t.Fatalf("model = %v, want UXGENT", got)
	}

	metadata, err := os.ReadFile(filepath.Join(outDir, "inform-000001.meta.json"))
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta captureMetadata
	if err := json.Unmarshal(metadata, &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta.MAC != "00:27:22:e0:00:01" {
		t.Fatalf("metadata MAC = %q", meta.MAC)
	}
	if meta.ContentType != "application/x-binary" {
		t.Fatalf("metadata content type = %q", meta.ContentType)
	}
}

func TestRecorderRestartPreservesExistingCapture(t *testing.T) {
	outDir := t.TempDir()
	firstWire, err := (&inform.Packet{
		MAC:     [6]byte{0x00, 0x27, 0x22, 0xe0, 0x00, 0x01},
		Payload: []byte(`{"model":"UXGENT"}`),
	}).Encode(inform.DefaultKey)
	if err != nil {
		t.Fatalf("encode first fixture: %v", err)
	}
	firstRequest := httptest.NewRequest(http.MethodPost, "/inform", bytes.NewReader(firstWire))
	firstRequest.Header.Set("Content-Type", "application/x-binary")
	newRecorder(outDir).ServeHTTP(httptest.NewRecorder(), firstRequest)

	secondWire, err := (&inform.Packet{
		MAC:     [6]byte{0x00, 0x27, 0x22, 0xe0, 0x00, 0x02},
		Payload: []byte(`{"model":"U7PRO"}`),
	}).Encode(inform.DefaultKey)
	if err != nil {
		t.Fatalf("encode second fixture: %v", err)
	}
	secondRequest := httptest.NewRequest(http.MethodPost, "/inform", bytes.NewReader(secondWire))
	secondRequest.Header.Set("Content-Type", "application/x-binary")
	newRecorder(outDir).ServeHTTP(httptest.NewRecorder(), secondRequest)

	gotFirst, err := os.ReadFile(filepath.Join(outDir, "inform-000001.bin"))
	if err != nil {
		t.Fatalf("read first capture: %v", err)
	}
	if !bytes.Equal(gotFirst, firstWire) {
		t.Fatal("first capture was overwritten after recorder restart")
	}
	gotSecond, err := os.ReadFile(filepath.Join(outDir, "inform-000002.bin"))
	if err != nil {
		t.Fatalf("read second capture: %v", err)
	}
	if !bytes.Equal(gotSecond, secondWire) {
		t.Fatal("second capture differs from second request body")
	}
}

func TestRecordersSharingDirectoryAllocateDistinctCaptures(t *testing.T) {
	outDir := t.TempDir()
	recorders := []*recorder{
		{outputDir: outDir, initialized: true},
		{outputDir: outDir, initialized: true},
	}
	models := []string{"UXGENT", "U7PRO"}

	var requests []*http.Request
	for i, model := range models {
		wire, err := (&inform.Packet{
			MAC:     [6]byte{0x00, 0x27, 0x22, 0xe0, 0x00, byte(i + 1)},
			Payload: []byte(`{"model":"` + model + `"}`),
		}).Encode(inform.DefaultKey)
		if err != nil {
			t.Fatalf("encode %s fixture: %v", model, err)
		}
		request := httptest.NewRequest(http.MethodPost, "/inform", bytes.NewReader(wire))
		request.Header.Set("Content-Type", "application/x-binary")
		requests = append(requests, request)
	}

	var wg sync.WaitGroup
	for i := range recorders {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorders[i].ServeHTTP(httptest.NewRecorder(), requests[i])
		}()
	}
	wg.Wait()

	for sequence := 1; sequence <= 2; sequence++ {
		if _, err := os.Stat(filepath.Join(outDir, fmt.Sprintf("inform-%06d.bin", sequence))); err != nil {
			t.Errorf("capture %d: %v", sequence, err)
		}
	}
}

func TestRecorderRejectsOversizedBodyWithoutCapture(t *testing.T) {
	outDir := t.TempDir()
	request := httptest.NewRequest(
		http.MethodPost,
		"/inform",
		bytes.NewReader(bytes.Repeat([]byte{0xff}, (1<<20)+1)),
	)
	response := httptest.NewRecorder()

	newRecorder(outDir).ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read capture directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("capture directory contains %d entries, want none", len(entries))
	}
}
