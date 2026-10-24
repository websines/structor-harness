package harnesseval

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/subprocess"
)

var standardCache struct {
	sync.Mutex
	root string
}

// PrepareToolchain builds only named standard-library packages in a fresh cache.
// Candidate source and private tests are never compiled into this template.
func PrepareToolchain(ctx context.Context) error {
	standardCache.Lock()
	defer standardCache.Unlock()
	if standardCache.root != "" {
		return nil
	}
	root, err := os.MkdirTemp("", "forge-stdlib-*")
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(root)
		}
	}()
	confiner, err := NewConfiner(root)
	if err != nil {
		return err
	}
	if err = confiner.Preflight(ctx); err != nil {
		return err
	}
	cache := filepath.Join(root, ".forge", "go-cache")
	if err = os.MkdirAll(cache, 0700); err != nil {
		return err
	}
	run := subprocess.New(confiner).Spawn(hx.SpawnRequest{Ctx: ctx, Argv: []string{filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "testing", "fmt", "strings", "encoding/json", "time", "path", "strconv", "sort"}, Dir: root, Env: Environment(root), TimeoutMs: 120000})
	if run.Err != "" || run.ExitCode != 0 || run.TimedOut {
		return errors.New("harnesseval: standard-library preparation failed: " + run.Err + " " + string(run.Stderr))
	}
	standardCache.root = root
	complete = true
	return nil
}

// seedToolchain copies a trusted template before any model tool can execute.
// macOS clones use copy-on-write, never hard links. Fallback copies independent
// regular files. A candidate cannot poison the template or the next workspace.
func seedToolchain(ctx context.Context, workspace, relative string) error {
	if err := PrepareToolchain(ctx); err != nil {
		return err
	}
	standardCache.Lock()
	defer standardCache.Unlock()
	source := filepath.Join(standardCache.root, ".forge", "go-cache")
	target := filepath.Join(workspace, relative)
	if !fs.ValidPath(relative) {
		return errors.New("harnesseval: invalid cache destination")
	}
	return copyCache(ctx, source, target)
}

func copyCache(ctx context.Context, source, target string) error {
	if err := os.MkdirAll(target, 0700); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		// Both arguments are host-owned fresh directories, never model strings.
		if err := exec.CommandContext(ctx, "/bin/cp", "-cR", source+"/.", target).Run(); err == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		if err := os.MkdirAll(target, 0700); err != nil {
			return err
		}
	}
	return filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		out := filepath.Join(target, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("harnesseval: unexpected toolchain cache entry")
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		file, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, in)
		return errors.Join(copyErr, file.Close())
	})
}

// CleanupToolchain removes only the process-owned template after runs join.
func CleanupToolchain() {
	standardCache.Lock()
	defer standardCache.Unlock()
	if standardCache.root != "" {
		_ = os.RemoveAll(standardCache.root)
		standardCache.root = ""
	}
}
