package maint

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// ContendedObject is one user object a shrink held a Sch-M lock on while blocking other
// sessions — an empirically confirmed tail blocker. Read back by `plan --confirmed`.
type ContendedObject struct {
	ObjectID     int64  `yaml:"object_id"`
	Schema       string `yaml:"schema"`
	Table        string `yaml:"table"`
	LockMode     string `yaml:"lock_mode"`
	TimesBlocked int    `yaml:"times_blocked"`
	FirstSeen    string `yaml:"first_seen"`
	LastSeen     string `yaml:"last_seen"`
}

// ContendedDoc is the machine body of a .contended.yaml sidecar.
type ContendedDoc struct {
	Database string            `yaml:"database"`
	Observed []ContendedObject `yaml:"observed"`
}

// ParseContended decodes a .contended.yaml sidecar, rejecting unknown fields so a
// malformed file fails loudly rather than silently dropping data.
func ParseContended(data []byte) (ContendedDoc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc ContendedDoc
	if err := dec.Decode(&doc); err != nil {
		return ContendedDoc{}, fmt.Errorf("parse contended sidecar: %w", err)
	}
	return doc, nil
}
