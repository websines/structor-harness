// Package skill is a Go port of dsh's skill system: a named bundle of
// INSTRUCTIONS (distinct from a tool, which executes) that the model — or the
// user — pulls in on demand.
//
// It keeps dsh's registry/provider split: a Registry holds discovered skills and
// renders the model-facing surface, while a provider (here DiscoverFilesystem)
// supplies the sources. Discovery walks ranked roots in project→user→shared
// precedence (the "user skill libraries" idea — a skill declared nearer the
// project shadows a same-named one from a user library), the nearest rank winning
// a duplicate name.
//
// Skills live either as a directory bundle (<name>/SKILL.md) or a flat file
// (<name>.md), with kebab-case names and a leading `---`/`---` YAML-ish
// frontmatter block (name, description, whenToUse, disable-model-invocation,
// user-invocable). The catalog and the body have two lifecycles: the CHEAP
// CATALOG (name + description only) is always present, and the LAZY BODY is
// re-read and reparsed from disk on every Load, so a skill's instructions can be
// edited without any cache or notification.
//
// Invocation is a two-axis policy. A model-invocable skill appears in the catalog
// and can be loaded through the `skill` tool; disable-model-invocation removes it
// from both. A user-invocable skill can be pulled in by a whitespace-bounded
// `/name` token in a user message — the sole entry point for skills the model may
// never invoke itself.
package skill
