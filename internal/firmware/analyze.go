package firmware

import (
	"bytes"
	"slices"
	"strings"
)

var knownCapabilityFields = []string{
	"ble_caps", "feature_caps", "feature_caps2", "fw_caps", "fw2_caps", "fw3_caps",
	"hw_caps", "poe_caps", "port_caps", "radio_caps", "radio_caps2", "speed_caps",
	"switch_caps", "udapi_caps", "usg_caps", "usg2_caps", "vlan_caps", "wifi_caps",
	"wifi_caps2",
}

var knownCommands = []string{
	"kick-sta", "led-locate", "linkperf-start", "linkperf-stop", "noop",
	"quick-scan", "reboot", "set-adopt", "setdefault", "setparam", "setstate",
	"spectrum-scan", "unblock-sta", "upgrade",
}

var knownInformFields = []string{
	"adopted", "architecture", "authkey", "board_rev", "bomrev", "bootrom_version",
	"cfgversion", "cfgversion_effective", "cfgversion_saved", "default", "if_table",
	"inform_url", "kernel_version", "lldp_table", "manufacturer_id", "port_table",
	"reboot_duration", "required_version", "required_versions", "state", "sys_stats",
	"upgrade_duration", "use_aes_gcm", "vap_table",
}

var knownIdentifiers = []string{
	"board", "board_id", "model", "product", "sysid", "system_id",
}

func Analyze(files []decodedFile, minLength int) []Observation {
	if minLength < 1 {
		minLength = 4
	}
	var observations []Observation
	for _, file := range files {
		for start := 0; start < len(file.Data); {
			for start < len(file.Data) && !printable(file.Data[start]) {
				start++
			}
			end := start
			for end < len(file.Data) && printable(file.Data[end]) {
				end++
			}
			if end-start >= minLength {
				observations = append(observations, analyzeRun(file.Path, file.Data[start:end], int64(start))...)
			}
			start = end + 1
		}
	}
	slices.SortFunc(observations, compareObservation)
	return slices.Compact(observations)
}

func analyzeRun(artifact string, run []byte, base int64) []Observation {
	var out []Observation
	for _, capability := range knownCapabilityFields {
		for _, offset := range boundedOccurrences(run, capability, false) {
			out = append(out, staticObservation("capability", capability, artifact, base+int64(offset), run, offset))
		}
	}
	for _, command := range knownCommands {
		for _, offset := range boundedOccurrences(run, command, false) {
			out = append(out, staticObservation("command", command, artifact, base+int64(offset), run, offset))
		}
	}
	for _, field := range knownInformFields {
		for _, offset := range boundedOccurrences(run, field, false) {
			out = append(out, staticObservation("inform_field", field, artifact, base+int64(offset), run, offset))
		}
	}
	for _, identifier := range knownIdentifiers {
		for _, offset := range boundedOccurrences(run, identifier, false) {
			out = append(out, staticObservation("identifier", identifier, artifact, base+int64(offset), run, offset))
		}
	}
	return out
}

func boundedOccurrences(data []byte, needle string, allowUnderscoreSuffix bool) []int {
	var offsets []int
	for searchFrom := 0; searchFrom < len(data); {
		relative := bytes.Index(data[searchFrom:], []byte(needle))
		if relative < 0 {
			break
		}
		start := searchFrom + relative
		end := start + len(needle)
		leftOK := start == 0 || !identifierByte(data[start-1])
		rightOK := end == len(data) || !identifierByte(data[end]) ||
			(allowUnderscoreSuffix && data[end] == '_')
		if leftOK && rightOK {
			offsets = append(offsets, start)
		}
		searchFrom = start + 1
	}
	return offsets
}

func staticObservation(kind, value, artifact string, offset int64, run []byte, runOffset int) Observation {
	const contextRadius = 32
	start := max(0, runOffset-contextRadius)
	end := min(len(run), runOffset+len(value)+contextRadius)
	return Observation{
		Kind: kind, Value: value, Artifact: artifact, Offset: offset,
		OffsetBasis: "artifact", Confidence: "observed_static", Source: "firmware",
		Context: string(run[start:end]),
	}
}

func classifyToken(value string) (string, bool) {
	if slices.Contains(knownCapabilityFields, value) {
		return "capability", true
	}
	if slices.Contains(knownCommands, value) {
		return "command", true
	}
	if slices.Contains(knownInformFields, value) {
		return "inform_field", true
	}
	if slices.Contains(knownIdentifiers, value) {
		return "identifier", true
	}
	return "", false
}

func printable(value byte) bool {
	return value >= 0x20 && value <= 0x7e
}

func identifierByte(value byte) bool {
	return value == '_' || value == '-' ||
		value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9'
}

func observationKey(observation Observation) string {
	return strings.Join([]string{observation.Kind, observation.Value}, ":")
}
