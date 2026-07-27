package firmware

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"slices"
	"strings"
)

type CapabilityDefinitions map[string][]CapabilityBitmap

type CapabilityBitmap struct {
	Name string
	Bits map[string]uint64
}

type CapabilityValue struct {
	Path        string   `json:"path"`
	Field       string   `json:"field"`
	Bitmap      string   `json:"bitmap"`
	RawValue    string   `json:"raw_value"`
	SetBits     []string `json:"set_bits,omitempty"`
	UnknownMask string   `json:"unknown_mask,omitempty"`
}

func ParseCapabilityDefinitions(r io.Reader) (CapabilityDefinitions, error) {
	var envelope struct {
		Bitmaps map[string]struct {
			Field string                 `json:"field"`
			Bits  map[string]json.Number `json:"bits"`
		} `json:"bitmaps"`
	}
	decoder := json.NewDecoder(r)
	decoder.UseNumber()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode capability definitions: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode capability definitions: multiple JSON values")
		}
		return nil, fmt.Errorf("decode trailing capability definitions: %w", err)
	}
	definitions := make(CapabilityDefinitions)
	for name, raw := range envelope.Bitmaps {
		if raw.Field == "" {
			continue
		}
		bitmap := CapabilityBitmap{Name: name, Bits: make(map[string]uint64)}
		for bitName, number := range raw.Bits {
			value, ok := new(big.Int).SetString(number.String(), 10)
			if !ok {
				return nil, fmt.Errorf("capability %s bit %s: invalid integer %q", name, bitName, number)
			}
			var bitValue uint64
			switch {
			case value.Sign() < 0:
				if !value.IsInt64() || value.Int64() < -1<<31 {
					return nil, fmt.Errorf("capability %s bit %s: negative value %s is outside Java int range",
						name, bitName, value)
				}
				bitValue = uint64(uint32(int32(value.Int64())))
			case value.IsUint64():
				bitValue = value.Uint64()
			default:
				return nil, fmt.Errorf("capability %s bit %s: value %s exceeds 64 bits",
					name, bitName, value)
			}
			if bitValue == 0 || bitValue&(bitValue-1) != 0 {
				return nil, fmt.Errorf("capability %s bit %s: value %d is not a single bit",
					name, bitName, bitValue)
			}
			bitmap.Bits[bitName] = bitValue
		}
		definitions[raw.Field] = append(definitions[raw.Field], bitmap)
	}
	for field := range definitions {
		slices.SortFunc(definitions[field], func(a, b CapabilityBitmap) int {
			return cmpStrings(a.Name, b.Name)
		})
	}
	return definitions, nil
}

func DecodeCapabilityValues(fields []LiveField, definitions CapabilityDefinitions) []CapabilityValue {
	var values []CapabilityValue
	for _, field := range fields {
		if field.Type != "number" {
			continue
		}
		name := liveFieldName(field.Path)
		raw, ok := new(big.Int).SetString(field.Value, 10)
		if !ok {
			continue
		}
		if raw.Sign() < 0 {
			if !raw.IsInt64() || raw.Int64() < -1<<31 {
				continue
			}
			raw.SetUint64(uint64(uint32(int32(raw.Int64()))))
		}
		for _, bitmap := range definitions[name] {
			known := new(big.Int)
			var set []string
			for bitName, bitValue := range bitmap.Bits {
				bit := new(big.Int).SetUint64(bitValue)
				known.Or(known, bit)
				if new(big.Int).And(new(big.Int).Set(raw), bit).Sign() != 0 {
					set = append(set, bitName)
				}
			}
			slices.Sort(set)
			unknown := new(big.Int).AndNot(new(big.Int).Set(raw), known)
			value := CapabilityValue{
				Path: field.Path, Field: name, Bitmap: bitmap.Name,
				RawValue: field.Value, SetBits: set,
			}
			if unknown.Sign() != 0 {
				value.UnknownMask = unknown.String()
			}
			values = append(values, value)
		}
	}
	slices.SortFunc(values, func(a, b CapabilityValue) int {
		if n := cmpStrings(a.Path, b.Path); n != 0 {
			return n
		}
		return cmpStrings(a.Bitmap, b.Bitmap)
	})
	return values
}

func liveFieldName(path string) string {
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		path = path[index+1:]
	}
	return strings.ReplaceAll(strings.ReplaceAll(path, "~1", "/"), "~0", "~")
}
