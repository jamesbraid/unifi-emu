package firmwarerunner

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestLoadBundleRequiresExactMetadataAndMatchingTar(t *testing.T) {
	dir := t.TempDir()
	tarData := rootFSTar(t, "arm64", "/lib/ld-linux-aarch64.so.1")
	tarPath := filepath.Join(dir, "rootfs.tar")
	metaPath := filepath.Join(dir, "rootfs.json")
	writeFile(t, tarPath, tarData, 0o644)
	writeMetadata(t, metaPath, rootFSMetadataFixture(tarData, "UXGENT"))

	bundle, err := LoadBundle(metaPath, tarPath)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Metadata.Platform != "UXGENT" || bundle.Metadata.EntryCount != 3 {
		t.Fatalf("bundle metadata = %+v", bundle.Metadata)
	}

	var fields map[string]any
	readJSON(t, metaPath, &fields)
	fields["unexpected"] = true
	writeTestJSON(t, metaPath, fields)
	if _, err := LoadBundle(metaPath, tarPath); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}

	writeMetadata(t, metaPath, rootFSMetadataFixture(tarData, "UXGENT"))
	writeFile(t, tarPath, append(tarData, 'x'), 0o644)
	if _, err := LoadBundle(metaPath, tarPath); err == nil ||
		!strings.Contains(err.Error(), "tar digest") {
		t.Fatalf("tar digest error = %v", err)
	}
}

func TestLoadProfileRejectsIncompleteAndUnsafeProfiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.json")
	writeTestJSON(t, path, map[string]any{
		"name":                  "uxgent",
		"platform":              "UXGENT",
		"firmware_digest":       strings.Repeat("a", 64),
		"firmware_version":      "1.2.3",
		"agent_digest":          strings.Repeat("b", 64),
		"architecture":          "aarch64",
		"interpreter":           "/lib/ld-linux-aarch64.so.1",
		"runner_image":          "runner:test",
		"container_name":        "firmware-uxgent",
		"startup_grace_seconds": 1,
		"overlay_dir":           "../outside",
		"command":               []string{"/usr/bin/mcad"},
	})
	if _, err := LoadProfile(path); err == nil ||
		!strings.Contains(err.Error(), "overlay_dir") {
		t.Fatalf("unsafe overlay error = %v", err)
	}

	writeTestJSON(t, path, map[string]any{
		"name": "uxgent",
	})
	if _, err := LoadProfile(path); err == nil ||
		!strings.Contains(err.Error(), "required") {
		t.Fatalf("incomplete profile error = %v", err)
	}
}

