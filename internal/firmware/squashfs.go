package firmware

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"

	"github.com/CalebQ42/squashfs"
)

// parseSquashFS uses the archive's io/fs view so file contents stay lazy until
// this extractor has checked its own aggregate limits.
func parseSquashFS(data []byte, limits Limits) (files []decodedFile, err error) {
	limits = limits.withDefaults()
	defer func() {
		if recovered := recover(); recovered != nil {
			files = nil
			err = fmt.Errorf("SquashFS parser panic: %v", recovered)
		}
	}()

	reader, err := squashfs.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open SquashFS: %w", err)
	}
	var expanded int64
	err = fs.WalkDir(&reader, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if len(files) >= limits.MaxArtifacts {
			return fmt.Errorf("SquashFS entry count exceeds limit %d", limits.MaxArtifacts)
		}
		opened, err := reader.OpenFile(name)
		if err != nil {
			return err
		}
		defer opened.Close()
		decoded := decodedFile{Path: name, Mode: int64(opened.Low.Inode.Perm & 0o7777)}
		switch {
		case info.IsDir():
			decoded.Kind = "directory"
		case info.Mode()&fs.ModeSymlink != 0:
			decoded.Kind, decoded.Linkname = "symlink", opened.SymlinkPath()
		case info.Mode().IsRegular():
			decoded.Kind = "regular"
			if opened.Low.Inode.LinkCount() > 1 {
				decoded.LinkKey = fmt.Sprintf("squashfs:%d", opened.Low.Inode.Num)
			}
		default:
			return nil
		}
		if decoded.Kind != "regular" {
			files = append(files, decoded)
			return nil
		}
		if info.Size() < 0 || info.Size() > limits.MaxExpandedBytes-expanded {
			return fmt.Errorf("SquashFS expanded bytes exceed limit %d", limits.MaxExpandedBytes)
		}
		content, readErr := readSquashFSContent(opened, name, info.Size())
		if readErr != nil {
			return readErr
		}
		if int64(len(content)) != info.Size() {
			return fmt.Errorf("short SquashFS entry %q: got %d bytes, want %d", name, len(content), info.Size())
		}
		expanded += int64(len(content))
		decoded.Data = content
		decoded.MaterializedBytes = int64(len(content))
		files = append(files, decoded)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk SquashFS: %w", err)
	}
	return files, nil
}

func readSquashFSContent(reader io.Reader, name string, size int64) ([]byte, error) {
	// CalebQ42/squashfs v1.4.1 initializes a block reader even for an empty
	// regular file. Such a file has neither data blocks nor a fragment, so its
	// unconditional Block(0) call reports "invalid block index".
	if size == 0 {
		return []byte{}, nil
	}
	content, err := io.ReadAll(io.LimitReader(reader, size+1))
	if err != nil {
		return nil, fmt.Errorf("read SquashFS entry %q: %w", name, err)
	}
	return content, nil
}
