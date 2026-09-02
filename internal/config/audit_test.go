package config

// Two mechanical audits over the configuration surface.
//
// They exist because two classes of defect walked past TDD and past a diff-scoped
// code review, and neither class is a bug in any single change:
//
//   - An inert key: checkpoint_between_operations was parsed, documented and
//     shipped in config.yaml, and read by nothing. Every diff that touched it was
//     internally coherent, and no test failed, because no test existed.
//   - A default that only the documentation believes in: the shipped config.yaml
//     states a value, the code applies another one when the key is absent, and the
//     operator who deletes the key gets the second.
//
// Both are found by walking the Config type rather than by reading a diff, which is
// why they belong in a test and not in a review checklist.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is where this package sits relative to the module root.
const repoRoot = "../.."

// configLeaf is one settable configuration value: a YAML key and the Go field it
// decodes into.
type configLeaf struct {
	yamlPath string // "monitoring.checkpoint_between_operations"
	field    string // "CheckpointBetweenOperations"
	value    any    // the parsed value, for the defaults audit
}

// configLeaves flattens a Config into its settable keys and their values. It recurses
// into exported struct types (MonitoringConfig, EmailConfig, ...) and stops at unexported
// ones (forceBool, forceInt), whose wrapper field is the key an operator writes.
//
// Both audits walk the same leaves: one asks whether each key is read, the other what
// value it holds. Two walks would have to be kept in agreement, and a leaf missing from
// one of them would silently compare a value against nothing.
func configLeaves(c *Config) []configLeaf {
	var out []configLeaf

	var walk func(v reflect.Value, yamlPrefix string)
	walk = func(v reflect.Value, yamlPrefix string) {
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if tag == "-" {
				continue
			}
			if tag == "" {
				tag = strings.ToLower(f.Name)
			}
			path := tag
			if yamlPrefix != "" {
				path = yamlPrefix + "." + tag
			}
			if fv := v.Field(i); fv.Kind() == reflect.Struct && ast.IsExported(fv.Type().Name()) {
				walk(fv, path)
				continue
			}
			out = append(out, configLeaf{yamlPath: path, field: f.Name, value: v.Field(i).Interface()})
		}
	}
	walk(reflect.ValueOf(*c), "")
	return out
}

// TestNoInertConfigKey fails when a key an operator can set in config.yaml is never
// read outside this package.
//
// A field counts as read when its name is selected in a non-test file outside
// internal/config, or when a method of this package mentions it and that method is
// itself called from outside — the accessor path, which is how most of Monitoring is
// consumed (cfg.Monitoring.BlockingPoll(), never .BlockingPollSeconds).
//
// The match is by identifier name, so a field whose name is shared with another
// type's field (Enabled, Host) can be laundered by that other type's use. The audit
// therefore under-reports; it never invents. A finding here is real.
func TestNoInertConfigKey(t *testing.T) {
	leaves := configLeaves(&Config{})
	fieldNames := make(map[string]bool, len(leaves))
	for _, l := range leaves {
		fieldNames[l.field] = true
	}

	outside := sourceOutsideConfig(t)
	accessors := accessorsMentioning(t, fieldNames)

	// Keys deliberately not read by the engine. Each entry states why, and is a
	// promise that the key still does something an operator can observe.
	allowed := map[string]string{}

	for _, l := range leaves {
		if reason, ok := allowed[l.yamlPath]; ok {
			t.Logf("skipping %s: %s", l.yamlPath, reason)
			continue
		}
		if selects(outside, l.field) {
			continue
		}
		if readViaAccessor(accessors, outside, l.field) {
			continue
		}
		t.Errorf("config key %q (field %s) is parsed, validated and shipped in config.yaml, "+
			"but nothing outside internal/config reads it, directly or through an accessor. "+
			"Either wire it up, delete it from config.yaml and the docs, or add it to the "+
			"allow-list in this test with the reason it has no read site.", l.yamlPath, l.field)
	}
}

// sourceOutsideConfig returns every non-test Go source file in the module except
// this package's own, joined. Parsing and validating a key is not using it: a key
// this package alone touches is inert from the operator's point of view.
func sourceOutsideConfig(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "bin" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) == "config" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		b.Write(data)
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatalf("walk module source: %v", err)
	}
	if b.Len() == 0 {
		t.Fatal("no source found outside internal/config; the audit would pass vacuously")
	}
	return b.String()
}

// accessorsMentioning maps each method of this package to the config fields its body
// selects, so a field reached only through cfg.Monitoring.BlockingPoll() still counts
// as read. Unexported methods (applyDefaults, validate) are included and simply never
// match an outside call, which is the point: defaulting a field is not using it.
func accessorsMentioning(t *testing.T, fields map[string]bool) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			mentioned := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && fields[sel.Sel.Name] {
					mentioned[sel.Sel.Name] = true
				}
				return true
			})
			if len(mentioned) > 0 {
				out[fn.Name.Name] = mentioned
			}
		}
	}
	return out
}

// selects reports whether the source selects the named identifier (".Name").
func selects(source, name string) bool {
	return regexp.MustCompile(`\.` + regexp.QuoteMeta(name) + `\b`).MatchString(source)
}

