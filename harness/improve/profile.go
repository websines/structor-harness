// Package improve runs bounded harness experiments. It deliberately has no
// access to private graders, task admission, or shared-guidance publication.
package improve

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/websines/structor-harness/record"
)

const Schema = 1
const Upstream = "1cfe254e8d25b15f37048c3730134686abe60335"

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

// Profile contains only editable guidance. Base identifies protected runtime
// configuration captured by the caller, which model output cannot replace.
type Profile struct {
	Schema       int           `json:"schema"`
	Base         record.Digest `json:"base"`
	Guidance     string        `json:"guidance"`
	ToolGuidance string        `json:"toolGuidance"`
	Skills       []Skill       `json:"skills"`
}

func (p Profile) Validate(base record.Digest) error {
	if p.Schema != Schema || p.Base != base || !ValidDigest(base) {
		return errors.New("improve: profile base/schema mismatch")
	}
	if !utf8.ValidString(p.Guidance+p.ToolGuidance) || len(p.Guidance)+len(p.ToolGuidance) > 8192 || len(p.Skills) > 4 {
		return errors.New("improve: guidance exceeds limits")
	}
	seen := make(map[string]bool)
	for _, s := range p.Skills {
		if !namePattern.MatchString(s.Name) || seen[s.Name] || strings.TrimSpace(s.Description) == "" || len(s.Description) > 256 || strings.ContainsAny(s.Description, "\r\n") || strings.TrimSpace(s.Body) == "" || len(s.Body) > 8192 || !utf8.ValidString(s.Description+s.Body) {
			return fmt.Errorf("improve: invalid skill %q", s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}

func ValidDigest(d record.Digest) bool {
	s := strings.TrimPrefix(string(d), "sha256:")
	return len(s) == 64 && len(d) == 71 && strings.IndexFunc(s, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) < 0
}

func Digest(v any) (record.Digest, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return record.ComputeDigest(b), nil
}

// Decode rejects undeclared authority/configuration fields and trailing objects.
func Decode(data []byte, value any) error {
	if len(data) > 1<<20 {
		return errors.New("improve: JSON exceeds limit")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("improve: trailing JSON")
	}
	return nil
}

func ParseProfile(data []byte, base record.Digest) (Profile, error) {
	var p Profile
	if err := Decode(data, &p); err != nil {
		return p, err
	}
	return p, p.Validate(base)
}
