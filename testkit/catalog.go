package testkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

type Catalog struct {
	Version  int       `json:"version"`
	Runtimes []Runtime `json:"runtimes"`
}

type Runtime struct {
	Name      string   `json:"name"`
	Models    []string `json:"models"`
	Image     string   `json:"image"`
	Isolation string   `json:"isolation"` // "fleet" or "device"
	CapAdd    []string `json:"cap_add"`
}

func (c *Catalog) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported catalog version: %d", c.Version)
	}
	names := make(map[string]bool)
	models := make(map[string]string)
	for _, r := range c.Runtimes {
		if r.Name == "" {
			return fmt.Errorf("runtime name cannot be empty")
		}
		if names[r.Name] {
			return fmt.Errorf("duplicate runtime name: %s", r.Name)
		}
		names[r.Name] = true

		if r.Image == "" {
			return fmt.Errorf("runtime %s: image cannot be empty", r.Name)
		}
		if len(r.Models) == 0 {
			return fmt.Errorf("runtime %s: models cannot be empty", r.Name)
		}
		if r.Isolation != "fleet" && r.Isolation != "device" {
			return fmt.Errorf("runtime %s: invalid isolation %q", r.Name, r.Isolation)
		}
		for _, m := range r.Models {
			if m == "" {
				return fmt.Errorf("runtime %s: model cannot be empty", r.Name)
			}
			if existing, ok := models[m]; ok {
				return fmt.Errorf("model %s is assigned to multiple runtimes: %s and %s", m, existing, r.Name)
			}
			models[m] = r.Name
		}
	}
	return nil
}

func LoadCatalog(path string) (*Catalog, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open catalog %s: %w", path, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()

	var cat Catalog
	if err := dec.Decode(&cat); err != nil {
		return nil, fmt.Errorf("parse catalog %s: %w", path, err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("parse catalog %s: trailing JSON", path)
		}
		return nil, fmt.Errorf("parse catalog %s: %w", path, err)
	}

	if err := cat.Validate(); err != nil {
		return nil, fmt.Errorf("validate catalog %s: %w", path, err)
	}

	return &cat, nil
}

func (c *Catalog) RuntimeFor(model string) *Runtime {
	if c == nil {
		return nil
	}
	for i := range c.Runtimes {
		r := &c.Runtimes[i]
		for _, m := range r.Models {
			if m == model {
				return r
			}
		}
	}
	return nil
}
