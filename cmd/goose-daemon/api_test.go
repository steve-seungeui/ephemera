package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// ---- profileConfigPaths ----

func newTestCP(t *testing.T) *ControlPlane {
	t.Helper()
	tmp := t.TempDir()
	// Create stub default config files.
	defaultCfg := filepath.Join(tmp, "goose.yaml")
	defaultSec := filepath.Join(tmp, "goose-secrets.yaml")
	os.WriteFile(defaultCfg, []byte("GOOSE_PROVIDER: default\n"), 0644)
	os.WriteFile(defaultSec, []byte("DEFAULT_KEY: x\n"), 0644)
	return &ControlPlane{
		vms:              make(map[string]*runningVM),
		workDir:          tmp,
		gooseConfigPath:  defaultCfg,
		gooseSecretsPath: defaultSec,
	}
}

func TestProfileConfigPaths_EmptyProfile_ReturnsDefaults(t *testing.T) {
	cp := newTestCP(t)
	cfg, sec, err := cp.profileConfigPaths("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != cp.gooseConfigPath {
		t.Errorf("expected default configPath %q, got %q", cp.gooseConfigPath, cfg)
	}
	if sec != cp.gooseSecretsPath {
		t.Errorf("expected default secretsPath %q, got %q", cp.gooseSecretsPath, sec)
	}
}

func TestProfileConfigPaths_ValidProfile_ReturnsPaths(t *testing.T) {
	cp := newTestCP(t)
	profileDir := filepath.Join(cp.workDir, "configs", "profiles", "anthropic")
	os.MkdirAll(profileDir, 0755)
	os.WriteFile(filepath.Join(profileDir, "goose.yaml"), []byte("GOOSE_PROVIDER: anthropic\n"), 0644)
	os.WriteFile(filepath.Join(profileDir, "goose-secrets.yaml"), []byte("ANTHROPIC_API_KEY: sk\n"), 0644)

	cfg, sec, err := cp.profileConfigPaths("anthropic")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != filepath.Join(profileDir, "goose.yaml") {
		t.Errorf("unexpected configPath: %q", cfg)
	}
	if sec != filepath.Join(profileDir, "goose-secrets.yaml") {
		t.Errorf("unexpected secretsPath: %q", sec)
	}
}

func TestProfileConfigPaths_MissingConfigYaml_Error(t *testing.T) {
	cp := newTestCP(t)
	profileDir := filepath.Join(cp.workDir, "configs", "profiles", "partial")
	os.MkdirAll(profileDir, 0755)
	// Only goose-secrets.yaml, no goose.yaml
	os.WriteFile(filepath.Join(profileDir, "goose-secrets.yaml"), []byte("KEY: x\n"), 0644)

	_, _, err := cp.profileConfigPaths("partial")
	if err == nil {
		t.Error("expected error for missing goose.yaml")
	}
}

func TestProfileConfigPaths_MissingSecretsYaml_Error(t *testing.T) {
	cp := newTestCP(t)
	profileDir := filepath.Join(cp.workDir, "configs", "profiles", "partial2")
	os.MkdirAll(profileDir, 0755)
	// Only goose.yaml, no goose-secrets.yaml
	os.WriteFile(filepath.Join(profileDir, "goose.yaml"), []byte("GOOSE_PROVIDER: test\n"), 0644)

	_, _, err := cp.profileConfigPaths("partial2")
	if err == nil {
		t.Error("expected error for missing goose-secrets.yaml")
	}
}

func TestProfileConfigPaths_PathTraversal_Rejected(t *testing.T) {
	cp := newTestCP(t)
	for _, evil := range []string{"../evil", "../../etc", "a/b", `a\b`} {
		_, _, err := cp.profileConfigPaths(evil)
		if err == nil {
			t.Errorf("expected error for path-traversal profile name %q", evil)
		}
	}
}

func TestProfileConfigPaths_DotDot_Rejected(t *testing.T) {
	cp := newTestCP(t)
	_, _, err := cp.profileConfigPaths("..")
	if err == nil {
		t.Error("expected error for profile name '..'")
	}
}

// ---- generateAgentToken ----

func TestGenerateAgentToken_Length(t *testing.T) {
	tok, err := generateAgentToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tok) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(tok))
	}
	if _, err := hex.DecodeString(tok); err != nil {
		t.Errorf("token is not valid hex: %v", err)
	}
}

func TestGenerateAgentToken_Uniqueness(t *testing.T) {
	a, _ := generateAgentToken()
	b, _ := generateAgentToken()
	if a == b {
		t.Error("two tokens should not be identical (probabilistic)")
	}
}
