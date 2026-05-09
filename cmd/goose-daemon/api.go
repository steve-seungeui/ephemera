package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	ops "github.com/firecracker-microvm/firecracker-go-sdk/client/operations"

	"ephemera/internal/network"
	"ephemera/internal/storage"
	"ephemera/internal/vm"
)

// authMiddleware enforces per-client Bearer token authentication on all requests.
// getClients is called on every request so token changes (via SIGHUP reload) take
// effect immediately without restarting the server or dropping running VMs.
//
// If getClients returns an empty slice, every request is allowed (auth disabled).
//
// Timing-safe design: subtle.ConstantTimeCompare always inspects every byte of both
// operands before returning, so response time does not vary with how many leading
// characters match. All registered tokens are compared on every request (no
// early-exit after the first match) to prevent leaking which client index was hit.
func authMiddleware(getClients func() []APIClient, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clients := getClients()
		if len(clients) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		auth := []byte(r.Header.Get("Authorization"))

		// Compare against every registered token without short-circuiting.
		matchedClient := ""
		for _, c := range clients {
			if subtle.ConstantTimeCompare(auth, []byte("Bearer "+c.Token)) == 1 {
				matchedClient = c.Name
			}
		}

		if matchedClient == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ephemera"`)
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		log.Printf("[%s] %s %s", matchedClient, r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// VMInfo is stored per-VM and returned by GET /vms (no token).
type VMInfo struct {
	VMID     string `json:"vm_id"`
	GuestIP  string `json:"guest_ip"`
	AgentURL string `json:"agent_url"` // proxy URL via control plane when EPHEMERA_PUBLIC_URL is set; otherwise http://{private-ip}:8080
	Profile  string `json:"profile,omitempty"`
}

// VMSpawnResult is returned only by POST /vms.
// AgentToken is the per-VM Bearer token for goose-agent; it is not stored on the server
// after this response — callers must persist it themselves.
type VMSpawnResult struct {
	VMInfo
	AgentToken string `json:"agent_token"`
}

// VMSpawnRequest is the optional JSON body for POST /vms.
type VMSpawnRequest struct {
	Profile string `json:"profile,omitempty"`
}

type runningVM struct {
	VMInfo
	agentToken      string // per-VM bearer token; only returned at spawn time, never re-serialized
	diskPath        string // actual disk file to delete on teardown (spawned) or exception store (COW-restored)
	bindMountTarget string // non-empty for bind-mount restored VMs (legacy path)
	dmSnapshot      *storage.DMSnapshotInfo // non-nil for COW-restored VMs; replaces bindMountTarget
	vsockPath       string // host-side UDS for Firecracker vsock proxy; cleaned up on teardown
	machine         *firecracker.Machine
	tapDevice       string
	socketPath      string
}

// generateAgentToken creates a 32-byte cryptographically random token, hex-encoded (64 chars).
func generateAgentToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate agent token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ControlPlane manages the MicroVM lifecycle and proxies agent requests.
// External clients interact entirely through the control plane URL:
//   - VM lifecycle: POST/GET/DELETE /vms, POST/GET/DELETE /snapshots
//   - Agent proxy:  POST /vms/{vm_id}/tasks, GET /vms/{vm_id}/health,
//     POST /vms/{vm_id}/stop  (forwarded to the VM's private goose-agent)
type ControlPlane struct {
	mu  sync.RWMutex
	vms map[string]*runningVM

	clientsMu sync.RWMutex
	clients   []APIClient

	snapshotsMu sync.RWMutex
	snapshots   map[string]storage.SnapshotMetadata

	// restoreMu serializes the bind-mount-setup + Firecracker-open window so that each
	// Firecracker instance opens the topmost (correct) bind mount before the next restore
	// can stack another one on top. Released as soon as RestoreMachine returns.
	restoreMu sync.Mutex

	provisioner      *storage.Provisioner
	netManager       *network.Manager
	kernelPath       string
	firecrackerPath  string
	gooseConfigPath  string
	gooseSecretsPath string
	workDir          string
	snapshotDir      string

	// agentHTTPClient is used for proxying requests to VM goose-agents.
	// No global timeout — timeouts are controlled by the incoming request's context.
	agentHTTPClient *http.Client

	stopCh chan struct{}
	srv    *http.Server
}

func NewControlPlane(
	provisioner *storage.Provisioner,
	netManager *network.Manager,
	kernelPath, firecrackerPath, gooseConfigPath, gooseSecretsPath, workDir, snapshotDir string,
) *ControlPlane {
	cp := &ControlPlane{
		vms:              make(map[string]*runningVM),
		clients:          apiClients,
		snapshots:        make(map[string]storage.SnapshotMetadata),
		provisioner:      provisioner,
		netManager:       netManager,
		kernelPath:       kernelPath,
		firecrackerPath:  firecrackerPath,
		gooseConfigPath:  gooseConfigPath,
		gooseSecretsPath: gooseSecretsPath,
		workDir:          workDir,
		snapshotDir:      snapshotDir,
		agentHTTPClient:  &http.Client{},
		stopCh:           make(chan struct{}, 1),
	}

	// Load any snapshots persisted from previous daemon runs.
	if existing, err := storage.ListSnapshots(workDir); err == nil {
		for _, meta := range existing {
			cp.snapshots[meta.SnapshotID] = meta
		}
		if len(existing) > 0 {
			log.Printf("Loaded %d existing snapshot(s) from %s", len(existing), snapshotDir)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/vms", cp.handleVMs)
	mux.HandleFunc("/vms/", cp.handleVM)
	mux.HandleFunc("/snapshots", cp.handleSnapshots)
	mux.HandleFunc("/snapshots/", cp.handleSnapshotItem)
	cp.srv = &http.Server{Addr: apiAddr, Handler: authMiddleware(cp.getClients, mux)}
	return cp
}

// getClients returns the current authorized client list under a read lock.
func (cp *ControlPlane) getClients() []APIClient {
	cp.clientsMu.RLock()
	defer cp.clientsMu.RUnlock()
	return cp.clients
}

// ReloadClients re-reads API tokens from the environment and hot-swaps the client list.
// Called on SIGHUP. Running VMs are not affected.
func (cp *ControlPlane) ReloadClients() {
	newClients := loadAPIClients()
	cp.clientsMu.Lock()
	cp.clients = newClients
	cp.clientsMu.Unlock()

	if len(newClients) == 0 {
		log.Println("SIGHUP: token reload complete — auth disabled (no tokens configured)")
		return
	}
	names := make([]string, len(newClients))
	for i, c := range newClients {
		names[i] = c.Name
	}
	log.Printf("SIGHUP: token reload complete — %d client(s): %s", len(newClients), strings.Join(names, ", "))
}

func (cp *ControlPlane) Start() error {
	clients := cp.getClients()
	auth := "disabled"
	if len(clients) > 0 {
		names := make([]string, len(clients))
		for i, c := range clients {
			names[i] = c.Name
		}
		auth = fmt.Sprintf("Bearer token (%d client(s): %s)", len(clients), strings.Join(names, ", "))
	}
	log.Printf("Control plane API on %s  (auth: %s)", apiAddr, auth)
	log.Printf("  POST   /vms                              — spawn VM")
	log.Printf("  GET    /vms                              — list VMs")
	log.Printf("  DELETE /vms/{vm_id}                      — stop VM")
	log.Printf("  POST   /vms/{vm_id}/snapshot             — create snapshot")
	log.Printf("  POST   /vms/{vm_id}/tasks                — proxy: run task on agent")
	log.Printf("  GET    /vms/{vm_id}/health               — proxy: agent health check")
	log.Printf("  POST   /vms/{vm_id}/stop                 — proxy: stop agent")
	log.Printf("  GET    /snapshots                        — list snapshots")
	log.Printf("  POST   /snapshots/{snapshot_id}/restore  — restore VM from snapshot")
	log.Printf("  DELETE /snapshots/{snapshot_id}          — delete snapshot")
	if publicURL != "" {
		log.Printf("  agent_url base: %s (EPHEMERA_PUBLIC_URL)", publicURL)
	}
	return cp.srv.ListenAndServe()
}

func (cp *ControlPlane) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cp.srv.Shutdown(ctx)
}

func (cp *ControlPlane) StopCh() <-chan struct{} { return cp.stopCh }

// POST /vms → spawn VM, return VMInfo with private IP
// GET  /vms → list running VMs
func (cp *ControlPlane) handleVMs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		cp.spawnVM(w, r)
	case http.MethodGet:
		cp.listVMs(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleVM routes /vms/{vm_id} and its sub-paths.
func (cp *ControlPlane) handleVM(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/vms/")

	if strings.HasSuffix(path, "/snapshot") {
		vmID := strings.TrimSuffix(path, "/snapshot")
		if vmID == "" {
			http.Error(w, `{"error":"vm_id required"}`, http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		cp.createSnapshot(w, r, vmID)
		return
	}

	// Agent proxy sub-paths: forward to the VM's private goose-agent.
	// The control plane injects the per-VM agent token; callers use their
	// control plane Bearer token only.
	if strings.HasSuffix(path, "/tasks") {
		vmID := strings.TrimSuffix(path, "/tasks")
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		cp.proxyAgentEndpoint(w, r, vmID, "/tasks")
		return
	}

	if strings.HasSuffix(path, "/health") {
		vmID := strings.TrimSuffix(path, "/health")
		cp.proxyAgentEndpoint(w, r, vmID, "/health")
		return
	}

	if strings.HasSuffix(path, "/stop") {
		vmID := strings.TrimSuffix(path, "/stop")
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"POST required"}`, http.StatusMethodNotAllowed)
			return
		}
		cp.proxyAgentEndpoint(w, r, vmID, "/stop")
		return
	}

	// DELETE /vms/{vm_id}
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE required", http.StatusMethodNotAllowed)
		return
	}
	if path == "" {
		http.Error(w, "vm_id required", http.StatusBadRequest)
		return
	}
	cp.stopVM(w, path)
}

// proxyAgentEndpoint forwards an HTTP request to the VM's private goose-agent
// and streams the response back to the caller. The control plane injects the
// per-VM agent token so external callers only need the control plane Bearer token.
// /health is forwarded without an Authorization header (it is unauthenticated on the agent).
func (cp *ControlPlane) proxyAgentEndpoint(w http.ResponseWriter, r *http.Request, vmID, agentPath string) {
	cp.mu.RLock()
	v, ok := cp.vms[vmID]
	cp.mu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"vm not found"}`, http.StatusNotFound)
		return
	}

	targetURL := fmt.Sprintf("http://%s:%d%s", v.GuestIP, agentPort, agentPath)
	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"proxy request: %v"}`, err), http.StatusInternalServerError)
		return
	}

	if ct := r.Header.Get("Content-Type"); ct != "" {
		proxyReq.Header.Set("Content-Type", ct)
	}
	// /health is always unauthenticated on the agent side.
	if agentPath != "/health" && v.agentToken != "" {
		proxyReq.Header.Set("Authorization", "Bearer "+v.agentToken)
	}

	resp, err := cp.agentHTTPClient.Do(proxyReq)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"agent unreachable: %v"}`, err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vals := range resp.Header {
		for _, val := range vals {
			w.Header().Add(k, val)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// buildAgentURL returns the agent_url field for VMInfo.
// When EPHEMERA_PUBLIC_URL is configured, returns the control plane proxy path
// so external clients can reach the agent through the control plane.
// Otherwise falls back to the VM's private IP (backward-compatible).
func buildAgentURL(vmID, guestIP string) string {
	if publicURL != "" {
		return publicURL + "/vms/" + vmID
	}
	return fmt.Sprintf("http://%s:%d", guestIP, agentPort)
}

// profileConfigPaths resolves the goose.yaml and goose-secrets.yaml paths for a given profile.
// An empty profile returns the ControlPlane's default paths (existing behavior).
// Returns HTTP 400-appropriate errors if the profile name is unsafe or the files are missing.
func (cp *ControlPlane) profileConfigPaths(profile string) (configPath, secretsPath string, err error) {
	if profile == "" {
		return cp.gooseConfigPath, cp.gooseSecretsPath, nil
	}
	// Reject path traversal attempts.
	if strings.ContainsAny(profile, "/\\") || profile == ".." {
		return "", "", fmt.Errorf("invalid profile name: %q", profile)
	}
	dir := filepath.Join(cp.workDir, "configs", "profiles", profile)
	configPath = filepath.Join(dir, "goose.yaml")
	secretsPath = filepath.Join(dir, "goose-secrets.yaml")
	if _, e := os.Stat(configPath); os.IsNotExist(e) {
		return "", "", fmt.Errorf("profile %q not found (missing goose.yaml)", profile)
	}
	if _, e := os.Stat(secretsPath); os.IsNotExist(e) {
		return "", "", fmt.Errorf("profile %q not found (missing goose-secrets.yaml)", profile)
	}
	return configPath, secretsPath, nil
}

func (cp *ControlPlane) spawnVM(w http.ResponseWriter, r *http.Request) {
	// Parse optional request body. An empty body is valid (uses default profile).
	var req VMSpawnRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"invalid request body: %v"}`, err), http.StatusBadRequest)
			return
		}
	}
	req.Profile = strings.TrimSpace(req.Profile)

	configPath, secretsPath, err := cp.profileConfigPaths(req.Profile)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusBadRequest)
		return
	}

	agentToken, err := generateAgentToken()
	if err != nil {
		http.Error(w, fmt.Sprintf("token generation failed: %v", err), http.StatusInternalServerError)
		return
	}

	vmID := fmt.Sprintf("vm-%d", time.Now().UnixNano())

	tapDevice, guestIP, macAddr, err := cp.netManager.Allocate()
	if err != nil {
		http.Error(w, fmt.Sprintf("network allocation failed: %v", err), http.StatusInternalServerError)
		return
	}

	diskPath, err := cp.provisioner.CloneDisk(vmID)
	if err != nil {
		cp.netManager.Release(tapDevice, guestIP)
		http.Error(w, fmt.Sprintf("disk provisioning failed: %v", err), http.StatusInternalServerError)
		return
	}

	if err := cp.provisioner.PrepareVM(vmID, storage.VMPrepareOptions{
		HostConfigPath:  configPath,
		HostSecretsPath: secretsPath,
		AgentToken:      agentToken,
	}); err != nil {
		cp.provisioner.CleanupDisk(vmID)
		cp.netManager.Release(tapDevice, guestIP)
		http.Error(w, fmt.Sprintf("VM preparation failed: %v", err), http.StatusInternalServerError)
		return
	}

	socketPath := fmt.Sprintf("/tmp/firecracker-%s.sock", vmID)
	vsockPath := fmt.Sprintf("/tmp/firecracker-vsock-%s.sock", vmID)
	os.Remove(socketPath)

	machine, err := vm.StartMachine(context.Background(), vm.VMConfig{
		VMID:           vmID,
		SocketPath:     socketPath,
		FirecrackerBin: cp.firecrackerPath,
		KernelPath:     cp.kernelPath,
		RootfsPath:     diskPath,
		TapDevice:      tapDevice,
		MacAddress:     macAddr,
		GuestIP:        guestIP,
		GatewayIP:      "10.0.1.1",
		VsockUDSPath:   vsockPath,
	})
	if err != nil {
		cp.provisioner.CleanupDisk(vmID)
		cp.netManager.Release(tapDevice, guestIP)
		http.Error(w, fmt.Sprintf("VM start failed: %v", err), http.StatusInternalServerError)
		return
	}

	info := VMInfo{
		VMID:     vmID,
		GuestIP:  guestIP,
		AgentURL: buildAgentURL(vmID, guestIP),
		Profile:  req.Profile,
	}

	cp.mu.Lock()
	cp.vms[vmID] = &runningVM{
		VMInfo:     info,
		agentToken: agentToken,
		diskPath:   diskPath,
		vsockPath:  vsockPath,
		machine:    machine,
		tapDevice:  tapDevice,
		socketPath: socketPath,
	}
	cp.mu.Unlock()

	log.Printf("VM [%s] booting at %s — waiting for goose-agent...", vmID, info.AgentURL)
	if err := waitForAgent(guestIP, 60*time.Second); err != nil {
		cp.destroyVM(vmID)
		http.Error(w, fmt.Sprintf("goose-agent not ready: %v", err), http.StatusInternalServerError)
		return
	}
	log.Printf("VM [%s] ready — agent: %s  profile: %q", vmID, info.AgentURL, req.Profile)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(VMSpawnResult{VMInfo: info, AgentToken: agentToken})
}

func (cp *ControlPlane) listVMs(w http.ResponseWriter, _ *http.Request) {
	cp.mu.RLock()
	list := make([]VMInfo, 0, len(cp.vms))
	for _, v := range cp.vms {
		list = append(list, v.VMInfo)
	}
	cp.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (cp *ControlPlane) stopVM(w http.ResponseWriter, vmID string) {
	cp.mu.RLock()
	_, ok := cp.vms[vmID]
	cp.mu.RUnlock()

	if !ok {
		http.Error(w, "VM not found", http.StatusNotFound)
		return
	}
	cp.destroyVM(vmID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "stopped", "vm_id": vmID})
}

func (cp *ControlPlane) destroyVM(vmID string) {
	cp.mu.Lock()
	v, ok := cp.vms[vmID]
	if ok {
		delete(cp.vms, vmID)
	}
	cp.mu.Unlock()

	if !ok {
		return
	}
	// StopVMM sends SIGTERM and waits for Firecracker to exit.
	v.machine.StopVMM()
	os.Remove(v.socketPath)
	os.Remove(fmt.Sprintf("/tmp/fc-%s-log.fifo", vmID))
	if v.vsockPath != "" {
		os.Remove(v.vsockPath)
	}

	if v.dmSnapshot != nil {
		// COW-restored VM: release dm-snapshot device, loop device, and exception store.
		storage.TeardownDMSnapshot(v.dmSnapshot)
	} else if v.bindMountTarget != "" {
		// Bind-mount restored VM (legacy): lazy-umount + remove per-restore disk copy.
		storage.TeardownBindMount(v.bindMountTarget, v.diskPath)
	} else if v.diskPath != "" {
		if err := os.Remove(v.diskPath); err != nil && !os.IsNotExist(err) {
			log.Printf("Warning: failed to delete disk %s for VM [%s]: %v", v.diskPath, vmID, err)
		}
	}
	cp.netManager.Release(v.tapDevice, v.GuestIP)
	log.Printf("VM [%s] destroyed.", vmID)
}

// DestroyAll stops all running VMs. Called on daemon shutdown.
func (cp *ControlPlane) DestroyAll() {
	cp.mu.RLock()
	ids := make([]string, 0, len(cp.vms))
	for id := range cp.vms {
		ids = append(ids, id)
	}
	cp.mu.RUnlock()
	for _, id := range ids {
		cp.destroyVM(id)
	}
}

func waitForAgent(guestIP string, timeout time.Duration) error {
	url := fmt.Sprintf("http://%s:%d/health", guestIP, agentPort)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("agent not ready after %v", timeout)
}

// ---- Snapshot types ----

// SnapshotInfo is the public representation of a snapshot (no sensitive fields).
type SnapshotInfo struct {
	SnapshotID     string    `json:"snapshot_id"`
	SourceVMID     string    `json:"source_vm_id"`
	Profile        string    `json:"profile,omitempty"`
	SnapshotType   string    `json:"snapshot_type"`              // "full" | "diff"
	BaseSnapshotID string    `json:"base_snapshot_id,omitempty"` // set for diff snapshots
	CreatedAt      time.Time `json:"created_at"`
}

// VMRestoreResult is returned by POST /snapshots/{id}/restore.
type VMRestoreResult struct {
	VMSpawnResult
	SourceSnapshotID string `json:"source_snapshot_id"`
}

// SnapshotRequest is the optional body for POST /vms/{id}/snapshot.
type SnapshotRequest struct {
	StopAfter bool   `json:"stop_after"`
	Type      string `json:"type,omitempty"` // "full" | "diff" | "" (auto-detect)
}

func snapshotInfoFrom(meta storage.SnapshotMetadata) SnapshotInfo {
	return SnapshotInfo{
		SnapshotID:     meta.SnapshotID,
		SourceVMID:     meta.SourceVMID,
		Profile:        meta.Profile,
		SnapshotType:   meta.SnapshotType,
		BaseSnapshotID: meta.BaseSnapshotID,
		CreatedAt:      meta.CreatedAt,
	}
}

// ---- Snapshot handlers ----

// GET /snapshots
func (cp *ControlPlane) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	cp.snapshotsMu.RLock()
	list := make([]SnapshotInfo, 0, len(cp.snapshots))
	for _, meta := range cp.snapshots {
		list = append(list, snapshotInfoFrom(meta))
	}
	cp.snapshotsMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// handleSnapshotItem routes POST /snapshots/{id}/restore and DELETE /snapshots/{id}.
func (cp *ControlPlane) handleSnapshotItem(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/snapshots/")

	if strings.HasSuffix(path, "/restore") {
		snapID := strings.TrimSuffix(path, "/restore")
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		cp.restoreSnapshot(w, snapID)
		return
	}

	// DELETE /snapshots/{id}
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE required", http.StatusMethodNotAllowed)
		return
	}
	cp.deleteSnapshot(w, path)
}

// resolveSnapshotType determines whether to create a Full or Diff snapshot.
// "" (auto): Full if no prior Full snapshot of this VM exists; Diff otherwise.
// "full" / "diff": explicit override. "diff" without a base returns an error.
// Returns (snapshotType, baseSnapshotID, error).
func (cp *ControlPlane) resolveSnapshotType(reqType, vmID string) (string, string, error) {
	switch strings.ToLower(reqType) {
	case "full":
		return "full", "", nil
	case "diff":
		base := cp.latestFullSnapshot(vmID)
		if base == nil {
			return "", "", fmt.Errorf("no full snapshot found for VM %s; create a full snapshot first", vmID)
		}
		return "diff", base.SnapshotID, nil
	default: // auto
		base := cp.latestFullSnapshot(vmID)
		if base == nil {
			return "full", "", nil
		}
		return "diff", base.SnapshotID, nil
	}
}

// latestFullSnapshot returns the most recent full snapshot for vmID, or nil if none.
func (cp *ControlPlane) latestFullSnapshot(vmID string) *storage.SnapshotMetadata {
	cp.snapshotsMu.RLock()
	defer cp.snapshotsMu.RUnlock()
	var latest *storage.SnapshotMetadata
	for i := range cp.snapshots {
		s := cp.snapshots[i]
		if s.SourceVMID == vmID && s.SnapshotType == "full" {
			if latest == nil || s.CreatedAt.After(latest.CreatedAt) {
				latest = &s
			}
		}
	}
	return latest
}

// POST /vms/{vm_id}/snapshot
func (cp *ControlPlane) createSnapshot(w http.ResponseWriter, r *http.Request, vmID string) {
	var req SnapshotRequest
	if r.Body != nil && r.ContentLength != 0 {
		json.NewDecoder(r.Body).Decode(&req)
	}

	cp.mu.RLock()
	v, ok := cp.vms[vmID]
	cp.mu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"VM not found"}`, http.StatusNotFound)
		return
	}

	// Determine snapshot type (full or diff) and base snapshot ID.
	snapType, baseSnapID, err := cp.resolveSnapshotType(req.Type, vmID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusBadRequest)
		return
	}

	snapID := fmt.Sprintf("snap-%d", time.Now().UnixNano())
	snapDir := storage.SnapshotDir(cp.workDir, snapID)
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to create snapshot dir: %v"}`, err), http.StatusInternalServerError)
		return
	}

	memPath := filepath.Join(snapDir, "memory.bin")
	statPath := filepath.Join(snapDir, "state.bin")

	log.Printf("Snapshot [%s] (%s): pausing VM [%s]...", snapID, snapType, vmID)
	if err := v.machine.PauseVM(context.Background()); err != nil {
		os.RemoveAll(snapDir)
		http.Error(w, fmt.Sprintf(`{"error":"failed to pause VM: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// Build SDK opts: Diff snapshots pass SnapshotType="Diff" to Firecracker.
	var snapOpts []firecracker.CreateSnapshotOpt
	if snapType == "diff" {
		snapOpts = append(snapOpts, func(p *ops.CreateSnapshotParams) {
			p.Body.SnapshotType = models.SnapshotCreateParamsSnapshotTypeDiff
		})
		log.Printf("Snapshot [%s]: creating Diff snapshot (base: %s)...", snapID, baseSnapID)
	} else {
		log.Printf("Snapshot [%s]: creating Full snapshot...", snapID)
	}

	if err := v.machine.CreateSnapshot(context.Background(), memPath, statPath, snapOpts...); err != nil {
		v.machine.ResumeVM(context.Background())
		os.RemoveAll(snapDir)
		http.Error(w, fmt.Sprintf(`{"error":"failed to create snapshot: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// Copy disk while VM is still paused (ensures consistent state).
	// Diff snapshots still copy the full rootfs — rootfs diff is a future optimization.
	diskPath := filepath.Join("/tmp/goose-workspaces", vmID+".ext4")
	log.Printf("Snapshot [%s]: copying disk...", snapID)
	diskCopyPath, err := storage.CopyDiskToSnapshot(diskPath, snapDir)
	if err != nil {
		v.machine.ResumeVM(context.Background())
		os.RemoveAll(snapDir)
		http.Error(w, fmt.Sprintf(`{"error":"failed to copy disk: %v"}`, err), http.StatusInternalServerError)
		return
	}

	if !req.StopAfter {
		log.Printf("Snapshot [%s]: resuming VM [%s]...", snapID, vmID)
		if err := v.machine.ResumeVM(context.Background()); err != nil {
			log.Printf("Warning: failed to resume VM [%s] after snapshot: %v", vmID, err)
		}
	} else {
		log.Printf("Snapshot [%s]: stop_after=true, destroying VM [%s]", snapID, vmID)
		cp.destroyVM(vmID)
	}

	// Firecracker v1.x embeds the TAP device name AND vsock UDS path in state.bin.
	// On restore, Firecracker reopens both by the exact names/paths from the snapshot.
	meta := storage.SnapshotMetadata{
		SnapshotID:     snapID,
		SourceVMID:     vmID,
		Profile:        v.VMInfo.Profile,
		SnapshotType:   snapType,
		BaseSnapshotID: baseSnapID,
		GuestIP:        v.VMInfo.GuestIP,
		TapDevice:      v.tapDevice,
		MacAddr:        deriveMACFromTap(v.tapDevice),
		VsockPath:      v.vsockPath,
		AgentToken:     v.agentToken,
		DiskPath:       diskPath,
		MemFilePath:    memPath,
		StatFilePath:   statPath,
		DiskCopyPath:   diskCopyPath,
		CreatedAt:      time.Now().UTC(),
	}

	if err := storage.SaveMetadata(snapDir, meta); err != nil {
		log.Printf("Warning: failed to save snapshot metadata: %v", err)
	}

	cp.snapshotsMu.Lock()
	cp.snapshots[snapID] = meta
	cp.snapshotsMu.Unlock()

	log.Printf("Snapshot [%s] (%s) created from VM [%s]", snapID, snapType, vmID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(snapshotInfoFrom(meta))
}

// deriveMACFromTap reproduces the MAC address from a tap device name (e.g. "tap3").
// Must match the formula in network.Manager.Allocate().
func deriveMACFromTap(tapDevice string) string {
	var tapID int
	fmt.Sscanf(tapDevice, "tap%d", &tapID)
	return fmt.Sprintf("AA:FC:00:00:%02X:%02X", tapID/256, tapID%256)
}

// POST /snapshots/{snapshot_id}/restore
func (cp *ControlPlane) restoreSnapshot(w http.ResponseWriter, snapID string) {
	cp.snapshotsMu.RLock()
	meta, ok := cp.snapshots[snapID]
	cp.snapshotsMu.RUnlock()
	if !ok {
		http.Error(w, `{"error":"snapshot not found"}`, http.StatusNotFound)
		return
	}

	// Prevent restoring if the source VM is still running (its disk is in active use).
	cp.mu.RLock()
	for id := range cp.vms {
		if id == meta.SourceVMID {
			cp.mu.RUnlock()
			http.Error(w, fmt.Sprintf(`{"error":"source VM %s is still running (delete it first)"}`, meta.SourceVMID), http.StatusConflict)
			return
		}
	}
	cp.mu.RUnlock()

	newVMID := fmt.Sprintf("vm-%d", time.Now().UnixNano())
	exceptionStorePath := filepath.Join(cp.provisioner.WorkspaceDir, newVMID+".cow")
	socketPath := fmt.Sprintf("/tmp/firecracker-%s.sock", newVMID)
	os.Remove(socketPath)

	// Vsock UDS path: use the original path from the snapshot.
	os.Remove(meta.VsockPath)

	// Allocate any available IP — the guest will be reconfigured to this IP via vsock.
	log.Printf("Restore [%s]: allocating network (TAP: %s, MAC: %s)...", snapID, meta.TapDevice, meta.MacAddr)
	tapDevice, newGuestIP, err := cp.netManager.AllocateForRestore(meta.TapDevice, meta.MacAddr)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"network allocation failed: %v"}`, err), http.StatusConflict)
		return
	}

	// Serialize dm-snapshot setup + Firecracker open so each restore sees its own COW device.
	cp.restoreMu.Lock()

	log.Printf("Restore [%s]: setting up dm-snapshot COW (base: %s, store: %s)...", snapID, meta.DiskCopyPath, exceptionStorePath)
	dmInfo, err := storage.SetupDMSnapshot(meta.DiskCopyPath, exceptionStorePath, meta.DiskPath)
	if err != nil {
		cp.restoreMu.Unlock()
		cp.netManager.Release(tapDevice, newGuestIP)
		log.Printf("Restore [%s]: dm-snapshot failed (%v), falling back to bind mount", snapID, err)
		// Fallback: use the existing bind-mount approach if dm-snapshot is unavailable.
		newDiskPath := filepath.Join(cp.provisioner.WorkspaceDir, newVMID+".ext4")
		cp.restoreMu.Lock()
		if bmErr := storage.SetupBindMount(meta.DiskCopyPath, newDiskPath, meta.DiskPath); bmErr != nil {
			cp.restoreMu.Unlock()
			cp.netManager.Release(tapDevice, newGuestIP)
			http.Error(w, fmt.Sprintf(`{"error":"failed to set up disk: dm-snapshot: %v; bind-mount fallback: %v"}`, err, bmErr), http.StatusInternalServerError)
			return
		}
		// Continue with bind-mount path (legacy runningVM fields).
		cp.restoreMu.Unlock()
		cp.restoreLegacyBindMount(w, snapID, meta, newVMID, newDiskPath, tapDevice, newGuestIP, socketPath)
		return
	}

	// For diff snapshots: merge base memory + diff memory into a temp file.
	// The merged file is used for restoration and deleted when the VM is destroyed.
	memFileToUse := meta.MemFilePath
	var mergedMemPath string
	if meta.SnapshotType == "diff" {
		cp.snapshotsMu.RLock()
		base, baseOK := cp.snapshots[meta.BaseSnapshotID]
		cp.snapshotsMu.RUnlock()
		if !baseOK {
			cp.restoreMu.Unlock()
			storage.TeardownDMSnapshot(dmInfo)
			cp.netManager.Release(tapDevice, newGuestIP)
			http.Error(w, fmt.Sprintf(`{"error":"base snapshot %s not found (was it deleted?)"}`, meta.BaseSnapshotID), http.StatusConflict)
			return
		}
		mergedMemPath = filepath.Join(cp.workDir, "tmp", newVMID+"-merged.bin")
		os.MkdirAll(filepath.Dir(mergedMemPath), 0755)
		log.Printf("Restore [%s]: merging base memory (%s) + diff (%s)...", snapID, base.MemFilePath, meta.MemFilePath)
		if err := storage.MergeMemoryDiff(base.MemFilePath, meta.MemFilePath, mergedMemPath); err != nil {
			cp.restoreMu.Unlock()
			storage.TeardownDMSnapshot(dmInfo)
			cp.netManager.Release(tapDevice, newGuestIP)
			http.Error(w, fmt.Sprintf(`{"error":"failed to merge diff snapshot: %v"}`, err), http.StatusInternalServerError)
			return
		}
		memFileToUse = mergedMemPath
	}

	log.Printf("Restore [%s]: starting VM [%s] from snapshot (%s)...", snapID, newVMID, meta.SnapshotType)
	machine, err := vm.RestoreMachine(context.Background(), vm.VMConfig{
		VMID:           newVMID,
		SocketPath:     socketPath,
		FirecrackerBin: cp.firecrackerPath,
		RootfsPath:     meta.DiskPath,
		TapDevice:      tapDevice,
		MacAddress:     meta.MacAddr,
		GuestIP:        newGuestIP,
		GatewayIP:      "10.0.1.1",
		// VsockUDSPath intentionally empty: snapshot state recreates vsock at meta.VsockPath
	}, memFileToUse, meta.StatFilePath)

	cp.restoreMu.Unlock()
	if mergedMemPath != "" {
		os.Remove(mergedMemPath) // temp merged file no longer needed after RestoreMachine
	}

	if err != nil {
		storage.TeardownDMSnapshot(dmInfo)
		cp.netManager.Release(tapDevice, newGuestIP)
		http.Error(w, fmt.Sprintf(`{"error":"failed to restore VM: %v"}`, err), http.StatusInternalServerError)
		return
	}

	// Firecracker has restored vsock at meta.VsockPath. Reconfigure the guest's IP.
	log.Printf("Restore [%s]: reconfiguring guest IP %s → %s via vsock %s...", snapID, meta.GuestIP, newGuestIP, meta.VsockPath)
	if err := vm.ReconfigureGuestIP(meta.VsockPath, newGuestIP+"/24", "10.0.1.1"); err != nil {
		log.Printf("Restore [%s]: vsock IP reconfigure failed: %v", snapID, err)
		machine.StopVMM()
		storage.TeardownDMSnapshot(dmInfo)
		cp.netManager.Release(tapDevice, newGuestIP)
		http.Error(w, fmt.Sprintf(`{"error":"vsock IP reconfigure failed: %v"}`, err), http.StatusInternalServerError)
		return
	}
	log.Printf("Restore [%s]: guest IP reconfigured to %s (COW exception store: %s)", snapID, newGuestIP, exceptionStorePath)

	info := VMInfo{
		VMID:     newVMID,
		GuestIP:  newGuestIP,
		AgentURL: buildAgentURL(newVMID, newGuestIP),
		Profile:  meta.Profile,
	}

	cp.mu.Lock()
	cp.vms[newVMID] = &runningVM{
		VMInfo:     info,
		agentToken: meta.AgentToken,
		diskPath:   exceptionStorePath, // only the COW store needs cleanup (not a full disk copy)
		dmSnapshot: dmInfo,
		vsockPath:  meta.VsockPath,
		machine:    machine,
		tapDevice:  tapDevice,
		socketPath: socketPath,
	}
	cp.mu.Unlock()

	log.Printf("Restore [%s]: waiting for goose-agent at %s...", snapID, info.AgentURL)
	if err := waitForAgent(newGuestIP, 30*time.Second); err != nil {
		cp.destroyVM(newVMID)
		http.Error(w, fmt.Sprintf(`{"error":"goose-agent not ready after restore: %v"}`, err), http.StatusInternalServerError)
		return
	}
	log.Printf("Restore [%s]: VM [%s] ready — agent: %s", snapID, newVMID, info.AgentURL)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(VMRestoreResult{
		VMSpawnResult:    VMSpawnResult{VMInfo: info, AgentToken: meta.AgentToken},
		SourceSnapshotID: snapID,
	})
}

// restoreLegacyBindMount handles the fallback path when dm-snapshot is unavailable.
// It uses the original bind-mount approach (full 700 MB copy per restore).
func (cp *ControlPlane) restoreLegacyBindMount(
	w http.ResponseWriter,
	snapID string, meta storage.SnapshotMetadata,
	newVMID, newDiskPath, tapDevice, newGuestIP, socketPath string,
) {
	// Diff memory merge if needed.
	memFileToUse := meta.MemFilePath
	var mergedMemPath string
	if meta.SnapshotType == "diff" {
		cp.snapshotsMu.RLock()
		base, baseOK := cp.snapshots[meta.BaseSnapshotID]
		cp.snapshotsMu.RUnlock()
		if !baseOK {
			storage.TeardownBindMount(meta.DiskPath, newDiskPath)
			cp.netManager.Release(tapDevice, newGuestIP)
			http.Error(w, fmt.Sprintf(`{"error":"base snapshot %s not found"}`, meta.BaseSnapshotID), http.StatusConflict)
			return
		}
		mergedMemPath = filepath.Join(cp.workDir, "tmp", newVMID+"-merged.bin")
		os.MkdirAll(filepath.Dir(mergedMemPath), 0755)
		if err := storage.MergeMemoryDiff(base.MemFilePath, meta.MemFilePath, mergedMemPath); err != nil {
			storage.TeardownBindMount(meta.DiskPath, newDiskPath)
			cp.netManager.Release(tapDevice, newGuestIP)
			http.Error(w, fmt.Sprintf(`{"error":"failed to merge diff: %v"}`, err), http.StatusInternalServerError)
			return
		}
		memFileToUse = mergedMemPath
	}

	machine, err := vm.RestoreMachine(context.Background(), vm.VMConfig{
		VMID:           newVMID,
		SocketPath:     socketPath,
		FirecrackerBin: cp.firecrackerPath,
		RootfsPath:     meta.DiskPath,
		TapDevice:      tapDevice,
		MacAddress:     meta.MacAddr,
		GuestIP:        newGuestIP,
		GatewayIP:      "10.0.1.1",
	}, memFileToUse, meta.StatFilePath)
	if mergedMemPath != "" {
		os.Remove(mergedMemPath)
	}
	if err != nil {
		storage.TeardownBindMount(meta.DiskPath, newDiskPath)
		cp.netManager.Release(tapDevice, newGuestIP)
		http.Error(w, fmt.Sprintf(`{"error":"failed to restore VM: %v"}`, err), http.StatusInternalServerError)
		return
	}

	if err := vm.ReconfigureGuestIP(meta.VsockPath, newGuestIP+"/24", "10.0.1.1"); err != nil {
		machine.StopVMM()
		storage.TeardownBindMount(meta.DiskPath, newDiskPath)
		cp.netManager.Release(tapDevice, newGuestIP)
		http.Error(w, fmt.Sprintf(`{"error":"vsock IP reconfigure failed: %v"}`, err), http.StatusInternalServerError)
		return
	}

	info := VMInfo{
		VMID:     newVMID,
		GuestIP:  newGuestIP,
		AgentURL: buildAgentURL(newVMID, newGuestIP),
		Profile:  meta.Profile,
	}
	cp.mu.Lock()
	cp.vms[newVMID] = &runningVM{
		VMInfo:          info,
		agentToken:      meta.AgentToken,
		diskPath:        newDiskPath,
		bindMountTarget: meta.DiskPath,
		vsockPath:       meta.VsockPath,
		machine:         machine,
		tapDevice:       tapDevice,
		socketPath:      socketPath,
	}
	cp.mu.Unlock()

	if err := waitForAgent(newGuestIP, 30*time.Second); err != nil {
		cp.destroyVM(newVMID)
		http.Error(w, fmt.Sprintf(`{"error":"goose-agent not ready: %v"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(VMRestoreResult{
		VMSpawnResult:    VMSpawnResult{VMInfo: info, AgentToken: meta.AgentToken},
		SourceSnapshotID: snapID,
	})
}

// DELETE /snapshots/{snapshot_id}
func (cp *ControlPlane) deleteSnapshot(w http.ResponseWriter, snapID string) {
	// Check for diff snapshots that reference this snapshot as their base.
	// Deleting a base would make those diffs un-restorable.
	cp.snapshotsMu.RLock()
	for id, snap := range cp.snapshots {
		if snap.BaseSnapshotID == snapID {
			cp.snapshotsMu.RUnlock()
			http.Error(w, fmt.Sprintf(`{"error":"cannot delete: snapshot %s is the base for diff snapshot %s — delete the diff first"}`, snapID, id), http.StatusConflict)
			return
		}
	}
	cp.snapshotsMu.RUnlock()

	cp.snapshotsMu.Lock()
	meta, ok := cp.snapshots[snapID]
	if ok {
		delete(cp.snapshots, snapID)
	}
	cp.snapshotsMu.Unlock()

	if !ok {
		http.Error(w, `{"error":"snapshot not found"}`, http.StatusNotFound)
		return
	}

	snapDir := storage.SnapshotDir(cp.workDir, snapID)
	if err := storage.DeleteSnapshot(snapDir); err != nil {
		log.Printf("Warning: failed to delete snapshot dir %s: %v", snapDir, err)
	}

	log.Printf("Snapshot [%s] (%s, from VM %s) deleted.", snapID, meta.SnapshotType, meta.SourceVMID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted", "snapshot_id": snapID})
}