func TestPrepareBuildsDisposableRootAndDeterministicPlan(t *testing.T) {
	dir := t.TempDir()
	tarData := rootFSTar(t, "arm64", "/lib/ld-linux-aarch64.so.1")
	tarPath := filepath.Join(dir, "rootfs.tar")
	metaPath := filepath.Join(dir, "rootfs.json")
	writeFile(t, tarPath, tarData, 0o644)
	writeMetadata(t, metaPath, rootFSMetadataFixture(tarData, "UXGENT"))
	bundle, err := LoadBundle(metaPath, tarPath)
	if err != nil {
		t.Fatal(err)
	}

	profileDir := filepath.Join(dir, "profile")
	overlay := filepath.Join(profileDir, "overlay")
	writeFile(t, filepath.Join(overlay, "proc/ubnthal/system.info"),
		[]byte("systemid=ea3e\ncpuid=00000001\nboardrevision=2\nserialno=00:27:22:e0:00:01\n"), 0o644)
	profilePath := filepath.Join(profileDir, "profile.json")
	writeTestJSON(t, profilePath, map[string]any{
		"name":                  "uxgent",
		"platform":              "UXGENT",
		"firmware_digest":       strings.Repeat("a", 64),
		"firmware_version":      "1.2.3",
		"agent_digest":          syntheticAgentDigest(t, "arm64", "/lib/ld-linux-aarch64.so.1"),
		"architecture":          "aarch64",
		"interpreter":           "/lib/ld-linux-aarch64.so.1",
		"runner_image":          "runner:test",
		"container_name":        "firmware-uxgent",
		"startup_grace_seconds": 1,
		"container_mac":         "00:27:22:e0:00:01",
		"overlay_dir":           "overlay",
		"cap_add":               []string{"SYS_CHROOT"},
		"tmpfs":                 []string{"/firmware/run:rw,mode=755"},
		"command":               []string{"chroot", "/firmware", "/usr/bin/mcad"},
		"post_start": map[string]any{
			"socket":          "/firmware/tmp/.mcad",
			"timeout_seconds": 75,
			"command":         []string{"chroot", "/firmware", "/usr/bin/mca-cli-op", "set-inform", "{inform_url}"},
		},
	})
	profile, err := LoadProfile(profilePath)
	if err != nil {
		t.Fatal(err)
	}

	workDir := filepath.Join(dir, "work")
	options := Options{
		Bundle: bundle, Profile: profile, WorkDir: workDir,
		Network: "firmware-net", InformURL: "http://controller:8080/inform",
	}
	first, err := Prepare(options)
	if err != nil {
		t.Fatal(err)
	}
	systemInfo, err := os.ReadFile(filepath.Join(workDir, "rootfs/proc/ubnthal/system.info"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(systemInfo), "systemid=ea3e\ncpuid=00000001\n") {
		t.Fatalf("ordered identity was not materialized:\n%s", systemInfo)
	}
	if first.PostStart.Command[len(first.PostStart.Command)-1] != options.InformURL {
		t.Fatalf("post-start command = %#v", first.PostStart.Command)
	}
	wantArgs := []string{
		"run", "-d", "--rm", "--name", "firmware-uxgent",
		"--network", "firmware-net",
		"--mac-address", "00:27:22:e0:00:01",
		"--cap-add", "SYS_CHROOT",
		"--tmpfs", "/firmware/run:rw,mode=755",
		"--mount", "type=bind,src=" + filepath.Join(workDir, "rootfs") + ",dst=/firmware",
		"runner:test", "chroot", "/firmware", "/usr/bin/mcad",
	}
	if got := first.DockerArgs(); !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("DockerArgs() = %#v\nwant %#v", got, wantArgs)
	}

	var persisted Plan
	readJSON(t, filepath.Join(workDir, "launch-plan.json"), &persisted)
	if !reflect.DeepEqual(persisted, first) {
		t.Fatalf("persisted plan differs:\n got %+v\nwant %+v", persisted, first)
	}
	if _, err := Prepare(options); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("work directory reuse error = %v", err)
	}
}

func TestPrepareRejectsPlatformAndELFMismatch(t *testing.T) {
	dir := t.TempDir()
	tarData := rootFSTar(t, "arm64", "/lib/ld-linux-aarch64.so.1")
	tarPath := filepath.Join(dir, "rootfs.tar")
	metaPath := filepath.Join(dir, "rootfs.json")
	writeFile(t, tarPath, tarData, 0o644)
	writeMetadata(t, metaPath, rootFSMetadataFixture(tarData, "OTHER"))
	bundle, err := LoadBundle(metaPath, tarPath)
	if err != nil {
		t.Fatal(err)
	}
	profile := Profile{
		Name: "u7pro", Platform: "U7PRO", Architecture: "arm",
		FirmwareDigest: strings.Repeat("a", 64), FirmwareVersion: "1.2.3",
		AgentDigest: syntheticAgentDigest(t, "arm64", "/lib/ld-linux-aarch64.so.1"),
		Interpreter: "/lib/ld-musl-armhf.so.1", RunnerImage: "runner:test",
		ContainerName: "firmware-u7pro", OverlayDir: ".",
		Command: []string{"/usr/bin/mcad"}, baseDir: dir,
	}
	if _, err := Prepare(Options{
		Bundle: bundle, Profile: profile, WorkDir: filepath.Join(dir, "platform"),
		Network: "net",
	}); err == nil || !strings.Contains(err.Error(), "platform") {
		t.Fatalf("platform mismatch error = %v", err)
	}

	bundle.Metadata.Platform = "U7PRO"
	if _, err := Prepare(Options{
		Bundle: bundle, Profile: profile, WorkDir: filepath.Join(dir, "elf"),
		Network: "net",
	}); err == nil || !strings.Contains(err.Error(), "ELF machine") {
		t.Fatalf("ELF mismatch error = %v", err)
	}
}

