package firmware

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/u-root/u-root/pkg/dt"
)

func TestParseFITReturnsImageDataAndCompression(t *testing.T) {
	fit := testFIT("kernel-1", "lzo", []byte("kernel data"))
	children, err := parseFIT(fit, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Name != "kernel-1" ||
		children[0].Compression != "lzo" || string(children[0].Data) != "kernel data" ||
		children[0].Offset <= 0 {
		t.Fatalf("children = %+v", children)
	}
}

func TestParseFITRejectsOutOfBoundsProperty(t *testing.T) {
	fit := testFIT("kernel-1", "none", []byte("data"))
	binary.BigEndian.PutUint32(fit[36:40], 0xffffffff)
	if _, err := parseFIT(fit, DefaultLimits()); err == nil {
		t.Fatal("accepted out-of-bounds FIT structure")
	}
}

func testFIT(name, compression string, data []byte) []byte {
	tree := &dt.FDT{
		Header: dt.Header{
			Magic: dt.Magic, Version: 17, LastCompVersion: 16,
		},
		RootNode: dt.NewNode("",
			dt.WithChildren(dt.NewNode("images",
				dt.WithChildren(dt.NewNode(name,
					dt.WithProperty(
						dt.PropertyString("compression", compression),
						dt.Property{Name: "data", Value: data},
					),
				)),
			)),
		),
	}
	var out bytes.Buffer
	if _, err := tree.Write(&out); err != nil {
		panic(err)
	}
	return out.Bytes()
}
