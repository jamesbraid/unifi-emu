package firmware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

type EvidenceComparison struct {
	Agreed     []string `json:"agreed,omitempty"`
	StaticOnly []string `json:"static_only,omitempty"`
	LiveOnly   []string `json:"live_only,omitempty"`
}

func ParseLiveEvidence(r io.Reader) ([]Observation, error) {
	observations, _, err := ParseLiveSnapshot(r)
	return observations, err
}

// ParseLiveSnapshot preserves every JSON path and JSON type in addition to the
// classified evidence used for static/live comparison. Callers must sanitize
// secrets before supplying a snapshot.
func ParseLiveSnapshot(r io.Reader) ([]Observation, []LiveField, error) {
	const maxLiveEvidence = 16 << 20
	data, err := io.ReadAll(io.LimitReader(r, maxLiveEvidence+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read live evidence: %w", err)
	}
	if len(data) > maxLiveEvidence {
		return nil, nil, fmt.Errorf("live evidence exceeds 16 MiB limit")
	}
	var value any
	var observations []Observation
	var fields []LiveField
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&value) == nil {
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				return nil, nil, fmt.Errorf("live evidence contains multiple JSON values")
			}
			return nil, nil, fmt.Errorf("decode trailing live evidence: %w", err)
		}
		walkLiveJSON(value, "$", &observations)
		walkLiveFields(value, "", &fields)
	} else {
		observations = parseLiveText(string(data))
	}
	slices.SortFunc(observations, compareObservation)
	slices.SortFunc(fields, func(a, b LiveField) int { return cmpStrings(a.Path, b.Path) })
	return slices.Compact(observations), fields, nil
}

func walkLiveFields(value any, path string, fields *[]LiveField) {
	field := LiveField{Path: path}
	switch typed := value.(type) {
	case map[string]any:
		field.Type = "object"
		*fields = append(*fields, field)
		for key, child := range typed {
			walkLiveFields(child, path+"/"+jsonPointerToken(key), fields)
		}
	case []any:
		field.Type = "array"
		*fields = append(*fields, field)
		for i, child := range typed {
			walkLiveFields(child, fmt.Sprintf("%s/%d", path, i), fields)
		}
	case string:
		field.Type, field.Value = "string", typed
		*fields = append(*fields, field)
	case json.Number:
		field.Type, field.Value = "number", typed.String()
		*fields = append(*fields, field)
	case bool:
		field.Type, field.Value = "boolean", fmt.Sprint(typed)
		*fields = append(*fields, field)
	case nil:
		field.Type, field.Value = "null", "null"
		*fields = append(*fields, field)
	}
}

func jsonPointerToken(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func walkLiveJSON(value any, path string, observations *[]Observation) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if kind, ok := classifyToken(key); ok {
				literal, _ := liveScalar(child)
				*observations = append(*observations, Observation{
					Kind: kind, Value: key, ObservedValue: literal, Artifact: path,
					Confidence: "observed_live", Source: "live",
				})
			}
			walkLiveJSON(child, path+"."+key, observations)
		}
	case []any:
		for i, child := range typed {
			walkLiveJSON(child, fmt.Sprintf("%s[%d]", path, i), observations)
		}
	case string:
		if kind, ok := classifyToken(typed); ok {
			*observations = append(*observations, Observation{
				Kind: kind, Value: typed, Artifact: path,
				Confidence: "observed_live", Source: "live",
			})
		}
	}
}

func parseLiveText(data string) []Observation {
	var observations []Observation
	offset := 0
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if kind, classified := classifyToken(key); classified {
				observations = append(observations, Observation{
					Kind: kind, Value: key, ObservedValue: value, Artifact: "live-dump",
					Offset: int64(offset), OffsetBasis: "artifact",
					Confidence: "observed_live", Source: "live",
				})
			}
			if kind, classified := classifyToken(value); classified {
				observations = append(observations, Observation{
					Kind: kind, Value: value, Artifact: "live-dump",
					Offset: int64(offset + strings.Index(line, value)), OffsetBasis: "artifact",
					Confidence: "observed_live", Source: "live",
				})
			}
		}
		offset += len(line) + 1
	}
	return observations
}

func liveScalar(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case json.Number:
		return typed.String(), true
	case float64, bool:
		return fmt.Sprint(typed), true
	case nil:
		return "null", true
	default:
		return "", false
	}
}

func CompareEvidence(static, live []Observation) EvidenceComparison {
	staticSet := evidenceKeys(static)
	liveSet := evidenceKeys(live)
	var comparison EvidenceComparison
	for key := range staticSet {
		if liveSet[key] {
			comparison.Agreed = append(comparison.Agreed, key)
		} else {
			comparison.StaticOnly = append(comparison.StaticOnly, key)
		}
	}
	for key := range liveSet {
		if !staticSet[key] {
			comparison.LiveOnly = append(comparison.LiveOnly, key)
		}
	}
	slices.Sort(comparison.Agreed)
	slices.Sort(comparison.StaticOnly)
	slices.Sort(comparison.LiveOnly)
	return comparison
}

func evidenceKeys(observations []Observation) map[string]bool {
	out := make(map[string]bool, len(observations))
	for _, observation := range observations {
		if observation.Kind != "" && strings.TrimSpace(observation.Value) != "" {
			out[observationKey(observation)] = true
		}
	}
	return out
}
