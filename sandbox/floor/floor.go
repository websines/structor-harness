// Package floor holds the single definition of the HARD-DENY policy (system
// design §4.6) — the rules, and nothing else.
//
// It is a LEAF: it imports only the standard library, so ANY enforcement point
// can apply the floor without an import cycle. That is why it exists as its own
// package rather than living in internal/sandbox with the driver: internal/sandbox
// imports internal/harness/subprocess (it composes the real spawner), so the
// spawner — an enforcement point in its own right, since it is the last code that
// runs before a model-chosen command becomes a process — could not have imported
// the rules back. Splitting the rules out keeps ONE floor with no second copy to
// drift; internal/sandbox re-exports these names unchanged (see
// internal/sandbox/harddeny.go), so every existing caller is untouched.
package floor

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The HARD-DENY policy (system design §4.6). Postures govern friction; the hard
// policy governs damage — so these rules are enforced UNCONDITIONALLY, in every
// posture including `open`. Run applies HardDeny before it confines or spawns, and
// there is no knob to disable it. The rule set below is deliberately EXPLICIT,
// CONSERVATIVE (it errs toward refusal), and unit-tested with an allow/deny table.
//
// It is a pure function of its inputs: it never touches the filesystem or a
// process, it only inspects a proposed argv (and optional SQL text) and returns a
// typed *DenyError describing what it refused and why. It never panics.

// DenyCategory names the hard-deny class a command tripped.
type DenyCategory string

const (
	// DenyDestructiveSQL: a statement that destroys schema or mass-mutates data —
	// DROP/TRUNCATE of an object, or a DELETE/UPDATE with no WHERE clause.
	DenyDestructiveSQL DenyCategory = "destructive-sql"
	// DenyRecursiveDelete: a recursive/tree delete — `rm -r`/`rm -rf`, `find -delete`.
	DenyRecursiveDelete DenyCategory = "recursive-delete"
	// DenyCredentialAccess: any reference to a known credential/secret path —
	// cloud creds, SSH private keys, .env files, netrc, kube/docker configs.
	DenyCredentialAccess DenyCategory = "credential-access" //nolint:gosec // G101 false positive: this is a denylist CATEGORY NAME, not a credential
)

// DenyError is the typed refusal the hard-deny policy returns. It is an error, so
// callers use errors.As to recover the Category/Match without string matching.
type DenyError struct {
	Category DenyCategory // which hard-deny class was hit
	Match    string       // the offending token or fragment (for the record/audit)
	Reason   string       // human-readable why
}

func (e *DenyError) Error() string {
	return fmt.Sprintf("sandbox: hard-deny [%s]: %s (matched %q)", e.Category, e.Reason, e.Match)
}

// HardDeny inspects a proposed command (argv) and optional SQL text and returns a
// *DenyError if it trips a hard-deny category, or nil to allow. Evaluation order is
// fixed and documented so the refusal is deterministic: recursive-delete, then
// credential-access, then destructive-SQL (the sql argument first, then any inline
// SQL carried as an argv token, e.g. `psql -c "DROP TABLE t"`).
//
// The sql argument is optional context for callers that already have the statement
// text (a DB collector); pass "" when there is none. Conservative by construction:
// when a heuristic is uncertain (e.g. a DELETE whose WHERE is on another statement),
// it denies.
func HardDeny(argv []string, sql string) error {
	if d := recursiveDelete(argv); d != nil {
		return d
	}
	if d := credentialAccess(argv); d != nil {
		return d
	}
	if d := destructiveSQL(sql); d != nil {
		return d
	}
	for _, tok := range argv {
		if d := destructiveSQL(tok); d != nil {
			return d
		}
	}
	return nil
}

// --- recursive delete ---------------------------------------------------------

// recursiveDelete denies a tree-delete. For `rm` it denies any recursive flag
// (-r/-R/--recursive), combined or separate, with or without force — §4.6 names
// "recursive deletes", so a non-recursive `rm -f file` is allowed. For `find` it
// denies the `-delete` action. The command is matched by basename so an absolute
// path (/bin/rm) is caught too.
func recursiveDelete(argv []string) *DenyError {
	if len(argv) == 0 {
		return nil
	}
	switch filepath.Base(argv[0]) {
	case "rm":
		for _, a := range argv[1:] {
			if a == "--recursive" || strings.HasPrefix(a, "--recursive=") {
				return &DenyError{DenyRecursiveDelete, a, "recursive delete (rm --recursive)"}
			}
			// A combined short-flag bundle like -rf / -fr / -Rf. Ignore long flags
			// and bare "-" / "--" (the end-of-options marker).
			if len(a) >= 2 && a[0] == '-' && a[1] != '-' {
				if strings.ContainsAny(a[1:], "rR") {
					return &DenyError{DenyRecursiveDelete, a, "recursive delete (rm -r)"}
				}
			}
		}
	case "find":
		if slices.Contains(argv[1:], "-delete") {
			return &DenyError{DenyRecursiveDelete, "find -delete", "recursive delete (find -delete)"}
		}
	}
	return nil
}

