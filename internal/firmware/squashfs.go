package firmware

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/KarpelesLab/squashfs"
	anchorelzo "github.com/anchore/go-lzo"
	"github.com/klauspost/compress/zstd"
	squashfsxz "github.com/mikelolasagasti/xz"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz/lzma"
)

const (
	maxSquashFSBlock      = 1 << 20
	maxSquashFSDictionary = 64 << 20
)

func init() {
	// SquashFS stores LZ4 as raw blocks, not LZ4 frames. KarpelesLab exposes
	// compression registration specifically so the maintained codec libraries
	// can be used without another filesystem or compression implementation.
	squashfs.RegisterCompHandler(squashfs.LZ4, &squashfs.CompHandler{
		Decompress: decompressSquashFSLZ4,
		Compress:   compressSquashFSLZ4,
	})
	squashfs.RegisterCompHandler(squashfs.LZO, &squashfs.CompHandler{
		Decompress: func(src []byte) ([]byte, error) {
			dst := make([]byte, maxSquashFSBlock)
			n, err := anchorelzo.Decompress(src, dst)
			if err != nil {
				return nil, err
			}
			return dst[:n], nil
		},
	})
	squashfs.RegisterCompHandler(squashfs.LZMA, &squashfs.CompHandler{
		Decompress: func(src []byte) ([]byte, error) {
			reader, err := (lzma.ReaderConfig{DictCap: maxSquashFSDictionary}).
				NewReader(bytes.NewReader(src))
			if err != nil {
				return nil, err
			}
			return readSquashFSBlock(reader)
		},
	})
	squashfs.RegisterCompHandler(squashfs.XZ, &squashfs.CompHandler{
		Decompress: func(src []byte) ([]byte, error) {
			reader, err := squashfsxz.NewReader(bytes.NewReader(src), maxSquashFSDictionary)
			if err != nil {
				return nil, err
			}
			return readSquashFSBlock(reader)
		},
	})
	squashfs.RegisterCompHandler(squashfs.ZSTD, &squashfs.CompHandler{
		Decompress: func(src []byte) ([]byte, error) {
			reader, err := zstd.NewReader(bytes.NewReader(src),
				zstd.WithDecoderLowmem(true),
				zstd.WithDecoderMaxMemory(maxSquashFSDictionary))
			if err != nil {
				return nil, err
			}
			defer reader.Close()
			return readSquashFSBlock(reader)
		},
	})
}

func decompressSquashFSLZ4(src []byte) ([]byte, error) {
	dst := make([]byte, maxSquashFSBlock)
	n, err := lz4.UncompressBlock(src, dst)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("raw LZ4 block did not decompress")
	}
	return dst[:n], nil
}

func compressSquashFSLZ4(src []byte) ([]byte, error) {
	dst := make([]byte, lz4.CompressBlockBound(len(src)))
	n, err := lz4.CompressBlock(src, dst, nil)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	return dst[:n], nil
}

func readSquashFSBlock(reader io.Reader) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(reader, maxSquashFSBlock+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxSquashFSBlock {
		return nil, fmt.Errorf("SquashFS block exceeds maximum size %d", maxSquashFSBlock)
	}
	return content, nil
}

// parseSquashFS walks directly from each directory inode. Resolving every entry
// again by its full path makes KarpelesLab repeatedly decompress metadata and
// consumes excessive memory on large firmware filesystems.
func parseSquashFS(data []byte, limits Limits) (files []decodedFile, err error) {
	limits = limits.withDefaults()
	defer func() {
		if recovered := recover(); recovered != nil {
			files = nil
			err = fmt.Errorf("SquashFS parser panic: %v", recovered)
		}
	}()

	reader, err := squashfs.New(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open SquashFS: %w", err)
	}
	defer reader.Close()

	root, err := reader.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open SquashFS root: %w", err)
	}
	rootDir, ok := root.(fs.ReadDirFile)
	if !ok {
		_ = root.Close()
		return nil, fmt.Errorf("SquashFS root is not a directory")
	}
	walker := squashFSWalker{limits: limits}
	err = walker.walkDirectory(rootDir, "")
	if err != nil {
		return nil, fmt.Errorf("walk SquashFS: %w", err)
	}
	return walker.files, nil
}

type squashFSWalker struct {
	limits   Limits
	files    []decodedFile
	expanded int64
}

