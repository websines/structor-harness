//go:build linux

package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/websines/structor-harness/harness/hx"
)

func TestLinuxSandboxLiveBoundary(t *testing.T) {
	sandbox := New()
	if !sandbox.Available() {
		t.Skip("no Linux confinement tool installed; CI separately requires bubblewrap")
	}
	workspace, reference, outside := t.TempDir(), t.TempDir(), t.TempDir()
	readOnly := filepath.Join(reference, "reference.txt")
	secret := filepath.Join(outside, "private.txt")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", home)
	homeFixture := t.TempDir()
	homeSecret := filepath.Join(homeFixture, "private.txt")
	for path, body := range map[string]string{readOnly: "read-only guidance", secret: "private host text", homeSecret: "private home text"} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Real subprocess policies always include /dev/null. It must keep using the
	// private device created by --dev, rather than remounting the host device.
	policy := hx.SandboxPolicy{ReadPaths: []string{reference, "/dev/null"}, WritePaths: []string{workspace, "/dev/null"}}
	run := func(policy hx.SandboxPolicy, script string, args ...string) ([]byte, error) {
		t.Helper()
		argv, err := sandbox.Confine(append([]string{"bash", "-c", script, "forge-sandbox-test"}, args...), policy)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		//nolint:gosec // Deliberately execute Confine's real wrapper and fixed test scripts to verify the OS boundary.
		command := exec.CommandContext(ctx, argv[0], argv[1:]...)
		command.Dir = workspace
		return command.CombinedOutput()
	}
	// An honest command must first execute through the real runtime; otherwise
	// failed writes/network connects would prove only an unusable sandbox.
	output, err := run(policy, `test -x /bin/sh && test -r "$1" && test ! -r "$2" && test ! -r "$3" && test ! -e /root/.ssh && printf saved > inside.txt && : > /dev/null && /bin/sh -c 'cat /dev/null >/dev/null 2>/dev/null' && git --version >/dev/null`, readOnly, secret, homeSecret)
	if err != nil {
		t.Fatalf("real Linux confinement cannot execute/read/write its declared scope: %v, %s", err, output)
	}
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer workspaceRoot.Close()
	if body, err := workspaceRoot.ReadFile("inside.txt"); err != nil || string(body) != "saved" {
		t.Fatalf("sandbox reported success without its actual declared write: %q, %v", body, err)
	}
	output, err = run(policy, `printf changed > "$1"`, readOnly)
	if err == nil {
		t.Fatalf("read-only declared source was writable: %s", output)
	}
	referenceRoot, err := os.OpenRoot(reference)
	if err != nil {
		t.Fatal(err)
	}
	defer referenceRoot.Close()
	if body, err := referenceRoot.ReadFile("reference.txt"); err != nil || string(body) != "read-only guidance" {
		t.Fatalf("real Linux sandbox changed read-only host data: %q, %v", body, err)
	}
	escape := filepath.Join(outside, "escape.txt")
	output, err = run(policy, `printf escaped > "$1"`, escape)
	if err == nil {
		t.Fatalf("undeclared host directory was writable: %s", output)
	}
	if _, err := os.Stat(escape); !os.IsNotExist(err) {
		t.Fatalf("undeclared host write escaped confinement: %v", err)
	}
	// CI provisions a host-writable marker in its exact Go runtime, so a failed
	// write proves the read-only mount rather than ordinary Unix permissions.
	if configuredProbe := os.Getenv("FORGE_SANDBOX_RUNTIME_PROBE"); configuredProbe != "" {
		const probe = "/usr/local/forge-ci-go/.forge-ci-readonly-probe"
		if configuredProbe != probe {
			t.Fatal("unexpected CI toolchain runtime probe")
		}
		info, err := os.Lstat(probe)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0666 {
			t.Fatalf("CI runtime probe is not a host-writable regular file: %v", err)
		}
		output, err = run(policy, `test -r "$1" && printf changed > "$1"`, probe)
		if err == nil {
			t.Fatalf("read-only OS/toolchain runtime was writable: %s", output)
		}
		toolchainRoot, err := os.OpenRoot(filepath.Dir(probe))
		if err != nil {
			t.Fatal(err)
		}
		defer toolchainRoot.Close()
		body, err := toolchainRoot.ReadFile(filepath.Base(probe))
		if err != nil || string(body) != "read-only toolchain\n" {
			t.Fatalf("sandbox changed its host toolchain runtime: %q, %v", body, err)
		}
	}
	hostNamespace, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	output, err = run(policy, `readlink /proc/self/ns/pid`)
	if err != nil || strings.TrimSpace(string(output)) == hostNamespace || !strings.HasPrefix(strings.TrimSpace(string(output)), "pid:[") {
		t.Fatalf("/proc exposes the host PID namespace: %q, %v", output, err)
	}
	// Use shell built-ins: distro awk aliases can resolve through host
	// /etc/alternatives, which deliberately is not a readable runtime mount.
	output, err = run(policy, `found=0; while IFS=: read -r name value; do case "$name" in CapEff|CapPrm|CapAmb) [[ "$value" =~ ^[[:space:]]*0000000000000000$ ]] || exit 1; ((++found));; esac; done < /proc/self/status; test "$found" = 3`)
	if err != nil {
		t.Fatalf("the confined program retained sandbox-setup capabilities: %v, %s", err, output)
	}
	var listenerConfig net.ListenConfig
	listener, err := listenerConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.Close()
		}
	}()
	defer func() { _ = listener.Close(); <-joined }()
	port := strings.TrimPrefix(listener.Addr().String(), "127.0.0.1:")
	output, err = run(policy, `exec 3<>/dev/tcp/127.0.0.1/"$1"`, port)
	if err == nil {
		t.Fatalf("undeclared network access reached the host listener: %s", output)
	}
	policy.AllowNetwork = true
	output, err = run(policy, `exec 3<>/dev/tcp/127.0.0.1/"$1"`, port)
	if err != nil {
		t.Fatalf("declared network access could not reach the same host listener: %v, %s", err, output)
	}
	t.Log("real Linux runtime/read-only input/private host/PID isolation/write scope/default-deny and declared network boundaries passed")
}
