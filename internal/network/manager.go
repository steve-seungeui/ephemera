package network

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"sync"
)

type Manager struct {
	mu         sync.Mutex
	ipList     []string // sorted slice for deterministic allocation order
	ipInUse    map[string]bool
	gatewayIP  string
	subnet     string
	nextTapID  int
	freeTapIDs []int // recycled tap IDs — prefer these over nextTapID
	bridgeName string
}

func NewManager(subnet string, gatewayIP string) *Manager {
	// Build a sorted IP list so allocation order is deterministic.
	// A map's iteration order is randomized in Go; a slice is not.
	ips := make([]string, 0, 253)
	for i := 2; i <= 254; i++ {
		ips = append(ips, fmt.Sprintf("%s%d", subnet, i))
	}
	sort.Strings(ips)

	inUse := make(map[string]bool, len(ips))
	for _, ip := range ips {
		inUse[ip] = false
	}

	m := &Manager{
		ipList:     ips,
		ipInUse:    inUse,
		gatewayIP:  gatewayIP,
		subnet:     subnet,
		nextTapID:  1,
		freeTapIDs: nil,
		bridgeName: "goose-br0",
	}

	if err := m.setupBridge(); err != nil {
		log.Printf("Warning: Bridge setup note (might already exist): %v", err)
	}

	return m
}

func (m *Manager) setupBridge() error {
	exec.Command("ip", "link", "add", "name", m.bridgeName, "type", "bridge").Run()
	exec.Command("ip", "addr", "add", m.gatewayIP+"/24", "dev", m.bridgeName).Run()
	if err := exec.Command("ip", "link", "set", "dev", m.bridgeName, "up").Run(); err != nil {
		return err
	}

	// Enable IP forwarding so VM packets can reach the internet via the host.
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
		log.Printf("Warning: failed to enable ip_forward: %v", err)
	}

	// NAT masquerade: rewrite source IP of VM packets leaving the internal subnet
	// so replies are routed back through the host.
	// -C checks for an existing identical rule; only add with -A when absent.
	masqArgs := []string{
		"POSTROUTING", "-s", m.subnet + "0/24", "!", "-d", m.subnet + "0/24", "-j", "MASQUERADE",
	}
	if exec.Command("iptables", append([]string{"-t", "nat", "-C"}, masqArgs...)...).Run() != nil {
		exec.Command("iptables", append([]string{"-t", "nat", "-A"}, masqArgs...)...).Run()
	}

	return nil
}

func (m *Manager) Allocate() (tapDevice string, guestIP string, macAddr string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Pick the first available IP from the sorted list for deterministic ordering.
	guestIP = ""
	for _, ip := range m.ipList {
		if !m.ipInUse[ip] {
			guestIP = ip
			m.ipInUse[ip] = true
			break
		}
	}
	if guestIP == "" {
		return "", "", "", fmt.Errorf("no available IP addresses")
	}

	// Prefer recycled tap IDs over allocating a new one.
	var tapID int
	if len(m.freeTapIDs) > 0 {
		tapID = m.freeTapIDs[0]
		m.freeTapIDs = m.freeTapIDs[1:]
	} else {
		tapID = m.nextTapID
		m.nextTapID++
	}

	tapDevice = fmt.Sprintf("tap%d", tapID)
	macAddr = fmt.Sprintf("AA:FC:00:00:%02X:%02X", tapID/256, tapID%256)

	log.Printf("Creating TAP device: %s for IP: %s...", tapDevice, guestIP)
	if err := m.createTapDevice(tapDevice); err != nil {
		m.ipInUse[guestIP] = false
		m.freeTapIDs = append([]int{tapID}, m.freeTapIDs...) // return ID to free-list
		return "", "", "", fmt.Errorf("failed to create TAP device: %w", err)
	}

	return tapDevice, guestIP, macAddr, nil
}

