package storage

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Provisioner handles the disk lifecycle for MicroVMs.
type Provisioner struct {
	GoldenImagePath string // e.g., "artifacts/ubuntu-22.04-goose.ext4"
	WorkspaceDir    string // e.g., "/tmp/goose-workspaces"
	BuildScriptPath string // e.g., "scripts/build_image.sh"
}

// NewProvisioner initializes a new Storage Provisioner and ensures the golden image exists.
func NewProvisioner(goldenImagePath, workspaceDir, buildScriptPath string) (*Provisioner, error) {
	// 1. Ensure the workspace directory exists
	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create workspace directory: %w", err)
	}

	p := &Provisioner{
		GoldenImagePath: goldenImagePath,
		WorkspaceDir:    workspaceDir,
		BuildScriptPath: buildScriptPath,
	}

	// 2. Self-bootstrap: Check and build golden image if missing
	if err := p.EnsureGoldenImage(); err != nil {
		return nil, fmt.Errorf("failed to ensure golden image: %w", err)
	}

	return p, nil
}

// EnsureGoldenImage checks if the golden image exists, and if not, builds it from scratch.
func (p *Provisioner) EnsureGoldenImage() error {
	_, err := os.Stat(p.GoldenImagePath)
	if err == nil {
		log.Printf("Golden image found at %s. Skipping build.", p.GoldenImagePath)
		return nil
	}

	if !os.IsNotExist(err) {
		return fmt.Errorf("error checking golden image: %w", err)
	}

	log.Printf("Golden image not found at %s. Starting automated build process...", p.GoldenImagePath)
	log.Printf("This may take a few minutes. Please wait.")

	// Execute the build script
	cmd := exec.Command("bash", p.BuildScriptPath)
	
	// Pipe the script's output to the daemon's standard output for visibility
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to execute build script: %w", err)
	}

	// Verify the image was actually created by the script
	if _, err := os.Stat(p.GoldenImagePath); os.IsNotExist(err) {
		return fmt.Errorf("build script completed, but golden image was not found at expected path")
	}

	log.Printf("Golden image successfully built and verified at %s.", p.GoldenImagePath)
	return nil
}

// CloneDisk creates an isolated copy of the golden image for a specific VM.
func (p *Provisioner) CloneDisk(vmID string) (string, error) {
	destPath := filepath.Join(p.WorkspaceDir, fmt.Sprintf("%s.ext4", vmID))
	log.Printf("Cloning golden image to %s...", destPath)

	srcFile, err := os.Open(p.GoldenImagePath)
	if err != nil {
		return "", fmt.Errorf("failed to open golden image: %w", err)
	}
	defer srcFile.Close()

	destFile, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("failed to create destination file: %w", err)
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, srcFile); err != nil {
		return "", fmt.Errorf("failed to copy disk contents: %w", err)
	}

	if err := destFile.Sync(); err != nil {
		return "", fmt.Errorf("failed to sync data to disk: %w", err)
	}

	log.Printf("Successfully provisioned isolated disk for MicroVM [%s]", vmID)
	return destPath, nil
}

// mountVMDisk mounts the cloned VM disk, calls fn with the mount point, then unmounts.
// All file injection helpers use this to avoid duplicating mount/unmount logic.
func (p *Provisioner) mountVMDisk(vmID string, fn func(mntDir string) error) error {
	diskPath := filepath.Join(p.WorkspaceDir, fmt.Sprintf("%s.ext4", vmID))
	mntDir := fmt.Sprintf("/tmp/goose-mnt-%s", vmID)

	// Clean up any stale mount left by a previous failed run before attempting
	// a fresh mount. -l (lazy) detaches immediately even if the FS is still busy.
	exec.Command("umount", "-l", mntDir).Run()
	os.Remove(mntDir)

	if err := os.MkdirAll(mntDir, 0755); err != nil {
		return fmt.Errorf("failed to create mount dir: %w", err)
	}

	mounted := false
	defer func() {
		if mounted {
			// -l ensures the unmount succeeds even if a background goroutine
			// still holds a file descriptor on the mounted filesystem.
			if err := exec.Command("umount", "-l", mntDir).Run(); err != nil {
				log.Printf("Warning: failed to unmount %s: %v", mntDir, err)
			}
		}
		os.Remove(mntDir)
	}()

	if out, err := exec.Command("mount", "-o", "loop", diskPath, mntDir).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to mount VM disk: %w: %s", err, out)
	}
	mounted = true

	return fn(mntDir)
}

// VMPrepareOptions carries all per-VM file injection parameters for PrepareVM.
type VMPrepareOptions struct {
	HostConfigPath  string // path to goose.yaml on the host
	HostSecretsPath string // path to goose-secrets.yaml on the host
	Task            string // optional task prompt written to /root/task.txt
	AgentToken      string // if non-empty, written to /root/.ephemera-agent-token (mode 0600)
}

