// Package harnesseval owns private behavioral evaluation. Only public task and
// execution evidence crosses into the improvement loop.
package harnesseval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/websines/structor-harness/record"
)

type Source map[string]string

func (s Source) Validate() error {
	if len(s) == 0 || len(s) > 128 {
		return errors.New("harnesseval: source file count")
	}
	n := 0
	for p, text := range s {
		if !fs.ValidPath(p) || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, ".forge/") || strings.HasPrefix(p, ".git/") {
			return fmt.Errorf("harnesseval: unsafe source path %q", p)
		}
		if len(text) > 1<<20 || !utf8.ValidString(text) {
			return fmt.Errorf("harnesseval: oversized or non-UTF-8 source %q", p)
		}
		n += len(text)
	}
	if n > 4<<20 {
		return errors.New("harnesseval: source exceeds limit")
	}
	return nil
}
func (s Source) Store(cas record.CAS) (record.Digest, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return cas.Put(b)
}
func LoadSource(cas record.CAS, d record.Digest) (Source, error) {
	b, err := cas.Get(d)
	if err != nil {
		return nil, err
	}
	var source Source
	if len(b) > 8<<20 {
		return nil, errors.New("harnesseval: source encoding exceeds limit")
	}
	if err := json.Unmarshal(b, &source); err != nil {
		return nil, err
	}
	return source, source.Validate()
}
func (s Source) Materialize(dir string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for name, text := range s {
		if err := root.MkdirAll(filepath.Dir(name), 0700); err != nil {
			return err
		}
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := f.WriteString(text)
		if err := errors.Join(writeErr, f.Close()); err != nil {
			return err
		}
	}
	return nil
}
func Capture(dir string) (Source, error) {
	s := make(Source)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel == ".forge" || rel == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return errors.New("harnesseval: output contains special/oversized file")
		}
		if len(s) >= 128 {
			return errors.New("harnesseval: too many output files")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		s[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s, s.Validate()
}
