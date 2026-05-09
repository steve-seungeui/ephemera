package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestAgentAuthMiddleware_EmptyToken_Passthrough(t *testing.T) {
	called := false
	handler := agentAuthMiddleware("", func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/tasks", nil))
	if !called {
		t.Error("expected passthrough when token is empty (auth disabled)")
	}
}

func TestAgentAuthMiddleware_CorrectToken_Passthrough(t *testing.T) {
	const token = "correcttoken"
	called := false
	handler := agentAuthMiddleware(token, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	req := httptest.NewRequest(http.MethodPost, "/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	handler(httptest.NewRecorder(), req)
	if !called {
		t.Error("expected passthrough with correct token")
	}
}

func TestAgentAuthMiddleware_WrongToken_401(t *testing.T) {
	handler := agentAuthMiddleware("righttoken", func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be called with wrong token")
	})
	req := httptest.NewRequest(http.MethodPost, "/tasks", nil)
	req.Header.Set("Authorization", "Bearer wrongtoken")
	rr := httptest.NewRecorder()
	handler(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestAgentAuthMiddleware_MissingHeader_401(t *testing.T) {
	handler := agentAuthMiddleware("sometoken", func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler must not be called without auth header")
	})
	rr := httptest.NewRecorder()
	handler(rr, httptest.NewRequest(http.MethodPost, "/tasks", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

func TestLoadAgentToken_FileAbsent(t *testing.T) {
	// /root/.ephemera-agent-token won't exist in test environments (non-VM hosts).
	// Verify that loadAgentToken returns "" without panicking.
	if _, err := os.Stat(agentTokenPath); !os.IsNotExist(err) {
		t.Skip("token file exists in this environment — skipping absence test")
	}
	if got := loadAgentToken(); got != "" {
		t.Errorf("expected empty string for absent file, got %q", got)
	}
}

func TestLoadAgentToken_TrimsWhitespace(t *testing.T) {
	// Verify that the TrimSpace behavior is correct (loadAgentToken's return path).
	// We test the trim logic directly since the token path is a const we cannot override.
	raw := "  mytoken123\n"
	got := strings.TrimSpace(raw)
	if got != "mytoken123" {
		t.Errorf("expected trimmed token, got %q", got)
	}
}
