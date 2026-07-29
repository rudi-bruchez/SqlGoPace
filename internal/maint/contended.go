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
	LockMode     string `yaml:"lock_mode,omitempty"`
	TimesBlocked int    `yaml:"times_blocked,omitempty"`
	FirstSeen    string `yaml:"first_seen,omitempty"`
	LastSeen     string `yaml:"last_seen,omitempty"`
	// Tail-position capture (see the tail-object design spec). ConfirmedBy is "lock" for a
	// lock-held blocker or "tail_position" for one found by the backward page walk; empty is
	// a legacy sidecar, read as "lock". IndexID/PageFromEnd are set only for tail entries.
	IndexID     int    `yaml:"index_id,omitempty"`
	ConfirmedBy string `yaml:"confirmed_by,omitempty"`
	PageFromEnd int    `yaml:"page_from_end,omitempty"`
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
