package main

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"ephemera/internal/network"
	"ephemera/internal/storage"
)

func main() {
	log.Println("Starting Ephemera Control Plane...")
	if len(apiClients) == 0 {
		log.Println("Warning: no API token configured (EPHEMERA_API_TOKENS / EPHEMERA_API_TOKEN unset) — API is unauthenticated.")
	}

	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("Fatal: %v", err)
	}

	goldenImagePath  := filepath.Join(cwd, "artifacts/golden-image.ext4")
	buildScriptPath  := filepath.Join(cwd, "scripts/build_image.sh")
	kernelPath       := filepath.Join(cwd, "artifacts/vmlinux.bin")
	firecrackerPath  := filepath.Join(cwd, "artifacts/firecracker")
	microInitPath    := filepath.Join(cwd, "artifacts/micro-init")
	gooseAgentPath   := filepath.Join(cwd, "artifacts/goose-agent")
	gooseConfigPath  := filepath.Join(cwd, "configs/goose.yaml")
	gooseSecretsPath := filepath.Join(cwd, "configs/goose-secrets.yaml")
	snapshotDir      := filepath.Join(cwd, "snapshots")

	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		log.Fatalf("Fatal: failed to create snapshot directory: %v", err)
	}

	const (
		kernelDownloadURL      = "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64/vmlinux-6.1.155"
		firecrackerDownloadURL = "https://github.com/firecracker-microvm/firecracker/releases/download/v1.15.1/firecracker-v1.15.1-x86_64.tgz"
		firecrackerSHA256      = "d4a32ab2322d887ca1bc4a4e7afa9cc35393e6362dfc2b3becb389d362e4275a"
	)

	// 1. Build in-VM binaries (included in the golden image).
	log.Println("Ensuring micro-init binary...")
	if err := storage.EnsureMicroInit(microInitPath, cwd); err != nil {
		log.Fatalf("Fatal: %v", err)
	}

	log.Println("Ensuring goose-agent binary...")
	if err := storage.EnsureGooseAgent(gooseAgentPath, cwd); err != nil {
		log.Fatalf("Fatal: %v", err)
	}

	// 2. Bootstrap storage artifacts.
	log.Println("Initializing Storage Provisioner...")
	provisioner, err := storage.NewProvisioner(goldenImagePath, "/tmp/goose-workspaces", buildScriptPath)
	if err != nil {
		log.Fatalf("Fatal: %v", err)
	}

	log.Println("Ensuring kernel binary...")
	if err := storage.EnsureKernel(kernelPath, kernelDownloadURL); err != nil {
		log.Fatalf("Fatal: %v", err)
	}

	log.Println("Ensuring Firecracker binary...")
	if err := storage.EnsureFirecracker(firecrackerPath, firecrackerDownloadURL, firecrackerSHA256); err != nil {
		log.Fatalf("Fatal: %v", err)
	}

	// 3. Network.
	log.Println("Initializing Network Manager...")
	netManager := network.NewManager("10.0.1.", "10.0.1.1")

	// 4. Start control plane.
	cp := NewControlPlane(
		provisioner, netManager,
		kernelPath, firecrackerPath, gooseConfigPath, gooseSecretsPath,
		cwd, snapshotDir,
	)
	defer cp.DestroyAll()

	go func() {
		if err := cp.Start(); err != nil && err != http.ErrServerClosed {
			log.Printf("Control plane API error: %v", err)
		}
	}()
	defer cp.Shutdown()

	// 5. Wait for termination. SIGHUP reloads API tokens without restarting.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		sig := <-sigChan
		if sig == syscall.SIGHUP {
			cp.ReloadClients()
			continue
		}
		break
	}
	log.Println("Shutting down...")
}