// minimalConfig carries only the keys Parse refuses to run without, so everything it
// yields is a value the code chose, not one the operator wrote. It is the ground
// truth for "what an operator gets when the key is absent".
const minimalConfig = `
database:
  connection_string: "server=example;database=X"
directories:
  to_run:     "./01.to_run"
  processing: "./02.processing"
  done:       "./03.done"
  failed:     "./04.failed"
monitoring:
  blocking_poll_seconds: 10
  log_poll_seconds: 60
  progress_poll_seconds: 30
  log_max_size_bytes: 53687091200
  log_max_percent: 80
matrix_file: "./ddl_compatibility.yaml"
`

// requiredKeys are the keys minimalConfig must state for Parse to succeed. They have
// no default by construction, so the shipped file's value for them is an example and
// not a documented default; comparing them would compare the fixture to itself.
var requiredKeys = map[string]bool{
	"database.connection_string":       true,
	"directories.to_run":               true,
	"directories.processing":           true,
	"directories.done":                 true,
	"directories.failed":               true,
	"monitoring.blocking_poll_seconds": true,
	"monitoring.log_poll_seconds":      true,
	"monitoring.progress_poll_seconds": true,
	"monitoring.log_max_size_bytes":    true,
	"monitoring.log_max_percent":       true,
	"matrix_file":                      true,
}

// documentedDivergences are keys where the shipped config.yaml deliberately states
// something other than what the code applies when the key is absent. Each entry is a
// claim that the difference is intended and that the operator who deletes the key
// gets the stated fallback — which is the thing worth knowing and the thing nobody
// writes down. Delete an entry by giving the code the default the file advertises.
var documentedDivergences = map[string]string{
	// Intended: the default is applied by an accessor (MinBehind, After) rather than
	// by applyDefaults, so the parsed field stays zero while the behaviour matches the
	// file. Uniform defaulting would remove both entries; see docs/specs/TODO.md.
	"kill_amplifying_maintenance.min_blocked_behind": "MinBehind() returns 1 for an unset field",
	"kill_amplifying_maintenance.after_seconds":      "After() returns 60s for an unset field",
}

// TestShippedConfigStatesTheRealDefaults fails when the config.yaml an operator is
// handed by `sqlgopace init` advertises a value the code does not apply when the key
// is absent. That divergence is silent in every other way: the file reads as
// documentation, the key is optional, and deleting it changes behaviour.
//
// internal/scaffold pins the embedded twin byte-for-byte against this same file, so
// auditing the repository copy audits what ships.
func TestShippedConfigStatesTheRealDefaults(t *testing.T) {
	shippedYAML, err := os.ReadFile(filepath.Join(repoRoot, "config.yaml"))
	if err != nil {
		t.Fatalf("read shipped config.yaml: %v", err)
	}
	shipped, err := Parse(shippedYAML)
	if err != nil {
		t.Fatalf("parse shipped config.yaml: %v", err)
	}
	defaults, err := Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("parse minimal config: %v", err)
	}

	want := map[string]any{}
	for _, l := range configLeaves(defaults) {
		want[l.yamlPath] = l.value
	}

	for _, l := range configLeaves(shipped) {
		if requiredKeys[l.yamlPath] {
			continue
		}
		if sameValue(l.value, want[l.yamlPath]) {
			if reason, ok := documentedDivergences[l.yamlPath]; ok {
				t.Errorf("config key %q no longer diverges from the code default, but is still "+
					"listed as a documented divergence (%s). Delete the entry.", l.yamlPath, reason)
			}
			continue
		}
		if reason, ok := documentedDivergences[l.yamlPath]; ok {
			t.Logf("known divergence %s: %s", l.yamlPath, reason)
			continue
		}
		t.Errorf("config key %q: the shipped config.yaml says %s, the code applies %s when the "+
			"key is absent. An operator who deletes the key silently gets the second. Either give "+
			"the code the default the file advertises, or record the difference in "+
			"documentedDivergences with the reason.",
			l.yamlPath, display(l.value), display(want[l.yamlPath]))
	}
}

// sameValue compares two leaf values as configuration rather than as Go values: an
// absent list and an empty one are the same setting, and yaml.v3 produces nil for the
// first and a zero-length slice for the second.
func sameValue(a, b any) bool {
	ra, rb := reflect.ValueOf(a), reflect.ValueOf(b)
	if ra.Kind() == reflect.Slice && rb.Kind() == reflect.Slice && ra.Len() == 0 && rb.Len() == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// readViaAccessor reports whether a field is read through one of this package's
// methods that is itself called from outside.
func readViaAccessor(accessors map[string]map[string]bool, outside, field string) bool {
	for method, fields := range accessors {
		if fields[field] && selects(outside, method) {
			return true
		}
	}
	return false
}

// display renders a leaf value for a failure message, dereferencing the tri-state
// fields so the reader sees a value rather than an address. It covers both shapes:
// the bare pointer (*int, *bool) and the one-field wrapper (forceBool, forceInt).
func display(v any) string {
	rv := reflect.ValueOf(v)
	switch {
	case rv.Kind() == reflect.Pointer && rv.IsNil():
		return "unset"
	case rv.Kind() == reflect.Pointer:
		return display(rv.Elem().Interface())
	case rv.Kind() == reflect.Struct && rv.NumField() == 1:
		return display(rv.Field(0).Interface())
	}
	return fmt.Sprintf("%v", v)
}
