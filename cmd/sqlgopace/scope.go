package main

import (
	"strings"

	"github.com/rudi-bruchez/SqlGoPace/internal/maint"
	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// dbSkip records a database left out of a multi-database run, with why — so the
// orchestrator can log it (spec §17.3/§17.4: skip + log, never silent for a
// database that cannot be maintained).
type dbSkip struct {
	name   string
	reason string
}

// resolveDatabases turns the eligibility sweep and the scope selection into the
// ordered list of databases to maintain plus the ones skipped (with reasons), per
// spec §17.4. Precedence:
//   - an explicit --databases list wins (profile scope is ignored, eligibility is
//     still enforced; an unknown or ineligible name is skipped with a reason);
//   - else --all-databases ∩ the profile scope (scope-excluded databases are
//     dropped silently — that is intentional, not a skip-worthy surprise);
//   - else the connected database only (today's single-database behaviour).
//
// `all` is the eligibility sweep (mssql.UserDatabases); `connected` is the
// connection's current database. The function is pure and table-tested.
func resolveDatabases(all []mssql.DatabaseInfo, allFlag bool, explicit []string, connected string, p *maint.Profile) (selected []string, skipped []dbSkip) {
	if len(explicit) > 0 {
		byName := make(map[string]mssql.DatabaseInfo, len(all))
		for _, d := range all {
			byName[strings.ToLower(d.Name)] = d
		}
		for _, raw := range explicit {
			name := strings.TrimSpace(raw)
			if name == "" {
				continue
			}
			switch d, ok := byName[strings.ToLower(name)]; {
			case !ok:
				skipped = append(skipped, dbSkip{name, "not found on server (or not a user database)"})
			case !d.Eligible:
				skipped = append(skipped, dbSkip{d.Name, d.IneligibleReason})
			default:
				selected = append(selected, d.Name)
			}
		}
		return selected, skipped
	}

	if allFlag {
		for _, d := range all {
			if !p.ScopeIncludes(d.Name) {
				continue // excluded by profile scope — intentional, silent
			}
			if !d.Eligible {
				skipped = append(skipped, dbSkip{d.Name, d.IneligibleReason})
				continue
			}
			selected = append(selected, d.Name)
		}
		return selected, skipped
	}

	return []string{connected}, nil
}

// runnableTargets splits the queued target databases into those to run now and
// those to skip because they are no longer eligible — e.g. an AG failover or a
// SET READ_ONLY between `plan` and the run (spec §17.6). The connected database is
// always runnable (we hold a usable connection to it); every other target is
// checked against the current eligibility sweep. A target absent from `eligible`
// (not a user database) is left to run so the engine surfaces any real error.
func runnableTargets(targets []string, connected string, eligible []mssql.DatabaseInfo) (run []string, skip []dbSkip) {
	byName := make(map[string]mssql.DatabaseInfo, len(eligible))
	for _, d := range eligible {
		byName[strings.ToLower(d.Name)] = d
	}
	for _, db := range targets {
		if strings.EqualFold(db, connected) {
			run = append(run, db)
			continue
		}
		if d, ok := byName[strings.ToLower(db)]; ok && !d.Eligible {
			skip = append(skip, dbSkip{db, "no longer eligible: " + d.IneligibleReason})
			continue
		}
		run = append(run, db)
	}
	return run, skip
}

// needsEligibilityRecheck reports whether any target is a database other than the
// connected one, so the run must survey current eligibility before processing.
func needsEligibilityRecheck(targets []string, connected string) bool {
	for _, db := range targets {
		if !strings.EqualFold(db, connected) {
			return true
		}
	}
	return false
}

// splitDatabases parses a comma-separated --databases value into trimmed names.
func splitDatabases(csv string) []string {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(csv, ",") {
		if name := strings.TrimSpace(raw); name != "" {
			out = append(out, name)
		}
	}
	return out
}