func (w *squashFSWalker) walkDirectory(directory fs.ReadDirFile, parent string) (err error) {
	defer func() {
		if closeErr := directory.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, entry := range entries {
		base := entry.Name()
		if base == "" || base == "." || base == ".." ||
			strings.Contains(base, "/") || strings.ContainsRune(base, 0) {
			return fmt.Errorf("unsafe SquashFS entry name %q", base)
		}
		name := base
		if parent != "" {
			name = parent + "/" + base
		}
		if !safeArchivePath(name) {
			return fmt.Errorf("unsafe SquashFS path %q", name)
		}
		if len(w.files) >= w.limits.MaxArtifacts {
			return fmt.Errorf("SquashFS entry count exceeds limit %d", w.limits.MaxArtifacts)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat SquashFS entry %q: %w", name, err)
		}
		inode, ok := info.Sys().(*squashfs.Inode)
		if !ok {
			return fmt.Errorf("SquashFS entry %q has no inode metadata", name)
		}
		decoded := decodedFile{Path: name, Mode: squashFSMode(info.Mode())}
		switch {
		case info.IsDir():
			decoded.Kind = "directory"
		case info.Mode()&fs.ModeSymlink != 0:
			decoded.Kind, decoded.Linkname = "symlink", string(inode.SymTarget)
		case info.Mode().IsRegular():
			decoded.Kind = "regular"
			decoded.LinkKey = squashFSLinkKey(inode)
		case inode.Type.Basic() == squashfs.CharDevType:
			decoded.Kind = "char-device"
			decoded.DeviceMajor, decoded.DeviceMinor = squashFSDeviceNumbers(inode.Rdev)
		case inode.Type.Basic() == squashfs.BlockDevType:
			decoded.Kind = "block-device"
			decoded.DeviceMajor, decoded.DeviceMinor = squashFSDeviceNumbers(inode.Rdev)
		case inode.Type.Basic() == squashfs.FifoType:
			decoded.Kind = "fifo"
		case inode.Type.Basic() == squashfs.SocketType:
			return fmt.Errorf("unsupported SquashFS socket %q", name)
		default:
			return fmt.Errorf("unsupported SquashFS inode type %d for %q", inode.Type, name)
		}
		w.files = append(w.files, decoded)

		if decoded.Kind == "directory" {
			child := inode.OpenFile(base)
			childDir, ok := child.(fs.ReadDirFile)
			if !ok {
				_ = child.Close()
				return fmt.Errorf("SquashFS entry %q is not a readable directory", name)
			}
			if err := w.walkDirectory(childDir, name); err != nil {
				return err
			}
			continue
		}
		if decoded.Kind != "regular" {
			continue
		}
		if info.Size() < 0 || info.Size() > w.limits.MaxExpandedBytes-w.expanded {
			return fmt.Errorf("SquashFS expanded bytes exceed limit %d", w.limits.MaxExpandedBytes)
		}
		opened := inode.OpenFile(base)
		content, readErr := readSquashFSContent(opened, name, info.Size())
		closeErr := opened.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return fmt.Errorf("close SquashFS entry %q: %w", name, closeErr)
		}
		if int64(len(content)) != info.Size() {
			return fmt.Errorf("short SquashFS entry %q: got %d bytes, want %d", name, len(content), info.Size())
		}
		w.expanded += int64(len(content))
		last := &w.files[len(w.files)-1]
		last.Data = content
		last.MaterializedBytes = int64(len(content))
	}
	return nil
}

// SquashFS stores the Linux 32-bit encoded device number. Decode it
// independently of the host OS so an image has the same metadata everywhere.
func squashFSDeviceNumbers(rdev uint32) (int64, int64) {
	major := (rdev >> 8) & 0xfff
	minor := (rdev & 0xff) | ((rdev >> 12) & 0xfff00)
	return int64(major), int64(minor)
}

func squashFSLinkKey(inode *squashfs.Inode) string {
	if inode.NLink <= 1 {
		return ""
	}
	return fmt.Sprintf("squashfs:%d", inode.Ino)
}

func squashFSMode(mode fs.FileMode) int64 {
	result := int64(mode.Perm())
	if mode&fs.ModeSetuid != 0 {
		result |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		result |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		result |= 0o1000
	}
	return result
}

func readSquashFSContent(reader io.Reader, name string, size int64) ([]byte, error) {
	if size == 0 {
		return []byte{}, nil
	}
	content, err := io.ReadAll(io.LimitReader(reader, size+1))
	if err != nil {
		return nil, fmt.Errorf("read SquashFS entry %q: %w", name, err)
	}
	return content, nil
}
