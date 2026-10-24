package improve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/websines/structor-harness/record"
)

// Journal retains immutable stage objects plus exclusive, fsynced references.
// The experiment owner must hold a process lock before dispatching paid work.
type Journal struct {
	mu  sync.Mutex
	dir string
	cas record.CAS
}
type Entry struct {
	Schema int             `json:"schema"`
	Kind   string          `json:"kind"`
	Body   json.RawMessage `json:"body"`
}

func NewJournal(dir string, cas record.CAS) (*Journal, error) {
	if !filepath.IsAbs(dir) || cas == nil {
		return nil, errors.New("improve: private absolute journal required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("improve: journal must be a private real directory")
	}
	return &Journal{dir: dir, cas: cas}, nil
}

func (j *Journal) Record(ctx context.Context, id string, value any) (record.Digest, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !namePattern.MatchString(id) {
		return "", errors.New("improve: invalid journal key")
	}
	body, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(body) > 1<<20 {
		return "", errors.New("improve: record exceeds limit")
	}
	data, err := json.Marshal(Entry{Schema: Schema, Kind: id, Body: body})
	if err != nil {
		return "", err
	}
	d, err := j.cas.Put(data)
	if err != nil {
		return "", err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	root, err := os.OpenRoot(j.dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	f, err := root.OpenFile(id+".ref", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		old, readErr := root.ReadFile(id + ".ref")
		if readErr == nil && string(old) == string(d) {
			return d, nil
		}
		return "", errors.New("improve: original operation already has different bytes")
	}
	if err != nil {
		return "", err
	}
	if _, err = f.WriteString(string(d)); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return "", err
	}
	dir, err := os.Open(j.dir)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	return d, dir.Sync()
}

func (j *Journal) Read(id string, value any) (record.Digest, error) {
	if !namePattern.MatchString(id) {
		return "", errors.New("improve: invalid journal key")
	}
	root, err := os.OpenRoot(j.dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	ref, err := root.ReadFile(id + ".ref")
	if err != nil {
		return "", err
	}
	d := record.Digest(ref)
	data, err := j.cas.Get(d)
	if err != nil {
		return "", err
	}
	var e Entry
	if err := Decode(data, &e); err != nil {
		return "", err
	}
	if e.Schema != Schema || e.Kind != id {
		return "", fmt.Errorf("improve: journal binding mismatch: %s", id)
	}
	return d, Decode(e.Body, value)
}
