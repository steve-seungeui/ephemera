package main

import (
	"os"
	"testing"
)

func TestLoadAPIClients_Empty(t *testing.T) {
	os.Unsetenv("EPHEMERA_API_TOKENS")
	os.Unsetenv("EPHEMERA_API_TOKEN")
	if got := loadAPIClients(); len(got) != 0 {
		t.Errorf("expected 0 clients, got %d", len(got))
	}
}

func TestLoadAPIClients_SingleToken(t *testing.T) {
	os.Unsetenv("EPHEMERA_API_TOKENS")
	os.Setenv("EPHEMERA_API_TOKEN", "tok1")
	defer os.Unsetenv("EPHEMERA_API_TOKEN")

	clients := loadAPIClients()
	if len(clients) != 1 {
		t.Fatalf("expected 1 client, got %d", len(clients))
	}
	if clients[0].Name != "default" || clients[0].Token != "tok1" {
		t.Errorf("unexpected client: %+v", clients[0])
	}
}

func TestLoadAPIClients_MultiToken(t *testing.T) {
	os.Setenv("EPHEMERA_API_TOKENS", "alice:tokenA,bob:tokenB")
	defer os.Unsetenv("EPHEMERA_API_TOKENS")

	clients := loadAPIClients()
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(clients))
	}
	if clients[0].Name != "alice" || clients[0].Token != "tokenA" {
		t.Errorf("unexpected client[0]: %+v", clients[0])
	}
	if clients[1].Name != "bob" || clients[1].Token != "tokenB" {
		t.Errorf("unexpected client[1]: %+v", clients[1])
	}
}

func TestLoadAPIClients_MultiTokenTakesPrecedence(t *testing.T) {
	os.Setenv("EPHEMERA_API_TOKENS", "alice:tokenA")
	os.Setenv("EPHEMERA_API_TOKEN", "single")
	defer os.Unsetenv("EPHEMERA_API_TOKENS")
	defer os.Unsetenv("EPHEMERA_API_TOKEN")

	clients := loadAPIClients()
	if len(clients) != 1 || clients[0].Name != "alice" {
		t.Errorf("EPHEMERA_API_TOKENS should take precedence, got: %+v", clients)
	}
}

func TestLoadAPIClients_MalformedEntrySkipped(t *testing.T) {
	os.Setenv("EPHEMERA_API_TOKENS", "alice:tokenA,malformed,bob:tokenB")
	defer os.Unsetenv("EPHEMERA_API_TOKENS")

	clients := loadAPIClients()
	if len(clients) != 2 {
		t.Errorf("expected 2 valid clients (malformed entry skipped), got %d", len(clients))
	}
}

func TestResolveAPIAddr_Default(t *testing.T) {
	os.Unsetenv("EPHEMERA_API_ADDR")
	os.Unsetenv("EPHEMERA_API_PORT")
	if got := resolveAPIAddr(); got != "127.0.0.1:3000" {
		t.Errorf("expected 127.0.0.1:3000, got %q", got)
	}
}

func TestResolveAPIAddr_FromAddr(t *testing.T) {
	os.Setenv("EPHEMERA_API_ADDR", "0.0.0.0:8080")
	defer os.Unsetenv("EPHEMERA_API_ADDR")
	if got := resolveAPIAddr(); got != "0.0.0.0:8080" {
		t.Errorf("expected 0.0.0.0:8080, got %q", got)
	}
}

func TestResolveAPIAddr_FromPort(t *testing.T) {
	os.Unsetenv("EPHEMERA_API_ADDR")
	os.Setenv("EPHEMERA_API_PORT", "9090")
	defer os.Unsetenv("EPHEMERA_API_PORT")
	if got := resolveAPIAddr(); got != "127.0.0.1:9090" {
		t.Errorf("expected 127.0.0.1:9090, got %q", got)
	}
}