// PrepareVM injects all VM-specific files in a single mount/unmount cycle:
//   - /root/.config/goose/config.yaml       (provider, model, extensions)
//   - /root/.config/goose/secrets.yaml      (API keys; requires GOOSE_DISABLE_KEYRING=true)
//   - /root/task.txt                         (task prompt; optional)
//   - /root/.ephemera-agent-token            (Bearer token for goose-agent auth; optional)
func (p *Provisioner) PrepareVM(vmID string, opts VMPrepareOptions) error {
	return p.mountVMDisk(vmID, func(mntDir string) error {
		gooseConfigDir := filepath.Join(mntDir, "root", ".config", "goose")
		if err := os.MkdirAll(gooseConfigDir, 0755); err != nil {
			return fmt.Errorf("failed to create goose config dir: %w", err)
		}

		for _, pair := range []struct{ src, dst string }{
			{opts.HostConfigPath, "config.yaml"},
			{opts.HostSecretsPath, "secrets.yaml"},
		} {
			if err := copyFile(pair.src, filepath.Join(gooseConfigDir, pair.dst)); err != nil {
				return fmt.Errorf("failed to inject %s: %w", pair.dst, err)
			}
		}

		// task is optional: empty means persistent mode (goose-agent handles requests).
		if opts.Task != "" {
			taskPath := filepath.Join(mntDir, "root", "task.txt")
			if err := os.WriteFile(taskPath, []byte(opts.Task), 0644); err != nil {
				return fmt.Errorf("failed to write task.txt: %w", err)
			}
		}

		// AgentToken is written with mode 0600 so only root can read it inside the VM.
		if opts.AgentToken != "" {
			tokenPath := filepath.Join(mntDir, "root", ".ephemera-agent-token")
			if err := os.WriteFile(tokenPath, []byte(opts.AgentToken), 0600); err != nil {
				return fmt.Errorf("failed to write agent token: %w", err)
			}
		}

		if err := injectHostTimezone(mntDir); err != nil {
			log.Printf("Warning: failed to inject timezone: %v", err)
		}

		log.Printf("Config, secrets, and timezone injected into MicroVM [%s]", vmID)
		return nil
	})
}

// injectHostTimezone configures the VM disk to use the host's timezone.
// It requires tzdata to be installed in the VM (golden image) so that
// /usr/share/zoneinfo/{tzName} exists for the symlink to resolve correctly.
func injectHostTimezone(mntDir string) error {
	// Derive the IANA timezone name from the host.
	// Prefer /etc/timezone (plain text), fall back to resolving /etc/localtime symlink.
	tzName := "UTC"
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		tzName = strings.TrimSpace(string(b))
	} else if target, err := os.Readlink("/etc/localtime"); err == nil {
		if idx := strings.Index(target, "zoneinfo/"); idx >= 0 {
			tzName = target[idx+len("zoneinfo/"):]
		}
	}

	// Verify the zoneinfo file exists inside the VM before creating the symlink.
	zoneFile := filepath.Join(mntDir, "usr", "share", "zoneinfo", tzName)
	if _, err := os.Stat(zoneFile); err != nil {
		return fmt.Errorf("zoneinfo file not found in VM (%s): tzdata may not be installed", zoneFile)
	}

	// Replace /etc/localtime with a symlink to the correct zoneinfo file.
	// This is the standard Linux approach; glibc reads it automatically when TZ is unset.
	dst := filepath.Join(mntDir, "etc", "localtime")
	os.Remove(dst)
	if err := os.Symlink("/usr/share/zoneinfo/"+tzName, dst); err != nil {
		return fmt.Errorf("failed to create localtime symlink: %w", err)
	}

	// Write /etc/timezone for tools that read the plain-text name.
	tzFile := filepath.Join(mntDir, "etc", "timezone")
	if err := os.WriteFile(tzFile, []byte(tzName+"\n"), 0644); err != nil {
		return fmt.Errorf("failed to write /etc/timezone: %w", err)
	}

	log.Printf("VM timezone set to %s.", tzName)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// EnsureMicroInit builds the micro-init binary into binaryPath if it doesn't exist.
// micro-init runs as PID 1 inside each VM; it mounts virtual filesystems, starts
// goose-agent as a child, and calls poweroff(2) on exit for graceful VM shutdown.
func EnsureMicroInit(binaryPath, projectRoot string) error {
	if _, err := os.Stat(binaryPath); err == nil {
		log.Printf("micro-init found at %s.", binaryPath)
		return nil
	}

	log.Printf("Building micro-init at %s ...", binaryPath)

	if err := os.MkdirAll(filepath.Dir(binaryPath), 0755); err != nil {
		return fmt.Errorf("failed to create artifacts dir: %w", err)
	}

	cmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/micro-init/")
	cmd.Dir = projectRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")

	if err := cmd.Run(); err != nil {
		os.Remove(binaryPath)
		return fmt.Errorf("failed to build micro-init: %w", err)
	}

	log.Printf("micro-init built at %s.", binaryPath)
	return nil
}

