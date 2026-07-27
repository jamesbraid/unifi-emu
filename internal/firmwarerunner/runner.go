package firmwarerunner

import (
	"archive/tar"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type RootFSMetadata struct {
	SourceFirmwareDigest string `json:"source_firmware_digest"`
	Platform             string `json:"platform"`
	Version              string `json:"version"`
	NestedArtifactPath   string `json:"nested_artifact_path"`
	TarDigest            string `json:"tar_digest"`
	EntryCount           int    `json:"entry_count"`
}

type Bundle struct {
	Metadata     RootFSMetadata
	MetadataPath string
	TarPath      string
}

type PostStart struct {
	Socket         string   `json:"socket"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	Command        []string `json:"command"`
}

type Profile struct {
	Name                string     `json:"name"`
	Platform            string     `json:"platform"`
	FirmwareDigest      string     `json:"firmware_digest"`
	FirmwareVersion     string     `json:"firmware_version"`
	AgentDigest         string     `json:"agent_digest"`
	Architecture        string     `json:"architecture"`
	Interpreter         string     `json:"interpreter"`
	RunnerImage         string     `json:"runner_image"`
	ContainerName       string     `json:"container_name"`
	StartupGraceSeconds int        `json:"startup_grace_seconds"`
	ContainerMAC        string     `json:"container_mac,omitempty"`
	OverlayDir          string     `json:"overlay_dir"`
	CapAdd              []string   `json:"cap_add,omitempty"`
	Tmpfs               []string   `json:"tmpfs,omitempty"`
	Command             []string   `json:"command"`
	PostStart           *PostStart `json:"post_start,omitempty"`

	baseDir string
}

type Options struct {
	Bundle    Bundle
	Profile   Profile
	WorkDir   string
	Network   string
	InformURL string
}

type Plan struct {
	Profile              string     `json:"profile"`
	Platform             string     `json:"platform"`
	Version              string     `json:"version"`
	SourceFirmwareDigest string     `json:"source_firmware_digest"`
	TarDigest            string     `json:"tar_digest"`
	RootFS               string     `json:"rootfs"`
	ContainerName        string     `json:"container_name"`
	StartupGraceSeconds  int        `json:"startup_grace_seconds"`
	RunnerImage          string     `json:"runner_image"`
	Network              string     `json:"network"`
	ContainerMAC         string     `json:"container_mac,omitempty"`
	CapAdd               []string   `json:"cap_add,omitempty"`
	Tmpfs                []string   `json:"tmpfs,omitempty"`
	Command              []string   `json:"command"`
	PostStart            *PostStart `json:"post_start,omitempty"`
}

func LoadBundle(metadataPath, tarPath string) (Bundle, error) {
	if metadataPath == "" {
		return Bundle{}, errors.New("rootfs metadata path is required")
	}
	if tarPath == "" {
		return Bundle{}, errors.New("rootfs tar path is required")
	}
	file, err := os.Open(metadataPath)
	if err != nil {
		return Bundle{}, fmt.Errorf("open rootfs metadata: %w", err)
	}
	defer file.Close()
	var metadata RootFSMetadata
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return Bundle{}, fmt.Errorf("decode rootfs metadata: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Bundle{}, fmt.Errorf("decode rootfs metadata: %w", err)
	}
	if err := validateMetadata(metadata); err != nil {
		return Bundle{}, err
	}
	got, err := fileSHA256(tarPath)
	if err != nil {
		return Bundle{}, fmt.Errorf("hash rootfs tar: %w", err)
	}
	if got != metadata.TarDigest {
		return Bundle{}, fmt.Errorf("rootfs tar digest = %s, want %s", got, metadata.TarDigest)
	}
	absoluteMetadata, err := filepath.Abs(metadataPath)
	if err != nil {
		return Bundle{}, fmt.Errorf("resolve rootfs metadata path: %w", err)
	}
	absoluteTar, err := filepath.Abs(tarPath)
	if err != nil {
		return Bundle{}, fmt.Errorf("resolve rootfs tar path: %w", err)
	}
	return Bundle{
		Metadata: metadata, MetadataPath: absoluteMetadata, TarPath: absoluteTar,
	}, nil
}

func LoadProfile(path string) (Profile, error) {
	file, err := os.Open(path)
	if err != nil {
		return Profile{}, fmt.Errorf("open runner profile: %w", err)
	}
	defer file.Close()
	var profile Profile
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profile); err != nil {
		return Profile{}, fmt.Errorf("decode runner profile: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Profile{}, fmt.Errorf("decode runner profile: %w", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Profile{}, fmt.Errorf("resolve runner profile path: %w", err)
	}
	profile.baseDir = filepath.Dir(absolute)
	if err := validateProfile(profile); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func Prepare(options Options) (plan Plan, err error) {
	if options.Bundle.Metadata.Platform != options.Profile.Platform {
		return Plan{}, fmt.Errorf(
			"rootfs platform %q does not match profile platform %q",
			options.Bundle.Metadata.Platform, options.Profile.Platform,
		)
	}
	if options.Bundle.Metadata.SourceFirmwareDigest != options.Profile.FirmwareDigest {
		return Plan{}, fmt.Errorf(
			"rootfs firmware digest %q does not match profile digest %q",
			options.Bundle.Metadata.SourceFirmwareDigest, options.Profile.FirmwareDigest,
		)
	}
	if options.Bundle.Metadata.Version != options.Profile.FirmwareVersion {
		return Plan{}, fmt.Errorf(
			"rootfs firmware version %q does not match profile version %q",
			options.Bundle.Metadata.Version, options.Profile.FirmwareVersion,
		)
	}
	if options.WorkDir == "" {
		return Plan{}, errors.New("work directory is required")
	}
	if options.Network == "" {
		return Plan{}, errors.New("Docker network is required")
	}
	absoluteWork, err := filepath.Abs(options.WorkDir)
	if err != nil {
		return Plan{}, fmt.Errorf("resolve work directory: %w", err)
	}
	if _, statErr := os.Stat(absoluteWork); statErr == nil {
		return Plan{}, fmt.Errorf("work directory %q already exists", absoluteWork)
	} else if !os.IsNotExist(statErr) {
		return Plan{}, fmt.Errorf("inspect work directory: %w", statErr)
	}
	root := filepath.Join(absoluteWork, "rootfs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Plan{}, fmt.Errorf("create disposable rootfs: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(absoluteWork)
		}
	}()
	count, err := extractRootFS(
		options.Bundle.TarPath, root, options.Bundle.Metadata.TarDigest,
	)
	if err != nil {
		return Plan{}, err
	}
	if count != options.Bundle.Metadata.EntryCount {
		return Plan{}, fmt.Errorf(
			"rootfs entry count = %d, metadata says %d",
			count, options.Bundle.Metadata.EntryCount,
		)
	}
	if err := validateAgent(root, options.Profile); err != nil {
		return Plan{}, err
	}
	overlay := filepath.Join(options.Profile.baseDir, options.Profile.OverlayDir)
	if err := copyOverlay(overlay, root); err != nil {
		return Plan{}, fmt.Errorf("apply profile overlay: %w", err)
	}

	plan = Plan{
		Profile:              options.Profile.Name,
		Platform:             options.Bundle.Metadata.Platform,
		Version:              options.Bundle.Metadata.Version,
		SourceFirmwareDigest: options.Bundle.Metadata.SourceFirmwareDigest,
		TarDigest:            options.Bundle.Metadata.TarDigest,
		RootFS:               root,
		ContainerName:        options.Profile.ContainerName,
		StartupGraceSeconds:  options.Profile.StartupGraceSeconds,
		RunnerImage:          options.Profile.RunnerImage,
		Network:              options.Network,
		ContainerMAC:         options.Profile.ContainerMAC,
		CapAdd:               append([]string(nil), options.Profile.CapAdd...),
		Tmpfs:                append([]string(nil), options.Profile.Tmpfs...),
		Command:              expandArgs(options.Profile.Command, options.InformURL),
		PostStart:            expandPostStart(options.Profile.PostStart, options.InformURL),
	}
	if err := writeJSON(filepath.Join(absoluteWork, "launch-plan.json"), plan); err != nil {
		return Plan{}, fmt.Errorf("write launch plan: %w", err)
	}
	complete = true
	return plan, nil
}

func (p Plan) DockerArgs() []string {
	args := []string{
		"run", "-d", "--rm", "--name", p.ContainerName,
		"--network", p.Network,
	}
	if p.ContainerMAC != "" {
		args = append(args, "--mac-address", p.ContainerMAC)
	}
	for _, capability := range p.CapAdd {
		args = append(args, "--cap-add", capability)
	}
	for _, mount := range p.Tmpfs {
		args = append(args, "--tmpfs", mount)
	}
	args = append(args,
		"--mount", "type=bind,src="+p.RootFS+",dst=/firmware",
		p.RunnerImage,
	)
	return append(args, p.Command...)
}

func validateMetadata(metadata RootFSMetadata) error {
	for name, value := range map[string]string{
		"source_firmware_digest": metadata.SourceFirmwareDigest,
		"platform":               metadata.Platform,
		"version":                metadata.Version,
		"nested_artifact_path":   metadata.NestedArtifactPath,
		"tar_digest":             metadata.TarDigest,
	} {
		if value == "" {
			return fmt.Errorf("rootfs metadata %s is required", name)
		}
	}
	if !validSHA256(metadata.SourceFirmwareDigest) {
		return errors.New("rootfs metadata source_firmware_digest must be 64 lowercase hex characters")
	}
	if !validSHA256(metadata.TarDigest) {
		return errors.New("rootfs metadata tar_digest must be 64 lowercase hex characters")
	}
	if metadata.EntryCount <= 0 {
		return errors.New("rootfs metadata entry_count must be positive")
	}
	return nil
}

func validateProfile(profile Profile) error {
	for name, value := range map[string]string{
		"name":             profile.Name,
		"platform":         profile.Platform,
		"firmware_digest":  profile.FirmwareDigest,
		"firmware_version": profile.FirmwareVersion,
		"agent_digest":     profile.AgentDigest,
		"architecture":     profile.Architecture,
		"interpreter":      profile.Interpreter,
		"runner_image":     profile.RunnerImage,
		"container_name":   profile.ContainerName,
		"overlay_dir":      profile.OverlayDir,
	} {
		if value == "" {
			return fmt.Errorf("runner profile %s is required", name)
		}
	}
	if !validSHA256(profile.FirmwareDigest) {
		return errors.New("runner profile firmware_digest must be 64 lowercase hex characters")
	}
	if !validSHA256(profile.AgentDigest) {
		return errors.New("runner profile agent_digest must be 64 lowercase hex characters")
	}
	if profile.StartupGraceSeconds <= 0 {
		return errors.New("runner profile startup_grace_seconds must be positive")
	}
	if _, ok := profileMachine(profile.Architecture); !ok {
		return fmt.Errorf("runner profile architecture %q is unsupported", profile.Architecture)
	}
	if !filepath.IsAbs(profile.Interpreter) {
		return errors.New("runner profile interpreter must be absolute")
	}
	cleanOverlay := filepath.Clean(profile.OverlayDir)
	if filepath.IsAbs(cleanOverlay) || cleanOverlay == ".." ||
		strings.HasPrefix(cleanOverlay, ".."+string(filepath.Separator)) {
		return fmt.Errorf("runner profile overlay_dir %q escapes the profile directory", profile.OverlayDir)
	}
	if len(profile.Command) == 0 {
		return errors.New("runner profile command is required")
	}
	if profile.ContainerMAC != "" {
		if _, err := net.ParseMAC(profile.ContainerMAC); err != nil {
			return fmt.Errorf("runner profile container_mac: %w", err)
		}
	}
	if profile.PostStart != nil {
		if profile.PostStart.Socket == "" || profile.PostStart.TimeoutSeconds <= 0 ||
			len(profile.PostStart.Command) == 0 {
			return errors.New("runner profile post_start requires socket, positive timeout_seconds, and command")
		}
	}
	return nil
}

func validateAgent(root string, profile Profile) error {
	path := filepath.Join(root, "usr/bin/mcad")
	digest, err := fileSHA256(path)
	if err != nil {
		return fmt.Errorf("hash mcad: %w", err)
	}
	if digest != profile.AgentDigest {
		return fmt.Errorf("mcad digest = %s, profile expects %s", digest, profile.AgentDigest)
	}
	file, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("open mcad ELF: %w", err)
	}
	defer file.Close()
	wantMachine, _ := profileMachine(profile.Architecture)
	if file.Machine != wantMachine {
		return fmt.Errorf("mcad ELF machine = %s, profile expects %s", file.Machine, wantMachine)
	}
	var interpreter string
	for _, program := range file.Progs {
		if program.Type != elf.PT_INTERP {
			continue
		}
		body, err := io.ReadAll(program.Open())
		if err != nil {
			return fmt.Errorf("read mcad interpreter: %w", err)
		}
		interpreter = strings.TrimRight(string(body), "\x00")
		break
	}
	if interpreter != profile.Interpreter {
		return fmt.Errorf("mcad interpreter = %q, profile expects %q", interpreter, profile.Interpreter)
	}
	return nil
}

func profileMachine(architecture string) (elf.Machine, bool) {
	switch architecture {
	case "arm":
		return elf.EM_ARM, true
	case "aarch64":
		return elf.EM_AARCH64, true
	default:
		return elf.EM_NONE, false
	}
}

func extractRootFS(tarPath, root, expectedDigest string) (int, error) {
	file, err := os.Open(tarPath)
	if err != nil {
		return 0, fmt.Errorf("open rootfs tar: %w", err)
	}
	defer file.Close()
	hasher := sha256.New()
	stream := io.TeeReader(file, hasher)
	reader := tar.NewReader(stream)
	seen := map[string]struct{}{}
	count := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if _, err := io.Copy(io.Discard, stream); err != nil {
				return 0, fmt.Errorf("finish hashing rootfs tar: %w", err)
			}
			got := hex.EncodeToString(hasher.Sum(nil))
			if got != expectedDigest {
				return 0, fmt.Errorf("rootfs tar digest = %s, want %s", got, expectedDigest)
			}
			return count, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read rootfs tar: %w", err)
		}
		name, err := safeArchivePath(header.Name)
		if err != nil {
			return 0, err
		}
		if _, ok := seen[name]; ok {
			return 0, fmt.Errorf("duplicate rootfs tar path %q", name)
		}
		target, err := resolveRootPath(root, filepath.FromSlash(name))
		if err != nil {
			return 0, fmt.Errorf("resolve rootfs tar path %q: %w", name, err)
		}
		mode := archiveFileMode(header)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return 0, fmt.Errorf("create rootfs directory %q: %w", name, err)
			}
			if err := os.Chmod(target, mode); err != nil {
				return 0, fmt.Errorf("set rootfs directory mode %q: %w", name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return 0, fmt.Errorf("create rootfs parent %q: %w", name, err)
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return 0, fmt.Errorf("create rootfs file %q: %w", name, err)
			}
			_, copyErr := io.Copy(output, reader)
			closeErr := output.Close()
			if copyErr != nil {
				return 0, fmt.Errorf("write rootfs file %q: %w", name, copyErr)
			}
			if closeErr != nil {
				return 0, fmt.Errorf("close rootfs file %q: %w", name, closeErr)
			}
			if err := os.Chmod(target, mode); err != nil {
				return 0, fmt.Errorf("set rootfs file mode %q: %w", name, err)
			}
		case tar.TypeSymlink:
			if err := safeLinkTarget(name, header.Linkname); err != nil {
				return 0, err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return 0, fmt.Errorf("create rootfs symlink parent %q: %w", name, err)
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return 0, fmt.Errorf("create rootfs symlink %q: %w", name, err)
			}
		case tar.TypeLink:
			linkname, err := safeArchivePath(header.Linkname)
			if err != nil {
				return 0, fmt.Errorf("unsafe hardlink target for %q: %w", name, err)
			}
			if _, ok := seen[linkname]; !ok {
				return 0, fmt.Errorf("hardlink %q precedes target %q", name, linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return 0, fmt.Errorf("create rootfs hardlink parent %q: %w", name, err)
			}
			linkTarget, err := resolveRootPath(root, filepath.FromSlash(linkname))
			if err != nil {
				return 0, fmt.Errorf("resolve hardlink target for %q: %w", name, err)
			}
			if err := os.Link(linkTarget, target); err != nil {
				return 0, fmt.Errorf("create rootfs hardlink %q: %w", name, err)
			}
		case tar.TypeFifo:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return 0, fmt.Errorf("create rootfs fifo parent %q: %w", name, err)
			}
			if err := syscall.Mkfifo(target, 0o600); err != nil {
				return 0, fmt.Errorf("create rootfs fifo %q: %w", name, err)
			}
			if err := os.Chmod(target, mode); err != nil {
				return 0, fmt.Errorf("set rootfs fifo mode %q: %w", name, err)
			}
		case tar.TypeChar, tar.TypeBlock:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return 0, fmt.Errorf("create rootfs device parent %q: %w", name, err)
			}
			// Device creation is privileged and bind-mounted device nodes are not
			// portable across hosts. Preserve the path as a placeholder; profiles
			// provide any device semantics required by the target agent.
			output, err := os.OpenFile(
				target, os.O_CREATE|os.O_EXCL|os.O_WRONLY,
				0o600,
			)
			if err != nil {
				return 0, fmt.Errorf("create rootfs device placeholder %q: %w", name, err)
			}
			if err := output.Close(); err != nil {
				return 0, fmt.Errorf("close rootfs device placeholder %q: %w", name, err)
			}
			if err := os.Chmod(target, mode); err != nil {
				return 0, fmt.Errorf("set rootfs device placeholder mode %q: %w", name, err)
			}
		default:
			return 0, fmt.Errorf("unsupported rootfs tar type %d for %q", header.Typeflag, name)
		}
		seen[name] = struct{}{}
		count++
	}
}

func archiveFileMode(header *tar.Header) fs.FileMode {
	return header.FileInfo().Mode() &
		(fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}

func safeArchivePath(name string) (string, error) {
	name = strings.TrimSuffix(name, "/")
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	if name == "" || clean == "." || filepath.IsAbs(name) || clean == ".." ||
		strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	return clean, nil
}

func safeLinkTarget(name, target string) error {
	if target == "" {
		return fmt.Errorf("unsafe symlink target %q for %q", target, name)
	}
	if filepath.IsAbs(target) {
		return nil
	}
	resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(name), filepath.FromSlash(target))))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("unsafe symlink target %q for %q", target, name)
	}
	return nil
}

func copyOverlay(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", source)
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		destination, err := resolveRootPath(target, relative)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(destination, info.Mode().Perm())
		case entry.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return err
			}
			_ = os.Remove(destination)
			return os.Symlink(link, destination)
		case entry.Type().IsRegular():
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return err
			}
			return copyFile(path, destination, info.Mode().Perm())
		default:
			return fmt.Errorf("unsupported overlay entry %q", path)
		}
	})
}

func resolveRootPath(root, relative string) (string, error) {
	clean := filepath.Clean(relative)
	if filepath.IsAbs(clean) || clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("overlay path %q escapes disposable root", relative)
	}
	pending := strings.FieldsFunc(filepath.ToSlash(clean), func(r rune) bool { return r == '/' })
	resolved := make([]string, 0, len(pending))
	for traversals := 0; len(pending) > 0; {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return "", fmt.Errorf("overlay path %q escapes disposable root", relative)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidate := filepath.Join(append([]string{root}, append(resolved, part)...)...)
		info, err := os.Lstat(candidate)
		if os.IsNotExist(err) {
			resolved = append(resolved, part)
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect overlay target %q: %w", candidate, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = append(resolved, part)
			continue
		}
		traversals++
		if traversals > 64 {
			return "", fmt.Errorf("too many symlinks while resolving overlay path %q", relative)
		}
		link, err := os.Readlink(candidate)
		if err != nil {
			return "", fmt.Errorf("read overlay target symlink %q: %w", candidate, err)
		}
		linkParts := strings.FieldsFunc(filepath.ToSlash(link), func(r rune) bool { return r == '/' })
		if filepath.IsAbs(link) {
			resolved = resolved[:0]
		}
		pending = append(linkParts, pending...)
	}
	return filepath.Join(append([]string{root}, resolved...)...), nil
}

func copyFile(source, target string, mode fs.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func expandArgs(args []string, informURL string) []string {
	result := make([]string, len(args))
	for i, arg := range args {
		result[i] = strings.ReplaceAll(arg, "{inform_url}", informURL)
	}
	return result
}

func expandPostStart(post *PostStart, informURL string) *PostStart {
	if post == nil {
		return nil
	}
	return &PostStart{
		Socket: post.Socket, TimeoutSeconds: post.TimeoutSeconds,
		Command: expandArgs(post.Command, informURL),
	}
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(path, body, 0o644)
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values")
}