func TestPrepareRejectsTarPathTraversal(t *testing.T) {
	dir := t.TempDir()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	tarData := archive.Bytes()
	tarPath := filepath.Join(dir, "rootfs.tar")
	metaPath := filepath.Join(dir, "rootfs.json")
	writeFile(t, tarPath, tarData, 0o644)
	meta := rootFSMetadataFixture(tarData, "UXGENT")
	meta.EntryCount = 1
	writeMetadata(t, metaPath, meta)
	bundle, err := LoadBundle(metaPath, tarPath)
	if err != nil {
		t.Fatal(err)
	}
	profile := Profile{
		Name: "uxgent", Platform: "UXGENT", Architecture: "aarch64",
		FirmwareDigest: strings.Repeat("a", 64), FirmwareVersion: "1.2.3",
		AgentDigest: syntheticAgentDigest(t, "arm64", "/lib/ld-linux-aarch64.so.1"),
		Interpreter: "/lib/ld-linux-aarch64.so.1", RunnerImage: "runner:test",
		ContainerName: "firmware-uxgent", OverlayDir: ".",
		Command: []string{"/usr/bin/mcad"}, baseDir: dir,
	}
	if _, err := Prepare(Options{
		Bundle: bundle, Profile: profile, WorkDir: filepath.Join(dir, "work"),
		Network: "net",
	}); err == nil || !strings.Contains(err.Error(), "unsafe tar path") {
		t.Fatalf("path traversal error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape")); !os.IsNotExist(err) {
		t.Fatalf("archive escaped work directory: %v", err)
	}
}

func TestPrepareRejectsProfileProvenanceMismatch(t *testing.T) {
	metadata := rootFSMetadataFixture([]byte("tar"), "UXGENT")
	profile := Profile{
		Platform: "UXGENT", FirmwareDigest: strings.Repeat("b", 64),
		FirmwareVersion: "9.9.9",
	}
	for name, mutate := range map[string]func(*Profile){
		"digest":  func(p *Profile) { p.FirmwareVersion = metadata.Version },
		"version": func(p *Profile) { p.FirmwareDigest = metadata.SourceFirmwareDigest },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := profile
			mutate(&candidate)
			_, err := Prepare(Options{
				Bundle: Bundle{Metadata: metadata}, Profile: candidate,
				WorkDir: filepath.Join(t.TempDir(), "work"), Network: "net",
			})
			if err == nil || !strings.Contains(err.Error(), "firmware "+name) {
				t.Fatalf("provenance mismatch error = %v", err)
			}
		})
	}
}

func TestPrepareRechecksTarDigestAtExtraction(t *testing.T) {
	dir := t.TempDir()
	first := rootFSTar(t, "arm64", "/lib/ld-linux-aarch64.so.1")
	second := rootFSTar(t, "arm", "/lib/ld-musl-armhf.so.1")
	tarPath := filepath.Join(dir, "rootfs.tar")
	metaPath := filepath.Join(dir, "rootfs.json")
	writeFile(t, tarPath, first, 0o644)
	writeMetadata(t, metaPath, rootFSMetadataFixture(first, "UXGENT"))
	bundle, err := LoadBundle(metaPath, tarPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, tarPath, second, 0o644)
	profile := Profile{
		Name: "uxgent", Platform: "UXGENT",
		FirmwareDigest: strings.Repeat("a", 64), FirmwareVersion: "1.2.3",
		AgentDigest:  syntheticAgentDigest(t, "arm64", "/lib/ld-linux-aarch64.so.1"),
		Architecture: "aarch64", Interpreter: "/lib/ld-linux-aarch64.so.1",
		RunnerImage: "runner:test", ContainerName: "firmware-uxgent",
		OverlayDir: ".", Command: []string{"/usr/bin/mcad"}, baseDir: dir,
	}
	work := filepath.Join(dir, "work")
	_, err = Prepare(Options{
		Bundle: bundle, Profile: profile, WorkDir: work, Network: "net",
	})
	if err == nil || !strings.Contains(err.Error(), "tar digest") {
		t.Fatalf("replacement tar error = %v", err)
	}
	if _, statErr := os.Stat(work); !os.IsNotExist(statErr) {
		t.Fatalf("failed preparation left work directory: %v", statErr)
	}
}

