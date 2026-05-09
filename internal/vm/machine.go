package vm

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/sirupsen/logrus"
)

// vsockReconfigPort is the well-known port the goose-agent vsock listener binds to inside the VM.
const vsockReconfigPort = 1234

// VMConfig holds the configuration required by the control plane to provision a new VM
type VMConfig struct {
	VMID           string // e.g., "agent-01"
	SocketPath     string // e.g., "/tmp/firecracker-agent-01.sock"
	FirecrackerBin string // Path to artifacts/firecracker
	KernelPath     string // Path to artifacts/vmlinux.bin
	RootfsPath     string // Path to the cloned rootfs
	TapDevice      string // e.g., "tap0"
	MacAddress     string // e.g., "AA:FC:00:00:00:01"
	GuestIP        string
	GatewayIP      string
	VsockUDSPath   string // host-side UDS for Firecracker vsock proxy; enables post-restore IP reconfiguration
}

// StartMachine initializes and boots a Firecracker microVM based on the provided VMConfig.
func StartMachine(ctx context.Context, cfg VMConfig) (*firecracker.Machine, error) {
	rootDriveID := "rootfs"
	isRootDevice := true
	isReadOnly := false

	drives := []models.Drive{
		{
			DriveID:      &rootDriveID,
			PathOnHost:   &cfg.RootfsPath,
			IsRootDevice: &isRootDevice,
			IsReadOnly:   &isReadOnly,
		},
	}

	netIfaces := []firecracker.NetworkInterface{
		{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				MacAddress:  cfg.MacAddress,
				HostDevName: cfg.TapDevice,
			},
		},
	}

	// Inject dynamic IP via kernel ip= parameter.
	// pci=off, root=/dev/vda, rw are intentionally omitted: the firecracker-go-sdk
	// derives them from the Drive configuration and appends them automatically,
	// so specifying them here would cause duplicates in the kernel command line.
	networkArg := fmt.Sprintf("ip=%s::%s:255.255.255.0::eth0:off", cfg.GuestIP, cfg.GatewayIP)
	// reboot=k: signals Firecracker to handle reboot/poweroff via the KVM interface,
	// allowing it to exit cleanly (exit_code=0) when micro-init calls poweroff(2).
	// panic=1 is no longer needed: micro-init (PID 1) calls syscall.Reboot with
	// LINUX_REBOOT_CMD_POWER_OFF on exit, preventing kernel panics entirely.
	kernelArgs := fmt.Sprintf("console=ttyS0 reboot=k %s init=/usr/local/sbin/micro-init", networkArg)

	// Provide a log FIFO path so the SDK sends PUT /logger to Firecracker with
	// LogLevel=Warning, suppressing verbose INFO-level API trace messages.
	// The SDK (CreateLogFilesHandler) creates the FIFO itself — we only remove
	// any stale file left by a previous run so the SDK's Mkfifo call succeeds.
	logFifoPath := fmt.Sprintf("/tmp/fc-%s-log.fifo", cfg.VMID)
	os.Remove(logFifoPath)

	fcCfg := firecracker.Config{
		SocketPath:        cfg.SocketPath,
		KernelImagePath:   cfg.KernelPath,
		KernelArgs:        kernelArgs,
		Drives:            drives,
		NetworkInterfaces: netIfaces,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:       firecracker.Int64(2),
			MemSizeMib:      firecracker.Int64(2048),
			TrackDirtyPages: true, // required for diff snapshot creation
		},
		LogFifo:      logFifoPath,
		LogLevel:     "Warning",
		VsockDevices: vsockDevices(cfg.VsockUDSPath),
	}

	// Capture Firecracker process logs at Warn level.
	// Debug produces hundreds of lines per boot (API PUT/GET traces, handler names)
	// that are not useful in normal operation.
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	logEntry := logrus.NewEntry(logger)

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(cfg.FirecrackerBin).
		WithSocketPath(cfg.SocketPath).
		WithStdout(os.Stdout).
		WithStderr(os.Stderr).
		Build(ctx)

	machineOpts := []firecracker.Opt{
		firecracker.WithLogger(logEntry),
		firecracker.WithProcessRunner(cmd),
	}

	// Create the Machine instance (not yet booted)
	m, err := firecracker.NewMachine(ctx, fcCfg, machineOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create machine: %w", err)
	}

	// Start the process and boot the VM (handles API socket communication automatically)
	if err := m.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start machine: %w", err)
	}

	logger.Infof("MicroVM [%s] successfully started on socket %s", cfg.VMID, cfg.SocketPath)

	// Return the machine instance so the control plane can manage its lifecycle (e.g., stop, wait)
	return m, nil
}

