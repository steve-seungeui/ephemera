package storage

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// These tests require root (loop mount) and mkfs.ext4.
// In CI they are skipped automatically; run them on a host with /dev/kvm.

func makeTestDisk(t *testing.T, dir string) string {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("loop mount requires root")
	}
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not in PATH")
	}

	img := filepath.Join(dir, "disk.ext4")
	f, err := os.Create(img)
	if err != nil {
		t.Fatalf("create disk: %v", err)
	}
	if err := f.Truncate(8 << 20); err != nil { // 8 MiB minimum for ext4
		f.Close()
		t.Fatalf("truncate: %v", err)
	}
	f.Close()
	if out, err := exec.Command("mkfs.ext4", "-F", "-q", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v — %s", err, out)
	}
	return img
}

func prepareTestVM(t *testing.T, tmp, workspaceDir string, opts VMPrepareOptions) {
	t.Helper()
	diskImg := makeTestDisk(t, tmp)
	vmID := "test-vm"
	destPath := filepath.Join(workspaceDir, vmID+".ext4")
	data, _ := os.ReadFile(diskImg)
	if err := os.WriteFile(destPath, data, 0644); err != nil {
		t.Fatalf("write disk clone: %v", err)
	}

	p := &Provisioner{WorkspaceDir: workspaceDir}
	if err := p.PrepareVM(vmID, opts); err != nil {
		t.Fatalf("PrepareVM: %v", err)
	}

	// Mount and run the verification callback, then unmount.
	mntDir := filepath.Join(tmp, "mnt")
	os.MkdirAll(mntDir, 0755)
	t.Cleanup(func() {
		exec.Command("umount", "-l", mntDir).Run()
		os.Remove(mntDir)
	})
	if out, err := exec.Command("mount", "-o", "loop", destPath, mntDir).CombinedOutput(); err != nil {
		t.Fatalf("mount: %v — %s", err, out)
	}
	t.Setenv("_TEST_MNT", mntDir) // pass mount point to caller via env (test-only convention)
}

func TestPrepareVM_WritesAgentToken(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	os.MkdirAll(ws, 0755)

	configPath := filepath.Join(tmp, "goose.yaml")
	secretsPath := filepath.Join(tmp, "goose-secrets.yaml")
	os.WriteFile(configPath, []byte("GOOSE_PROVIDER: test\n"), 0644)
	os.WriteFile(secretsPath, []byte("TEST_KEY: x\n"), 0644)

	const token = "deadbeefcafe1234"
	prepareTestVM(t, tmp, ws, VMPrepareOptions{
		HostConfigPath:  configPath,
		HostSecretsPath: secretsPath,
		AgentToken:      token,
	})

	mntDir := os.Getenv("_TEST_MNT")
	tokenFile := filepath.Join(mntDir, "root", ".ephemera-agent-token")

	info, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatalf("token file not found: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("expected mode 0600, got %04o", perm)
	}
	content, _ := os.ReadFile(tokenFile)
	if string(content) != token {
		t.Errorf("expected token %q, got %q", token, string(content))
	}
}

func TestPrepareVM_NoTokenFileWhenTokenEmpty(t *testing.T) {
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	os.MkdirAll(ws, 0755)

	configPath := filepath.Join(tmp, "goose.yaml")
	secretsPath := filepath.Join(tmp, "goose-secrets.yaml")
	os.WriteFile(configPath, []byte("GOOSE_PROVIDER: test\n"), 0644)
	os.WriteFile(secretsPath, []byte("TEST_KEY: x\n"), 0644)

	prepareTestVM(t, tmp, ws, VMPrepareOptions{
		HostConfigPath:  configPath,
		HostSecretsPath: secretsPath,
		AgentToken:      "", // empty — no file should be written
	})

	mntDir := os.Getenv("_TEST_MNT")
	tokenFile := filepath.Join(mntDir, "root", ".ephemera-agent-token")
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Errorf("expected no token file, but it exists (stat err: %v)", err)
	}
}