// EnsureGooseAgent builds the goose-agent binary into binaryPath if it doesn't exist.
// The binary is compiled from cmd/goose-agent/ in the projectRoot directory using
// CGO_ENABLED=0 so it is statically linked and portable across the VM's glibc version.
func EnsureGooseAgent(binaryPath, projectRoot string) error {
	if _, err := os.Stat(binaryPath); err == nil {
		log.Printf("goose-agent found at %s.", binaryPath)
		return nil
	}

	log.Printf("Building goose-agent at %s ...", binaryPath)

	if err := os.MkdirAll(filepath.Dir(binaryPath), 0755); err != nil {
		return fmt.Errorf("failed to create artifacts dir: %w", err)
	}

	cmd := exec.Command("go", "build", "-o", binaryPath, "./cmd/goose-agent/")
	cmd.Dir = projectRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")

	if err := cmd.Run(); err != nil {
		os.Remove(binaryPath)
		return fmt.Errorf("failed to build goose-agent: %w", err)
	}

	log.Printf("goose-agent built at %s.", binaryPath)
	return nil
}

// EnsureKernel downloads the Firecracker kernel binary to kernelPath if it does not exist.
func EnsureKernel(kernelPath, downloadURL string) error {
	if _, err := os.Stat(kernelPath); err == nil {
		log.Printf("Kernel found at %s.", kernelPath)
		return nil
	}

	log.Printf("Kernel not found at %s. Downloading from %s ...", kernelPath, downloadURL)

	if err := os.MkdirAll(filepath.Dir(kernelPath), 0755); err != nil {
		return fmt.Errorf("failed to create kernel directory: %w", err)
	}

	resp, err := http.Get(downloadURL) //nolint:noctx
	if err != nil {
		return fmt.Errorf("failed to download kernel: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kernel download returned HTTP %s", resp.Status)
	}

	f, err := os.Create(kernelPath)
	if err != nil {
		return fmt.Errorf("failed to create kernel file: %w", err)
	}

	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(kernelPath) // remove partial download
		return fmt.Errorf("failed to write kernel: %w", err)
	}

	if err := f.Close(); err != nil {
		os.Remove(kernelPath)
		return fmt.Errorf("failed to flush kernel file: %w", err)
	}

	log.Printf("Kernel successfully downloaded to %s.", kernelPath)
	return nil
}

// EnsureFirecracker downloads the Firecracker release tarball, verifies its SHA256,
// and extracts the firecracker binary to destPath. A no-op if the binary already exists.
func EnsureFirecracker(destPath, downloadURL, expectedSHA256 string) error {
	if _, err := os.Stat(destPath); err == nil {
		log.Printf("Firecracker found at %s.", destPath)
		return nil
	}

	log.Printf("Firecracker not found at %s. Downloading from %s ...", destPath, downloadURL)

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Stream download into a temp file and compute SHA256 simultaneously.
	tmp, err := os.CreateTemp("", "firecracker-*.tgz")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	resp, err := http.Get(downloadURL) //nolint:noctx
	if err != nil {
		tmp.Close()
		return fmt.Errorf("failed to download Firecracker: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return fmt.Errorf("firecracker download returned HTTP %s", resp.Status)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write Firecracker tarball: %w", err)
	}
	tmp.Close()

	if actual := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(actual, expectedSHA256) {
		return fmt.Errorf("firecracker SHA256 mismatch: expected %s, got %s", expectedSHA256, actual)
	}

	if err := extractFirecrackerBin(tmpPath, destPath); err != nil {
		return fmt.Errorf("failed to extract Firecracker binary: %w", err)
	}

	log.Printf("Firecracker successfully installed at %s.", destPath)
	return nil
}

// extractFirecrackerBin finds the firecracker binary inside a release .tgz and writes it to dest.
// The release tarball layout is: release-v{VERSION}-x86_64/firecracker-v{VERSION}-x86_64
func extractFirecrackerBin(tgzPath, dest string) error {
	f, err := os.Open(tgzPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("failed to open gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}
		// Match "firecracker-*" but not "jailer-*"
		base := filepath.Base(hdr.Name)
		if hdr.Typeflag != tar.TypeReg || !strings.HasPrefix(base, "firecracker-") {
			continue
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return fmt.Errorf("failed to create binary file: %w", err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			os.Remove(dest)
			return fmt.Errorf("failed to write binary: %w", err)
		}
		return out.Close()
	}
	return fmt.Errorf("firecracker binary not found in archive")
}

// CleanupDisk safely removes the disk image after the VM is destroyed.
func (p *Provisioner) CleanupDisk(vmID string) error {
	destPath := filepath.Join(p.WorkspaceDir, fmt.Sprintf("%s.ext4", vmID))
	log.Printf("Cleaning up disk image: %s", destPath)

	if err := os.Remove(destPath); err != nil {
		if os.IsNotExist(err) {
			log.Printf("Disk image %s already deleted or does not exist.", destPath)
			return nil
		}
		return fmt.Errorf("failed to delete disk file: %w", err)
	}

	log.Printf("Successfully cleaned up storage resources for MicroVM [%s]", vmID)
	return nil
}