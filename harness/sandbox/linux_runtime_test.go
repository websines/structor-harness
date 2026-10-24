package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/websines/structor-harness/harness/hx"
)

func TestLinuxRuntimeMergedUsrAliasesAndCacheAreReadOnly(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"usr/bin", "usr/sbin", "usr/lib", "usr/lib64", "etc"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{"bin": "usr/bin", "sbin": "usr/sbin", "lib": "usr/lib", "lib64": "usr/lib64"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/ld.so.cache"), []byte("loader fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	bindings, err := linuxRuntimeBindings(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--ro-bind", filepath.Join(root, "usr"), "/usr", "--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin", "--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64", "--ro-bind", filepath.Join(root, "etc/ld.so.cache"), "/etc/ld.so.cache"}
	if !reflect.DeepEqual(bindings, want) {
		t.Fatalf("runtime alias/mount order changed: %v", bindings)
	}
}

func TestLinuxRuntimeRefusesAliasesOutsideFixedRoots(t *testing.T) {
	for name, target := range map[string]string{"host credentials": "/home/operator", "parent escape": "../root", "self cycle": "/bin"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Symlink(target, filepath.Join(root, "bin")); err != nil {
				t.Fatal(err)
			}
			if _, err := linuxRuntimeBindings(root); err == nil {
				t.Fatal("a runtime alias inherited a private host root")
			}
		})
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../home/private", filepath.Join(root, "etc/ld.so.cache")); err != nil {
		t.Fatal(err)
	}
	if _, err := linuxRuntimeBindings(root); err == nil {
		t.Fatal("dynamic loader cache followed an unrelated host path")
	}
}

func TestLinuxConfinePreservesIsolationAndExplicitMountPrecedence(t *testing.T) {
	policy := hx.SandboxPolicy{ReadPaths: []string{"/workspace", "/reference", "/dev/null"}, WritePaths: []string{"/workspace", "/dev/null"}}
	argv, err := linuxConfine("/usr/bin/bwrap", []string{"bash", "-c", "true"}, policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--unshare-pid", "--proc", "--dev", "--new-session", "--die-with-parent", "--unshare-net"} {
		if !slices.Contains(argv, flag) {
			t.Fatalf("Linux confinement lost isolation: %s", flag)
		}
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "--bind /dev/null") || strings.Contains(joined, "--ro-bind /dev/null") {
		t.Fatalf("host null device replaced the private runtime device: %v", argv)
	}
	read := strings.LastIndex(joined, "--ro-bind /workspace /workspace")
	write := strings.LastIndex(joined, "--bind /workspace /workspace")
	if read < 0 || write <= read || !strings.Contains(joined, "--ro-bind /reference /reference") {
		t.Fatalf("explicit writable workspace did not override its read-only mount: %v", argv)
	}
	for i, flag := range argv {
		if flag == "--bind" && (i+2 >= len(argv) || argv[i+1] != "/workspace" || argv[i+2] != "/workspace") {
			t.Fatalf("unrequested host write was added: %v", argv)
		}
		if flag == "--ro-bind" && i+1 < len(argv) && slices.Contains([]string{"/", "/home", "/root", "/etc", "/tmp"}, argv[i+1]) {
			t.Fatalf("unrequested private host root was exposed: %v", argv)
		}
	}
	policy.AllowNetwork = true
	open, err := linuxConfine("/usr/bin/bwrap", []string{"true"}, policy)
	if err != nil || slices.Contains(open, "--unshare-net") || !slices.Contains(open, "--unshare-pid") {
		t.Fatalf("declared egress changed filesystem/process isolation: %v, %v", open, err)
	}
}
