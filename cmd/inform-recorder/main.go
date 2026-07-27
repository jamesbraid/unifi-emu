package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/jamesbraid/unifi-emu/inform"
)

const maxInformPacketBytes = 1 << 20

type captureMetadata struct {
	MAC         string `json:"mac"`
	ContentType string `json:"content_type"`
}

type recorder struct {
	outputDir   string
	mu          sync.Mutex
	sequence    uint64
	initialized bool
}

func newRecorder(outputDir string) http.Handler {
	return &recorder{outputDir: outputDir}
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || req.URL.Path != "/inform" {
		http.NotFound(w, req)
		return
	}

	req.Body = http.MaxBytesReader(w, req.Body, maxInformPacketBytes)
	wire, err := io.ReadAll(req.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			http.Error(w, "inform body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read inform body", http.StatusBadRequest)
		return
	}
	packet, err := inform.Decode(wire, inform.DefaultKey)
	if err != nil {
		http.Error(w, "decode inform packet", http.StatusBadRequest)
		return
	}

	metadata, err := json.MarshalIndent(captureMetadata{
		MAC:         fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", packet.MAC[0], packet.MAC[1], packet.MAC[2], packet.MAC[3], packet.MAC[4], packet.MAC[5]),
		ContentType: req.Header.Get("Content-Type"),
	}, "", "  ")
	if err != nil {
		http.Error(w, "encode capture metadata", http.StatusInternalServerError)
		return
	}
	metadata = append(metadata, '\n')

	r.mu.Lock()
	defer r.mu.Unlock()

	if err := os.MkdirAll(r.outputDir, 0o755); err != nil {
		http.Error(w, "create capture directory", http.StatusInternalServerError)
		return
	}
	captures := []struct {
		suffix string
		data   []byte
	}{
		{suffix: ".bin", data: wire},
		{suffix: ".json", data: packet.Payload},
		{suffix: ".meta.json", data: metadata},
	}
	if err := r.persist(captures); err != nil {
		http.Error(w, "persist inform capture", http.StatusInternalServerError)
		return
	}

	http.NotFound(w, req)
}

func (r *recorder) persist(captures []struct {
	suffix string
	data   []byte
}) error {
	if !r.initialized {
		entries, err := os.ReadDir(r.outputDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := strings.TrimPrefix(entry.Name(), "inform-")
			if name == entry.Name() {
				continue
			}
			dot := strings.IndexByte(name, '.')
			if dot < 1 {
				continue
			}
			sequence, err := strconv.ParseUint(name[:dot], 10, 64)
			if err == nil && sequence > r.sequence {
				r.sequence = sequence
			}
		}
		r.initialized = true
	}

	for {
		r.sequence++
		base := filepath.Join(r.outputDir, fmt.Sprintf("inform-%06d", r.sequence))
		var created []string
		collision := false
		for _, capture := range captures {
			path := base + capture.suffix
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if os.IsExist(err) {
				collision = true
				break
			}
			if err != nil {
				removeFiles(created)
				return err
			}
			created = append(created, path)
			if _, err := file.Write(capture.data); err != nil {
				file.Close()
				removeFiles(created)
				return err
			}
			if err := file.Close(); err != nil {
				removeFiles(created)
				return err
			}
		}
		if collision {
			removeFiles(created)
			continue
		}
		return nil
	}
}

func removeFiles(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func main() {
	listen := flag.String("listen", ":8080", "address to listen on")
	output := flag.String("output", "captures", "directory for inform captures")
	flag.Parse()

	log.Printf("recording inform packets on %s in %s", *listen, *output)
	if err := http.ListenAndServe(*listen, newRecorder(*output)); err != nil {
		log.Fatal(err)
	}
}
