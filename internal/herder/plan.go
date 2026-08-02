package herder

import (
	"regexp"
	"strconv"
	"strings"

	emu "github.com/jamesbraid/unifi-emu"
)

// syntheticImageRepo is the public synthetic image the release pipeline
// builds from the same tag as the herder binary.
const syntheticImageRepo = "ghcr.io/jamesbraid/unifi-emu"

// UnitKind distinguishes the one batched public synthetic container from the
// standalone opaque runtime containers.
type UnitKind int

const (
	UnitSynthetic UnitKind = iota
	UnitOpaque
)

func (k UnitKind) String() string {
	if k == UnitOpaque {
		return "opaque"
	}
	return "synthetic"
}

// Unit is one planned container. The topology is fixed: every unmapped device
// rides in the single synthetic unit, and every mapped device gets a unit of
// its own -- including two devices of the same model. That removes any need
// for a runtime mode or a grouping language in the request.
type Unit struct {
	Kind    UnitKind
	Image   string
	CapAdd  []string
	Devices []Identity
}

// DeviceIndices renders the unit's request indices for the public run label.
func (u Unit) DeviceIndices() string {
	parts := make([]string, len(u.Devices))
	for i, d := range u.Devices {
		parts[i] = strconv.Itoa(d.Index)
	}
	return strings.Join(parts, ",")
}

// EndpointMAC is the MAC to assign to this unit's network endpoint, or "" for
// the synthetic batch. The batch stands for several virtual devices behind
// one Docker endpoint, so claiming one device's MAC there would be a lie
// about the other devices, and every synthetic device keeps its virtual MAC
// inside the container instead.
func (u Unit) EndpointMAC() string {
	if u.Kind != UnitOpaque || len(u.Devices) != 1 {
		return ""
	}
	return u.Devices[0].MAC
}

// Plan is the immutable launch plan. Nothing after planning changes it.
type Plan struct {
	RunID     string
	Network   string
	InformURL string
	Units     []Unit
}

// PlanInput is everything BuildPlan needs, already validated individually.
type PlanInput struct {
	RunID          string
	Network        string
	InformURL      string
	Devices        []Identity
	Runtime        RuntimeConfig
	SyntheticImage string
}

// BuildPlan maps every resolved identity to synthetic or opaque execution and
// freezes the result in request order.
func BuildPlan(in PlanInput) (Plan, error) {
	// Every requested model must exist in the public synthetic registry,
	// mapped or not. That is what guarantees the same request runs
	// all-synthetic on a runner with no mapping installed.
	for _, d := range in.Devices {
		if _, ok := emu.Profile(d.Model); !ok {
			return Plan{}, failf(CodeSyntheticModelUnknown, PhaseValidate,
				"device %d model %q has no synthetic profile", d.Index, d.Model)
		}
	}

	plan := Plan{RunID: in.RunID, Network: in.Network, InformURL: in.InformURL}
	var synthetic []Identity
	syntheticFirst := -1
	for _, d := range in.Devices {
		rt, mapped := in.Runtime.Mapping(d.Model)
		if !mapped {
			if syntheticFirst < 0 {
				syntheticFirst = len(plan.Units)
				plan.Units = append(plan.Units, Unit{Kind: UnitSynthetic})
			}
			synthetic = append(synthetic, d)
			continue
		}
		plan.Units = append(plan.Units, Unit{
			Kind: UnitOpaque, Image: rt.Image, CapAdd: rt.CapAdd, Devices: []Identity{d},
		})
	}
	if syntheticFirst >= 0 {
		if in.SyntheticImage == "" {
			return Plan{}, failf(CodeSyntheticImageRequired, PhaseValidate,
				"%d device(s) need the public synthetic image and none was selected", len(synthetic))
		}
		plan.Units[syntheticFirst].Image = in.SyntheticImage
		plan.Units[syntheticFirst].Devices = synthetic
	}
	return plan, nil
}

// Images lists every image the plan needs, deduplicated, in unit order. The
// herder pulls all of them before it creates any container, so a registry
// failure cannot leave half a fleet running.
func (p Plan) Images() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(p.Units))
	for _, u := range p.Units {
		if seen[u.Image] {
			continue
		}
		seen[u.Image] = true
		out = append(out, u.Image)
	}
	return out
}

// Devices returns every planned identity in request order.
func (p Plan) Devices() []Identity {
	out := make([]Identity, 0, len(p.Units))
	for _, u := range p.Units {
		out = append(out, u.Devices...)
	}
	sortByIndex(out)
	return out
}

func sortByIndex(ids []Identity) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j].Index < ids[j-1].Index; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}

// ResolveSyntheticImage picks the public synthetic image: the development
// override first, then the image the release pipeline compiled in. Neither
// path falls back to a floating tag -- an unpinned "latest" would silently
// test a different emulator than the herder was built from -- so an empty
// result is left for BuildPlan to reject when the plan actually needs one.
func ResolveSyntheticImage(flagImage, compiledDefault string) (string, error) {
	if flagImage != "" {
		return flagImage, nil
	}
	return compiledDefault, nil
}

// releaseVersion matches a published tag in either the "v0.5.0" form the go
// command records in a binary or the "0.5.0" form goreleaser's .Version passes.
// What it rejects is the point: since Go 1.24 a plain `go build` also stamps a
// version, but a checkout reports "(devel)", a pseudo-version, or a release with
// a "+dirty" suffix, and no image was ever published from any of those.
var releaseVersion = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// ReleaseVersion returns version without its leading "v" when it names a
// published release, and "" for anything built from a working tree.
func ReleaseVersion(version string) string {
	if !releaseVersion.MatchString(version) {
		return ""
	}
	return strings.TrimPrefix(version, "v")
}

// DefaultSyntheticImage is the version-matched public image for a release
// build. A development binary has no release version and no image default, so
// it requires --synthetic-image whenever the plan contains a synthetic device.
func DefaultSyntheticImage(version string) string {
	release := ReleaseVersion(version)
	if release == "" {
		return ""
	}
	return syntheticImageRepo + ":" + release
}
