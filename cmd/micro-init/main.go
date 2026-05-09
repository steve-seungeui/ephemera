// micro-init runs as PID 1 inside each Firecracker MicroVM.
// It mounts virtual filesystems, starts goose-agent as a child process, and calls
// poweroff(2) when the agent exits or when SIGTERM is received — avoiding the kernel
// panic that would occur if PID 1 exited without a graceful shutdown sequence.
package main

import (
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func main() {
	// Mount essential virtual filesystems that goose-agent and glibc expect.
	mount("proc", "/proc", "proc", 0, "")
	mount("sysfs", "/sys", "sysfs", 0, "")
	mount("devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, "")
	os.MkdirAll("/dev/pts", 0755)
	mount("devpts", "/dev/pts", "devpts", 0, "")

	// Set environment variables that init systems normally provide.
	// Without HOME, goose cannot resolve ~/.config/goose/config.yaml.
	os.Setenv("HOME", "/root")
	os.Setenv("USER", "root")
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")

	cmd := exec.Command("/usr/local/bin/goose-agent")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start goose-agent: %v", err)
		shutdown()
		return
	}

	// PID 1 must explicitly register for signals — the kernel does not deliver
	// default-action signals to PID 1 unless a handler is installed.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	select {
	case err := <-doneCh:
		if err != nil {
			log.Printf("goose-agent exited: %v", err)
		}
	case sig := <-sigCh:
		log.Printf("Signal %v received, stopping goose-agent", sig)
		cmd.Process.Signal(syscall.SIGTERM)
		<-doneCh
	}

	shutdown()
}

func mount(src, target, fstype string, flags uintptr, data string) {
	if err := syscall.Mount(src, target, fstype, flags, data); err != nil {
		log.Printf("Warning: mount %s -> %s: %v", src, target, err)
	}
}

func shutdown() {
	syscall.Sync()
	// LINUX_REBOOT_CMD_POWER_OFF triggers an ACPI power-off event.
	// Firecracker intercepts this and exits cleanly (exit_code=0).
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		log.Printf("poweroff failed: %v", err)
	}
}