func TestExtractRootFSAcceptsExtractorLinkAndSpecialEntryTypes(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	headers := []*tar.Header{
		{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "etc/alternatives/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "etc/alternatives/write", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "usr/bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "usr/bin/write", Typeflag: tar.TypeSymlink, Linkname: "/etc/alternatives/write"},
		{Name: "dev/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "dev/console", Typeflag: tar.TypeChar, Mode: 0o600, Devmajor: 5, Devminor: 1},
		{Name: "dev/loop0", Typeflag: tar.TypeBlock, Mode: 0o600, Devmajor: 7},
		{Name: "run/agent.fifo", Typeflag: tar.TypeFifo, Mode: 0o600},
	}
	for _, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rootfs.tar")
	writeFile(t, path, archive.Bytes(), 0o644)
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	count, err := extractRootFS(path, root, fmt.Sprintf("%x", sha256.Sum256(archive.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	if count != len(headers) {
		t.Fatalf("entry count = %d, want %d", count, len(headers))
	}
	link, err := os.Readlink(filepath.Join(root, "usr/bin/write"))
	if err != nil || link != "/etc/alternatives/write" {
		t.Fatalf("absolute symlink = %q, error %v", link, err)
	}
	for _, path := range []string{"dev/console", "dev/loop0"} {
		if _, err := os.Lstat(filepath.Join(root, path)); err != nil {
			t.Errorf("special entry %s was not materialized: %v", path, err)
		}
	}
	info, err := os.Lstat(filepath.Join(root, "run/agent.fifo"))
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("fifo mode = %v, error %v", info, err)
	}
}

func TestExtractRootFSPreservesSpecialModesDespiteUmask(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	stickyHeader := &tar.Header{
		Name: "sticky/", Typeflag: tar.TypeDir, Mode: 0o1777,
	}
	if err := writer.WriteHeader(stickyHeader); err != nil {
		t.Fatal(err)
	}
	fileHeaders := []*tar.Header{
		{Name: "sticky/setuid", Typeflag: tar.TypeReg, Mode: 0o4755, Size: 1},
		{Name: "sticky/setgid", Typeflag: tar.TypeReg, Mode: 0o2750, Size: 1},
	}
	for _, header := range fileHeaders {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rootfs.tar")
	writeFile(t, path, archive.Bytes(), 0o644)
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	oldUmask := syscall.Umask(0o077)
	defer syscall.Umask(oldUmask)
	if _, err := extractRootFS(
		path, root, fmt.Sprintf("%x", sha256.Sum256(archive.Bytes())),
	); err != nil {
		t.Fatal(err)
	}
	for header, want := range map[*tar.Header]os.FileMode{
		stickyHeader:   os.ModeSticky | 0o777,
		fileHeaders[0]: os.ModeSetuid | 0o755,
		fileHeaders[1]: os.ModeSetgid | 0o750,
	} {
		if got := archiveFileMode(header); got != want {
			t.Errorf("archive mode for %s = %v, want %v", header.Name, got, want)
		}
	}
	for name, want := range map[string]os.FileMode{
		"sticky":        os.ModeDir | os.ModeSticky | 0o777,
		"sticky/setuid": 0o755,
		"sticky/setgid": 0o750,
	} {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil {
			t.Errorf("stat %s: %v", name, err)
			continue
		}
		got := info.Mode() & (os.ModeType | os.ModeSticky | os.ModePerm)
		if got != want {
			t.Errorf("%s mode = %v, want %v", name, got, want)
		}
	}
}

func TestExtractRootFSHardlinkKeepsCanonicalTargetMode(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name: "bin/tool", Typeflag: tar.TypeReg, Mode: 0o755, Size: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{
		Name: "bin/tool-link", Typeflag: tar.TypeLink,
		Linkname: "bin/tool", Mode: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "rootfs.tar")
	writeFile(t, path, archive.Bytes(), 0o644)
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := extractRootFS(
		path, root, fmt.Sprintf("%x", sha256.Sum256(archive.Bytes())),
	); err != nil {
		t.Fatal(err)
	}
	target, err := os.Stat(filepath.Join(root, "bin/tool"))
	if err != nil {
		t.Fatal(err)
	}
	link, err := os.Stat(filepath.Join(root, "bin/tool-link"))
	if err != nil {
		t.Fatal(err)
	}
	if target.Mode().Perm() != 0o755 || link.Mode().Perm() != 0o755 {
		t.Fatalf("hardlink modes target=%v link=%v, want 0755", target.Mode(), link.Mode())
	}
	if !os.SameFile(target, link) {
		t.Fatal("hardlink does not share the target inode")
	}
}

func TestCopyOverlayResolvesAbsoluteVendorSymlinkInsideDisposableRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	overlay := filepath.Join(dir, "overlay")
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "var"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/tmp", filepath.Join(root, "var/run")); err != nil {
		t.Fatal(err)
	}
	name := "firmware-runner-" + filepath.Base(dir)
	writeFile(t, filepath.Join(overlay, "var/run", name), []byte("marker"), 0o644)
	hostEscape := filepath.Join("/tmp", name)
	t.Cleanup(func() { _ = os.Remove(hostEscape) })

	if err := copyOverlay(overlay, root); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, "tmp", name))
	if err != nil {
		t.Fatalf("overlay did not follow chroot-relative vendor symlink: %v", err)
	}
	if string(body) != "marker" {
		t.Fatalf("resolved overlay = %q", body)
	}
	if _, err := os.Stat(hostEscape); !os.IsNotExist(err) {
		t.Fatalf("overlay escaped disposable root through absolute symlink: %v", err)
	}
}