// --- credential access --------------------------------------------------------

// credFragments are substrings that, appearing in any argv token, denote a
// well-known credential or secret path. Conservative: presence anywhere in the
// token denies, whether the command reads or writes it (§4.6 says "credential
// access", not "credential read"). A leading "~" or an absolute prefix does not
// change the match because these fragments are the tail of the path.
var credFragments = []string{
	".aws/credentials",    // AWS static creds
	".git-credentials",    // git stored creds
	".netrc",              // machine login tokens
	".docker/config.json", // registry auth
	".kube/config",        // cluster credentials
	"id_rsa",              // SSH private keys
	"id_dsa",
	"id_ecdsa",
	"id_ed25519",
}

// credentialAccess denies any argv token that references a credential path: one of
// credFragments, anything under an .ssh directory, or a .env / .env.* file (matched
// as a path component so "readme.env.md" and "development" do NOT trip it).
func credentialAccess(argv []string) *DenyError {
	for _, tok := range argv {
		for _, frag := range credFragments {
			if strings.Contains(tok, frag) {
				return &DenyError{DenyCredentialAccess, frag, "reference to a credential path"}
			}
		}
		if strings.Contains(tok, ".ssh/") || strings.HasSuffix(tok, "/.ssh") || tok == ".ssh" {
			return &DenyError{DenyCredentialAccess, ".ssh", "reference to an SSH key directory"}
		}
		for comp := range strings.SplitSeq(tok, "/") {
			if comp == ".env" || strings.HasPrefix(comp, ".env.") {
				return &DenyError{DenyCredentialAccess, comp, "reference to a .env secret file"}
			}
		}
	}
	return nil
}

// --- destructive SQL ----------------------------------------------------------

var (
	// DROP of a database object (not e.g. "DROP CONSTRAINT" alone, but the common
	// destructive targets).
	reDropObject = regexp.MustCompile(`\bDROP\s+(DATABASE|SCHEMA|TABLE|INDEX|VIEW|SEQUENCE|ROLE|USER|TRIGGER|FUNCTION|MATERIALIZED\s+VIEW)\b`)
	reDeleteFrom = regexp.MustCompile(`\bDELETE\s+FROM\b`)
	reUpdateSet  = regexp.MustCompile(`\bUPDATE\b.*\bSET\b`)
)

// destructiveSQL denies schema-destroying or unscoped-mutation SQL in text. It
// normalizes case and whitespace onto a single padded line, then checks, in order:
// DROP <object>, TRUNCATE, DELETE FROM without a WHERE, UPDATE ... SET without a
// WHERE. The WHERE test is whole-text and conservative: a DELETE/UPDATE is denied
// unless a WHERE appears somewhere in the text, so an unscoped statement cannot be
// hidden behind a scoped sibling.
func destructiveSQL(text string) *DenyError {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	norm := " " + strings.ToUpper(strings.Join(strings.Fields(text), " ")) + " "
	if m := reDropObject.FindString(norm); m != "" {
		return &DenyError{DenyDestructiveSQL, strings.TrimSpace(m), "DROP of a database object"}
	}
	if strings.Contains(norm, " TRUNCATE ") {
		return &DenyError{DenyDestructiveSQL, "TRUNCATE", "TRUNCATE removes every row"}
	}
	hasWhere := strings.Contains(norm, " WHERE ")
	if reDeleteFrom.MatchString(norm) && !hasWhere {
		return &DenyError{DenyDestructiveSQL, "DELETE FROM", "DELETE without a WHERE clause"}
	}
	if reUpdateSet.MatchString(norm) && !hasWhere {
		return &DenyError{DenyDestructiveSQL, "UPDATE ... SET", "UPDATE without a WHERE clause"}
	}
	return nil
}
