package env

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuickStartConfigCreatesMissingFileAndPreservesExistingFile(t *testing.T) {
	rootDir, err := findRootDir()
	if err != nil {
		t.Fatalf("find repository root: %v", err)
	}
	scriptPath := filepath.Join(rootDir, "test", "e2e", "hack", "quickstart-config.sh")
	configPath := filepath.Join(t.TempDir(), ".fastctl", "config.json")

	output := runQuickStartConfig(t, scriptPath, configPath, "19090", "18081")
	if strings.TrimSpace(output) != "created" {
		t.Fatalf("first invocation output = %q, want created", output)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	var config map[string]string
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("parse generated config: %v\n%s", err, data)
	}
	if got := config["endpoint"]; got != "localhost:19090" {
		t.Fatalf("endpoint = %q, want localhost:19090", got)
	}
	if got := config["proxy-endpoint"]; got != "http://localhost:18081" {
		t.Fatalf("proxy endpoint = %q, want http://localhost:18081", got)
	}
	if got := config["namespace"]; got != "fast-sandbox" {
		t.Fatalf("namespace = %q, want fast-sandbox", got)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat generated config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("config permissions = %#o, want 0600", got)
	}

	original := append([]byte(nil), data...)
	output = runQuickStartConfig(t, scriptPath, configPath, "29090", "28080")
	if strings.TrimSpace(output) != "existing" {
		t.Fatalf("second invocation output = %q, want existing", output)
	}
	preserved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read preserved config: %v", err)
	}
	if string(preserved) != string(original) {
		t.Fatalf("existing config was modified:\nold: %s\nnew: %s", original, preserved)
	}
}

func TestQuickStartPrintExplainsConfigState(t *testing.T) {
	rootDir, err := findRootDir()
	if err != nil {
		t.Fatalf("find repository root: %v", err)
	}
	scriptPath := filepath.Join(rootDir, "test", "e2e", "hack", "quickstart-print.sh")

	created := runScript(t, scriptPath,
		"pool-a", "sandbox-a", "execd", "created", "19090", "18081",
	)
	if !strings.Contains(created, "created .fastctl/config.json") {
		t.Fatalf("created output does not describe generated config:\n%s", created)
	}
	if strings.Contains(created, "export FAST_SANDBOX_") {
		t.Fatalf("created output should not require endpoint exports:\n%s", created)
	}
	assertQuickStartExamplesUseConfiguredEndpoints(t, created)

	existing := runScript(t, scriptPath,
		"pool-a", "sandbox-a", "execd", "existing", "19090", "18081",
	)
	for _, want := range []string{
		"existing .fastctl/config.json was preserved",
		"export FAST_SANDBOX_ENDPOINT=localhost:19090",
		"export FAST_SANDBOX_PROXY_ENDPOINT=http://localhost:18081",
		"QUICKSTART_FASTPATH_PORT=19090 QUICKSTART_PROXY_PORT=18081",
	} {
		if !strings.Contains(existing, want) {
			t.Fatalf("existing output missing %q:\n%s", want, existing)
		}
	}
	assertQuickStartExamplesUseConfiguredEndpoints(t, existing)
}

func assertQuickStartExamplesUseConfiguredEndpoints(t *testing.T, output string) {
	t.Helper()
	for _, forbidden := range []string{
		"bin/fastctl --endpoint",
		"bin/fastctl --proxy-endpoint",
		"--proxy-endpoint http://localhost:",
		"kubectl wait --for=jsonpath='{.status.dataPlaneState}'=Ready",
	} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("Quick Start output contains obsolete endpoint/readiness instruction %q:\n%s", forbidden, output)
		}
	}
	for _, want := range []string{
		"Endpoint precedence: explicit flags > environment > config file > defaults.",
		"example commands intentionally omit --endpoint and --proxy-endpoint",
		"bin/fastctl run sandbox-a",
		"bin/fastctl opensandbox exec sandbox-a",
		"bin/fastctl delete sandbox-a",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("Quick Start output missing %q:\n%s", want, output)
		}
	}
}

func runQuickStartConfig(t *testing.T, scriptPath, configPath, fastPathPort, proxyPort string) string {
	t.Helper()
	command := exec.Command("bash", scriptPath, configPath)
	command.Env = append(os.Environ(),
		"QUICKSTART_FASTPATH_PORT="+fastPathPort,
		"QUICKSTART_PROXY_PORT="+proxyPort,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run quickstart config helper: %v\n%s", err, output)
	}
	return string(output)
}

func runScript(t *testing.T, scriptPath string, args ...string) string {
	t.Helper()
	output, err := exec.Command("bash", append([]string{scriptPath}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("run %s: %v\n%s", filepath.Base(scriptPath), err, output)
	}
	return string(output)
}
