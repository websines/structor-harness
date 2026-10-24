// Package skillstore makes the RECORD the skill store: a skill activates only
// when an admission (promotion) fact exists for it AND it is in scope, never
// because a file sits in a directory (v0.1 design §3). It reuses the learning
// store's acceptance-gated promotion + supersession and the harness/skill
// registry rendering, so an admitted skill is activated through the exact same
// catalog/tool surface a filesystem skill would be — the only difference is the
// gate. Import brings filesystem skills in as SOURCES (candidates), promoting
// them only under an explicit, recorded trust decision.
package skillstore

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// idPrefix namespaces a skill's logical id in the record. A skill id is
// "skill:" + name, optionally suffixed with "#" + a content-address of the body
// for a specific version (see VersionID). These three helpers are the single
// id↔name↔version convention shared by skillstore and skillsmith, so both
// packages mint and read ids identically.
const idPrefix = "skill:"

// SkillID is the record id for a skill named name — the stable, versionless id
// under which a skill is first authored/imported ("skill:foo").
func SkillID(name string) string { return idPrefix + name }

// SkillName recovers a skill's name from any of its ids: it strips the "skill:"
// prefix, then cuts at the first "#", so both the base id ("skill:foo") and a
// versioned id ("skill:foo#ab12cd34") map back to "foo".
func SkillName(id string) string {
	name := strings.TrimPrefix(id, idPrefix)
	if i := strings.IndexByte(name, '#'); i >= 0 {
		name = name[:i]
	}
	return name
}

// VersionID is a content-addressed id for a specific body of a named skill:
// "skill:" + name + "#" + the first 8 hex of sha256(body). An improved body
// therefore gets a DISTINCT fact id (a new version), while SkillName still
// recovers the same name — this is how improve_skill supersedes rather than
// edits in place.
func VersionID(name, body string) string {
	sum := sha256.Sum256([]byte(body))
	return idPrefix + name + "#" + hex.EncodeToString(sum[:])[:8]
}