// AllocateForRestore recreates a TAP device with the exact original name and MAC (required by
// Firecracker's snapshot state.bin) and allocates any available IP from the pool.
// The guest IP is reconfigured post-restore via vsock, so any free IP works.
func (m *Manager) AllocateForRestore(tapDeviceName, macAddr string) (tapDevice string, guestIP string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Pick any available IP — the guest will be told its new IP via vsock after restore.
	for _, ip := range m.ipList {
		if !m.ipInUse[ip] {
			guestIP = ip
			m.ipInUse[ip] = true
			break
		}
	}
	if guestIP == "" {
		return "", "", fmt.Errorf("no available IP addresses for restore")
	}

	// Parse tap ID to keep the pool consistent (prevent future collisions).
	var tapID int
	if _, scanErr := fmt.Sscanf(tapDeviceName, "tap%d", &tapID); scanErr != nil {
		m.ipInUse[guestIP] = false
		return "", "", fmt.Errorf("invalid tap device name %q: %w", tapDeviceName, scanErr)
	}
	for i, id := range m.freeTapIDs {
		if id == tapID {
			m.freeTapIDs = append(m.freeTapIDs[:i], m.freeTapIDs[i+1:]...)
			break
		}
	}
	if tapID >= m.nextTapID {
		m.nextTapID = tapID + 1
	}

	log.Printf("Creating TAP device %s (restore) with IP %s and MAC %s...", tapDeviceName, guestIP, macAddr)
	if err := m.createTapDeviceWithMAC(tapDeviceName, macAddr); err != nil {
		m.ipInUse[guestIP] = false
		return "", "", fmt.Errorf("failed to create TAP device for restore: %w", err)
	}

	return tapDeviceName, guestIP, nil
}

// createTapDeviceWithMAC creates a TAP device and sets its MAC address explicitly,
// overriding the kernel-assigned default. Used for snapshot restoration where the
// guest's eth0 already has the original MAC baked into its memory state.
func (m *Manager) createTapDeviceWithMAC(tapName, macAddr string) error {
	exec.Command("ip", "link", "delete", tapName).Run()

	if err := exec.Command("ip", "tuntap", "add", tapName, "mode", "tap").Run(); err != nil {
		return fmt.Errorf("failed to add tuntap %s: %w", tapName, err)
	}

	if err := exec.Command("ip", "link", "set", "dev", tapName, "address", macAddr).Run(); err != nil {
		m.deleteTapDevice(tapName)
		return fmt.Errorf("failed to set MAC %s on %s: %w", macAddr, tapName, err)
	}

	if err := exec.Command("ip", "link", "set", tapName, "master", m.bridgeName).Run(); err != nil {
		m.deleteTapDevice(tapName)
		return fmt.Errorf("failed to attach %s to bridge %s: %w", tapName, m.bridgeName, err)
	}

	if err := exec.Command("ip", "link", "set", tapName, "up").Run(); err != nil {
		m.deleteTapDevice(tapName)
		return fmt.Errorf("failed to bring up %s: %w", tapName, err)
	}

	return nil
}

func (m *Manager) Release(tapDevice string, guestIP string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.deleteTapDevice(tapDevice); err != nil {
		log.Printf("Warning: failed to delete TAP device %s: %v", tapDevice, err)
	}

	// Return IP to the pool.
	if _, exists := m.ipInUse[guestIP]; exists {
		m.ipInUse[guestIP] = false
	}

	// Return tap ID to the free-list for reuse.
	var tapID int
	if _, err := fmt.Sscanf(tapDevice, "tap%d", &tapID); err == nil {
		m.freeTapIDs = append(m.freeTapIDs, tapID)
	}
	return nil
}

func (m *Manager) createTapDevice(tapName string) error {
	exec.Command("ip", "link", "delete", tapName).Run()

	if err := exec.Command("ip", "tuntap", "add", tapName, "mode", "tap").Run(); err != nil {
		return fmt.Errorf("failed to add tuntap %s: %w", tapName, err)
	}

	if err := exec.Command("ip", "link", "set", tapName, "master", m.bridgeName).Run(); err != nil {
		m.deleteTapDevice(tapName)
		return fmt.Errorf("failed to attach %s to bridge %s: %w", tapName, m.bridgeName, err)
	}

	if err := exec.Command("ip", "link", "set", tapName, "up").Run(); err != nil {
		m.deleteTapDevice(tapName)
		return fmt.Errorf("failed to bring up %s: %w", tapName, err)
	}

	return nil
}

func (m *Manager) deleteTapDevice(tapName string) error {
	return exec.Command("ip", "link", "delete", tapName).Run()
}
