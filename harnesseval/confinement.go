package harnesseval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/websines/structor-harness/harness/hx"
	"github.com/websines/structor-harness/harness/sandbox"
	"github.com/websines/structor-harness/harness/subprocess"
)

// Confiner restricts reads as well as writes. The ordinary macOS Forge sandbox
// allows broad reads and therefore cannot protect private evaluation fixtures.
type Confiner struct {
	root     string
	wrapper  string
	fallback hx.SandboxProvider
}

func NewConfiner(root string) (*Confiner, error) {
	path, err := filepath.EvalSymlinks(root)
	if err != nil || !filepath.IsAbs(path) {
		return nil, errors.New("harnesseval: real absolute workspace required")
	}
	c := &Confiner{root: path}
	if runtime.GOOS == "darwin" {
		c.wrapper, err = exec.LookPath("sandbox-exec")
		if err != nil {
			return nil, err
		}
	} else if runtime.GOOS == "linux" {
		if exe, err := os.Executable(); err == nil {
			if real, err := filepath.EvalSymlinks(exe); err == nil {
				for _, root := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", runtime.GOROOT()} {
					if real == root || strings.HasPrefix(real, root+"/") {
						return nil, errors.New("harnesseval: install the fixture-bearing pilot binary outside readable runtime roots")
					}
				}
			}
		}
		c.fallback = sandbox.New()
		if !c.fallback.Available() {
			return nil, errors.New("harnesseval: confinement unavailable")
		}
	} else {
		return nil, errors.New("harnesseval: unsupported confinement host")
	}
	return c, nil
}
func (c *Confiner) Available() bool {
	return c != nil && (c.wrapper != "" || c.fallback != nil && c.fallback.Available())
}
func (c *Confiner) Confine(argv []string, p hx.SandboxPolicy) ([]string, error) {
	if !c.Available() || p.AllowNetwork {
		return nil, errors.New("harnesseval: confinement/egress refused")
	}
	policy := hx.SandboxPolicy{ReadPaths: []string{c.root, runtime.GOROOT()}, WritePaths: []string{c.root, "/dev/null"}}
	if c.fallback != nil {
		return c.fallback.Confine(argv, policy)
	}
	var b strings.Builder
	// dyld enumerates the root directory before loading /bin/sh. Literal root
	// access permits that enumeration without granting reads below it.
	b.WriteString("(version 1)(deny default)(allow process-exec)(allow process-fork)(allow sysctl-read)(allow mach-lookup)(allow file-read-metadata)(allow file-read* (literal \"/\"))")
	for _, p := range []string{"/System", "/usr", "/bin", "/sbin", "/Library/Apple", "/dev", runtime.GOROOT(), c.root} {
		b.WriteString("(allow file-read* (subpath " + strconv.Quote(p) + "))")
	}
	b.WriteString("(allow file-write* (subpath " + strconv.Quote(c.root) + "))(allow file-write* (literal \"/dev/null\"))")
	// An installed pilot binary embeds the private fixture catalog. Even when
	// installed under a readable OS runtime root, child tools must not inspect it.
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			b.WriteString("(deny file-read* (literal " + strconv.Quote(real) + "))")
		}
	}
	return append([]string{c.wrapper, "-p", b.String()}, argv...), nil
}

// Preflight proves this mechanism executes a known-good command and refuses a
// specific private canary. Merely finding a wrapper binary is not sufficient.
func (c *Confiner) Preflight(ctx context.Context) error {
	private, err := os.CreateTemp("", "forge-grader-canary-*")
	if err != nil {
		return err
	}
	defer os.Remove(private.Name())
	private.WriteString("private-evaluation-canary")
	private.Close()
	probe := subprocess.New(c)
	allowed := probe.Spawn(hx.SpawnRequest{Ctx: ctx, Argv: []string{"/bin/sh", "-c", "printf canary"}, Dir: c.root, Env: Environment(c.root), TimeoutMs: 5000})
	if allowed.Err != "" || allowed.ExitCode != 0 || string(allowed.Stdout) != "canary" {
		return fmt.Errorf("harnesseval: execution canary failed: %+v", allowed)
	}
	denied := probe.Spawn(hx.SpawnRequest{Ctx: ctx, Argv: []string{"/bin/cat", private.Name()}, Dir: c.root, Env: Environment(c.root), TimeoutMs: 5000})
	if denied.Err != "" || denied.ExitCode == 0 || strings.Contains(string(denied.Stdout), "private-evaluation-canary") {
		return errors.New("harnesseval: private grader boundary not enforced")
	}
	return nil
}

// Environment is an allowlist: no model keys, login profiles or host Go config.
func Environment(root string) []string {
	// Go records local telemetry and other runtime state under HOME. Keep that
	// data in the reserved runtime directory, outside captured source artifacts.
	return []string{"PATH=" + filepath.Join(runtime.GOROOT(), "bin") + ":/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + filepath.Join(root, ".forge", "home"), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOENV=off", "CGO_ENABLED=0", "GO111MODULE=off", "GOMAXPROCS=2", "GOCACHE=" + filepath.Join(root, ".forge", "go-cache"), "GOTMPDIR=" + root, "TMPDIR=" + root}
}