// RestoreMachine starts a new Firecracker process from a snapshot created by CreateSnapshot.
// The guest resumes execution from exactly where it was frozen — no kernel boot occurs.
//
// Constraints the caller must satisfy:
//   - cfg.RootfsPath must be at the same absolute path as when the snapshot was created
//     (Firecracker snapshot state embeds the original disk path)
//   - cfg.MacAddress must match the MAC recorded in the snapshot state
//   - The guest IP is restored from memory; cfg.GuestIP is used only for host-side routing
func RestoreMachine(ctx context.Context, cfg VMConfig, memFilePath, snapshotPath string) (*firecracker.Machine, error) {
	rootDriveID := "rootfs"
	isRootDevice := true
	isReadOnly := false

	drives := []models.Drive{
		{
			DriveID:      &rootDriveID,
			PathOnHost:   &cfg.RootfsPath,
			IsRootDevice: &isRootDevice,
			IsReadOnly:   &isReadOnly,
		},
	}

	netIfaces := []firecracker.NetworkInterface{
		{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				MacAddress:  cfg.MacAddress,
				HostDevName: cfg.TapDevice,
			},
		},
	}

	logFifoPath := fmt.Sprintf("/tmp/fc-%s-log.fifo", cfg.VMID)
	os.Remove(logFifoPath)

	// No KernelImagePath or KernelArgs: snapshot already contains the full running kernel state.
	// VsockDevices is set so AddVsocksHandler (which runs after LoadSnapshotHandler) updates
	// the vsock UDS path to the new per-VM path, enabling ReconfigureGuestIP after restore.
	fcCfg := firecracker.Config{
		SocketPath:        cfg.SocketPath,
		Drives:            drives,
		NetworkInterfaces: netIfaces,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:       firecracker.Int64(2),
			MemSizeMib:      firecracker.Int64(2048),
			TrackDirtyPages: true, // required for diff snapshot creation
		},
		LogFifo:      logFifoPath,
		LogLevel:     "Warning",
		VsockDevices: vsockDevices(cfg.VsockUDSPath),
	}

	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	logEntry := logrus.NewEntry(logger)

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(cfg.FirecrackerBin).
		WithSocketPath(cfg.SocketPath).
		WithStdout(os.Stdout).
		WithStderr(os.Stderr).
		Build(ctx)

	// Vsock cannot be configured via PUT /vsock when restoring from a snapshot:
	//   - Before LoadSnapshot → blocked ("boot-specific resource conflict")
	//   - After LoadSnapshot  → blocked ("not supported after starting the microVM")
	// Solution: omit AddVsocksHandler entirely. Firecracker rebuilds the vsock device
	// from the snapshot state, recreating the UDS socket at the original path recorded
	// in state.bin. The caller must pass that original path to ReconfigureGuestIP.
	//
	// EnableDiffSnapshots=true allows subsequent diff snapshots of the restored VM.
	noVsockRestoreOpt := firecracker.Opt(func(m *firecracker.Machine) {
		m.Cfg.Snapshot.EnableDiffSnapshots = true
		m.Handlers.FcInit = firecracker.HandlerList{}.Append(
			firecracker.SetupNetworkHandler,
			firecracker.StartVMMHandler,
			firecracker.CreateLogFilesHandler,
			firecracker.BootstrapLoggingHandler,
			firecracker.LoadSnapshotHandler,
			// AddVsocksHandler intentionally omitted
		)
	})

	machineOpts := []firecracker.Opt{
		firecracker.WithLogger(logEntry),
		firecracker.WithProcessRunner(cmd),
		firecracker.WithSnapshot(memFilePath, snapshotPath),
		noVsockRestoreOpt, // overrides FcInit to remove AddVsocksHandler
	}

	m, err := firecracker.NewMachine(ctx, fcCfg, machineOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create machine for restore: %w", err)
	}

	if err := m.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to restore machine from snapshot: %w", err)
	}

	// WithSnapshot sets ResumeVM=false by default, so the guest is paused after loading.
	// Explicitly resume so the guest resumes execution and goose-agent becomes reachable.
	if err := m.ResumeVM(ctx); err != nil {
		m.StopVMM()
		return nil, fmt.Errorf("failed to resume restored machine: %w", err)
	}

	logger.Infof("MicroVM [%s] restored and resumed on socket %s", cfg.VMID, cfg.SocketPath)
	return m, nil
}

// vsockDevices returns the vsock device list for a Firecracker config.
// Returns an empty slice when udsPath is empty (vsock disabled).
func vsockDevices(udsPath string) []firecracker.VsockDevice {
	if udsPath == "" {
		return nil
	}
	os.Remove(udsPath) // remove stale socket from a previous run
	return []firecracker.VsockDevice{{ID: "1", CID: 3, Path: udsPath}}
}

// ReconfigureGuestIP connects to the VM's vsock listener and instructs the guest
// to reconfigure eth0 with newCIDRIP (e.g. "10.0.1.5/24") and default gateway.
// Used after snapshot restore to assign a fresh IP without rebooting the guest.
// Retries for up to 4 seconds to allow the vsock proxy to become ready.
func ReconfigureGuestIP(vsockUDSPath, newCIDRIP, gateway string) error {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		if attempt > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		if err := vsockSendChangeIP(vsockUDSPath, newCIDRIP, gateway); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("vsock IP reconfigure failed after 20 attempts: %w", lastErr)
}

func vsockSendChangeIP(vsockUDSPath, newCIDRIP, gateway string) error {
	conn, err := net.DialTimeout("unix", vsockUDSPath, 1*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	r := bufio.NewReader(conn)

	// Firecracker vsock proxy handshake: CONNECT <port>\n → OK <port>\n
	fmt.Fprintf(conn, "CONNECT %d\n", vsockReconfigPort)
	resp, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("vsock handshake: %w", err)
	}
	if !strings.HasPrefix(resp, "OK ") {
		return fmt.Errorf("vsock NACK: %s", strings.TrimSpace(resp))
	}

	// Send reconfiguration command and wait for confirmation.
	fmt.Fprintf(conn, "CHANGE_IP %s %s\n", newCIDRIP, gateway)
	reply, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read reconfig reply: %w", err)
	}
	if !strings.HasPrefix(reply, "OK") {
		return fmt.Errorf("reconfig failed: %s", strings.TrimSpace(reply))
	}
	return nil
}