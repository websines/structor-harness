package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Public tool names follow the harness function-name shape: at most 64 chars
// drawn from [A-Za-z0-9_-]. These are wire constants, not configuration.
const (
	maxToolNameLen = 64
	hashSuffixLen  = 12
)

// publicToolName derives the model-facing name for one MCP tool. The clean case
// is mcp__<server>__<rawName>, normalized to the function-name shape. When that
// normalized name is ALREADY taken in this generation — two distinct raw names
// collapsing onto one public name, or two truncations meeting — a "_" plus the
// first 12 hex of sha256(rawName) is appended so distinct tools never share a
// name. taken records the names already assigned in the generation being built;
// the caller adds the returned name to it.
func publicToolName(server, raw string, taken map[string]struct{}) string {
	name := normalizeToolName("mcp__" + server + "__" + raw)
	if _, dup := taken[name]; !dup {
		return name
	}
	return withHashSuffix(name, raw)
}

// normalizeToolName maps any character outside [A-Za-z0-9_-] to '_' and truncates
// to the length budget. The output is pure ASCII, so the byte truncation never
// splits a rune.
func normalizeToolName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > maxToolNameLen {
		out = out[:maxToolNameLen]
	}
	return out
}

// withHashSuffix appends "_" + first 12 hex of sha256(rawName), trimming the base
// as needed to keep the total within the length budget. The suffix is a pure
// function of rawName, so a given tool's disambiguated name is deterministic.
func withHashSuffix(name, raw string) string {
	sum := sha256.Sum256([]byte(raw))
	suffix := "_" + hex.EncodeToString(sum[:])[:hashSuffixLen]
	if len(name)+len(suffix) > maxToolNameLen {
		name = name[:maxToolNameLen-len(suffix)]
	}
	return name + suffix
}
