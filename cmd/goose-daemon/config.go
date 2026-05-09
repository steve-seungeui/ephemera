package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultAPIPort   = 3000
	defaultAgentPort = 8080
)

// APIClient represents a named caller with its own Bearer token.
// Using separate tokens per client allows individual revocation and audit logging.
type APIClient struct {
	Name  string
	Token string
}

var (
	// agentPort is the port goose-agent listens on inside each VM.
	// Must match GOOSE_AGENT_PORT if overridden on the VM side.
	agentPort = envInt("EPHEMERA_AGENT_PORT", defaultAgentPort)

	// apiAddr is the address the control plane API binds to.
	// Default 127.0.0.1:3000 makes the API reachable only on localhost,
	// requiring a reverse proxy for external access.
	// Set EPHEMERA_API_ADDR=0.0.0.0:3000 to bind on all interfaces.
	apiAddr = resolveAPIAddr()

	// publicURL is the externally-reachable base URL of the control plane
	// (no trailing slash). When set, agent_url in VM responses points to the
	// control plane proxy path ("{publicURL}/vms/{vm_id}") instead of the
	// VM's private IP. Example: "https://api.example.com"
	// Set via EPHEMERA_PUBLIC_URL env var.
	publicURL = strings.TrimRight(os.Getenv("EPHEMERA_PUBLIC_URL"), "/")

	// apiClients is the set of authorized callers loaded once at startup.
	// Populated from EPHEMERA_API_TOKENS (multi-client) or EPHEMERA_API_TOKEN
	// (single-client fallback). Empty = authentication disabled.
	apiClients = loadAPIClients()
)

// loadAPIClients parses caller tokens from environment variables.
//
// Multi-client (preferred):
//
//	EPHEMERA_API_TOKENS=alice:token1,bob:token2
//
// Single-client (backward-compatible fallback):
//
//	EPHEMERA_API_TOKEN=token
func loadAPIClients() []APIClient {
	if raw := os.Getenv("EPHEMERA_API_TOKENS"); raw != "" {
		var clients []APIClient
		for _, entry := range strings.Split(raw, ",") {
			entry = strings.TrimSpace(entry)
			idx := strings.Index(entry, ":")
			if idx <= 0 {
				continue
			}
			clients = append(clients, APIClient{
				Name:  entry[:idx],
				Token: entry[idx+1:],
			})
		}
		return clients
	}
	// Fall back to the legacy single-token variable.
	if t := os.Getenv("EPHEMERA_API_TOKEN"); t != "" {
		return []APIClient{{Name: "default", Token: t}}
	}
	return nil
}

// resolveAPIAddr builds the listen address.
// EPHEMERA_API_ADDR (full host:port) takes precedence over EPHEMERA_API_PORT (port only).
func resolveAPIAddr() string {
	if v := os.Getenv("EPHEMERA_API_ADDR"); v != "" {
		return v
	}
	return fmt.Sprintf("127.0.0.1:%d", envInt("EPHEMERA_API_PORT", defaultAPIPort))
}

func envInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultVal
}
