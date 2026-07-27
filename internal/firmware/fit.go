package firmware

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strings"

	"github.com/u-root/u-root/pkg/dt"
)

const fdtMagic = dt.Magic

func parseFIT(data []byte, limits Limits) ([]containerChild, error) {
	limits = limits.withDefaults()
	if len(data) < 40 || binary.BigEndian.Uint32(data[:4]) != dt.Magic {
		return nil, fmt.Errorf("not a FIT image")
	}
	if int64(len(data)) > limits.MaxImageBytes {
		return nil, fmt.Errorf("FIT size %d exceeds limit %d", len(data), limits.MaxImageBytes)
	}
	total := binary.BigEndian.Uint32(data[4:8])
	structOffset := binary.BigEndian.Uint32(data[8:12])
	stringsOffset := binary.BigEndian.Uint32(data[12:16])
	stringsSize := binary.BigEndian.Uint32(data[32:36])
	structSize := binary.BigEndian.Uint32(data[36:40])
	if total > uint32(len(data)) {
		return nil, fmt.Errorf("FIT total size %d exceeds input %d", total, len(data))
	}
	if _, ok := boundedEnd(int(structOffset), uint64(structSize), int(total)); !ok {
		return nil, fmt.Errorf("FIT structure block exceeds image")
	}
	if _, ok := boundedEnd(int(stringsOffset), uint64(stringsSize), int(total)); !ok {
		return nil, fmt.Errorf("FIT strings block exceeds image")
	}

	tree, err := dt.ReadFDT(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse FIT device tree: %w", err)
	}
	if err := checkFDTDepth(tree.RootNode, 1, limits.MaxDepth); err != nil {
		return nil, err
	}
	images, ok := tree.RootNode.LookupChildByName("images")
	if !ok {
		return nil, nil
	}
	if len(images.Children) > limits.MaxArtifacts {
		return nil, fmt.Errorf("FIT image count exceeds limit %d", limits.MaxArtifacts)
	}

	children := make([]containerChild, 0, len(images.Children))
	for _, image := range images.Children {
		dataProperty, ok := image.LookProperty("data")
		if !ok {
			if _, external := image.LookProperty("data-offset"); external {
				return nil, fmt.Errorf("FIT image %q uses unsupported external data", image.Name)
			}
			if _, external := image.LookProperty("data-position"); external {
				return nil, fmt.Errorf("FIT image %q uses unsupported external data", image.Name)
			}
			continue
		}
		if err := verifyFITHashes(image, dataProperty.Value); err != nil {
			return nil, err
		}
		compression := ""
		if property, ok := image.LookProperty("compression"); ok {
			compression = cString(property.Value)
		}
		offset := bytes.Index(data, dataProperty.Value)
		if offset < 0 || bytes.Count(data, dataProperty.Value) != 1 {
			offset = -1
		}
		children = append(children, containerChild{
			Name: image.Name, Data: dataProperty.Value, Offset: int64(offset),
			Compression: compression,
		})
	}
	if len(children) == 0 {
		return nil, fmt.Errorf("FIT contains no supported embedded data properties")
	}
	return children, nil
}

func checkFDTDepth(node *dt.Node, depth, maxDepth int) error {
	if depth > maxDepth {
		return fmt.Errorf("FIT nesting exceeds limit %d", maxDepth)
	}
	for _, child := range node.Children {
		if err := checkFDTDepth(child, depth+1, maxDepth); err != nil {
			return err
		}
	}
	return nil
}

func verifyFITHashes(image *dt.Node, data []byte) error {
	for _, child := range image.Children {
		if !strings.HasPrefix(child.Name, "hash") {
			continue
		}
		algoProperty, hasAlgo := child.LookProperty("algo")
		valueProperty, hasValue := child.LookProperty("value")
		if !hasAlgo || !hasValue {
			return fmt.Errorf("FIT image %q hash node %q is incomplete", image.Name, child.Name)
		}
		algo := cString(algoProperty.Value)
		var got []byte
		switch algo {
		case "crc32":
			got = make([]byte, 4)
			binary.BigEndian.PutUint32(got, crc32.ChecksumIEEE(data))
		case "sha1":
			sum := sha1.Sum(data)
			got = sum[:]
		case "sha256":
			sum := sha256.Sum256(data)
			got = sum[:]
		default:
			return fmt.Errorf("FIT image %q uses unsupported hash %q", image.Name, algo)
		}
		if !bytes.Equal(got, valueProperty.Value) {
			return fmt.Errorf("FIT image %q %s hash mismatch", image.Name, algo)
		}
	}
	return nil
}
