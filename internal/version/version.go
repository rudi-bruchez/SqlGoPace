// Package version exposes the application version. To change it, edit the VERSION
// file in this directory before building — it is embedded into the binary, so no
// build flags are required. A release pipeline may still override it without
// editing the file via:
//
//	go build -ldflags "-X github.com/rudi-bruchez/SqlGoPace/internal/version.override=1.2.3" ./cmd/sqlgopace
package version

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var embedded string

// override is injected at build time via -ldflags; when empty the VERSION file
// content is used.
var override string

// Version returns the application version string (trimmed of surrounding space).
func Version() string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	return strings.TrimSpace(embedded)
}