func rootFSMetadataFixture(tarData []byte, platform string) RootFSMetadata {
	return RootFSMetadata{
		SourceFirmwareDigest: strings.Repeat("a", 64),
		Platform:             platform,
		Version:              "1.2.3",
		NestedArtifactPath:   "firmware.bin/rootfs",
		TarDigest:            fmt.Sprintf("%x", sha256.Sum256(tarData)),
		EntryCount:           3,
	}
}

func rootFSTar(t *testing.T, architecture, interpreter string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, name := range []string{"usr/", "usr/bin/"} {
		if err := writer.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeDir, Mode: 0o755,
		}); err != nil {
			t.Fatal(err)
		}
	}
	agent := syntheticELF(t, architecture, interpreter)
	if err := writer.WriteHeader(&tar.Header{
		Name: "usr/bin/mcad", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(agent)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(agent); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func syntheticELF(t *testing.T, architecture, interpreter string) []byte {
	t.Helper()
	interp := append([]byte(interpreter), 0)
	switch architecture {
	case "arm":
		data := make([]byte, 52+32+len(interp))
		copy(data[:16], []byte{0x7f, 'E', 'L', 'F', 1, 1, 1})
		binary.LittleEndian.PutUint16(data[16:], 3)
		binary.LittleEndian.PutUint16(data[18:], 40)
		binary.LittleEndian.PutUint32(data[20:], 1)
		binary.LittleEndian.PutUint32(data[28:], 52)
		binary.LittleEndian.PutUint16(data[40:], 52)
		binary.LittleEndian.PutUint16(data[42:], 32)
		binary.LittleEndian.PutUint16(data[44:], 1)
		binary.LittleEndian.PutUint32(data[52:], 3)
		binary.LittleEndian.PutUint32(data[56:], 84)
		binary.LittleEndian.PutUint32(data[68:], uint32(len(interp)))
		copy(data[84:], interp)
		return data
	case "arm64":
		data := make([]byte, 64+56+len(interp))
		copy(data[:16], []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
		binary.LittleEndian.PutUint16(data[16:], 3)
		binary.LittleEndian.PutUint16(data[18:], 183)
		binary.LittleEndian.PutUint32(data[20:], 1)
		binary.LittleEndian.PutUint64(data[32:], 64)
		binary.LittleEndian.PutUint16(data[52:], 64)
		binary.LittleEndian.PutUint16(data[54:], 56)
		binary.LittleEndian.PutUint16(data[56:], 1)
		binary.LittleEndian.PutUint32(data[64:], 3)
		binary.LittleEndian.PutUint64(data[72:], 120)
		binary.LittleEndian.PutUint64(data[96:], uint64(len(interp)))
		copy(data[120:], interp)
		return data
	default:
		t.Fatalf("unknown synthetic ELF architecture %q", architecture)
		return nil
	}
}

func syntheticAgentDigest(t *testing.T, architecture, interpreter string) string {
	t.Helper()
	return fmt.Sprintf("%x", sha256.Sum256(syntheticELF(t, architecture, interpreter)))
}

func writeMetadata(t *testing.T, path string, metadata RootFSMetadata) {
	t.Helper()
	writeTestJSON(t, path, metadata)
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	writeFile(t, path, body, 0o644)
}

func readJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, value); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
}
