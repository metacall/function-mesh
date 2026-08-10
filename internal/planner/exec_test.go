package planner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeAndValidateAcceptsAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "service", "main.py")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatalf("create source directory: %v", err)
	}
	if err := os.WriteFile(script, []byte("def main():\n    return 1\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	manifest := PodManifest{
		Version: SupportedSchemaVersion,
		Deployments: []PodDeployment{{
			ID:       7,
			Language: "py",
			Files:    []string{script},
		}},
	}
	if err := normalizeAndValidate(root, &manifest); err != nil {
		t.Fatalf("validate manifest: %v", err)
	}
	if got := manifest.Deployments[0].Files[0]; got != "service/main.py" {
		t.Fatalf("absolute path was not normalized: %q", got)
	}
}

func TestNormalizeAndValidateRejectsEscapedPath(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside.py")
	if err := os.WriteFile(outside, []byte("pass\n"), 0o644); err != nil {
		t.Fatalf("write outside source: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	manifest := PodManifest{
		Version:     SupportedSchemaVersion,
		Deployments: []PodDeployment{{ID: 0, Language: "py", Files: []string{outside}}},
	}
	if err := normalizeAndValidate(root, &manifest); err == nil || !strings.Contains(err.Error(), "escapes source root") {
		t.Fatalf("expected containment error, got %v", err)
	}
}

func TestNormalizeAndValidateRejectsUnknownEdgeAndVersion(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "main.py")
	if err := os.WriteFile(script, []byte("pass\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	manifest := PodManifest{
		Version:     SupportedSchemaVersion,
		Deployments: []PodDeployment{{ID: 0, Language: "py", Files: []string{"main.py"}}},
		Edges:       []Edge{{FromPod: 0, ToPod: 2, Confidence: 0.8}},
	}
	if err := normalizeAndValidate(root, &manifest); err == nil || !strings.Contains(err.Error(), "unknown to_pod") {
		t.Fatalf("expected edge validation error, got %v", err)
	}

	manifest.Edges = nil
	manifest.Version = "2.0"
	if err := normalizeAndValidate(root, &manifest); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected version validation error, got %v", err)
	}
}

func TestExecRunnerReportsMissingBinary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.py"), []byte("pass\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	runner := NewExecRunner(ExecConfig{
		Binary:  filepath.Join(t.TempDir(), "missing-meta-ast"),
		WorkDir: t.TempDir(),
		Timeout: time.Second,
	})
	if _, err := runner.Plan(t.Context(), root); err == nil || !strings.Contains(err.Error(), "meta-ast planning failed") {
		t.Fatalf("expected missing binary error, got %v", err)
	}
}
func TestValidateInputLimits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.py"), []byte("1234"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "two.py"), []byte("5678"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	if err := validateInput(root, 1, 100); err == nil || !strings.Contains(err.Error(), "more than 1 files") {
		t.Fatalf("expected file count error, got %v", err)
	}
	if err := validateInput(root, 10, 7); err == nil || !strings.Contains(err.Error(), "exceeds 7 bytes") {
		t.Fatalf("expected byte limit error, got %v", err)
	}
}
