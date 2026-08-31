// Package dotenv loads a .env file (KEY=VALUE lines) into the process
// environment without overwriting variables that are already set. It is a
// copied stdlib-only loader rather than a dependency, on purpose: secrets
// handling is small enough to own, and one less module in go.mod is one less
// thing to audit.
package dotenv

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Parse reads KEY=VALUE lines. Blank lines and comments (#) are skipped, an
// "export " prefix is tolerated, and one surrounding pair of single or double
// quotes is removed from the value. A non-blank line without '=' is an error.
func Parse(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: no '=': %q", line, s)
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("line %d: empty key", line)
		}
		out[k] = unquote(strings.TrimSpace(v))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return out, nil
}

func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// Load reads the .env file at path and sets every key that is not already in
// the environment: a real environment variable always wins over the file. A
// missing file is a silent no-op, never an error — .env is optional.
func Load(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	vars, err := Parse(f)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	for k, v := range vars {
		if _, ok := os.LookupEnv(k); ok {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("set %s: %w", k, err)
		}
	}
	return nil
}
