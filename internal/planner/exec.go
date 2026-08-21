package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultBinary         = "meta-ast"
	defaultTimeout        = 30 * time.Second
	defaultMaxOutputBytes = int64(5 << 20)
	defaultMaxInputFiles  = 5000
	defaultMaxInputBytes  = int64(50 << 20)
	maxDiagnosticBytes    = 32 << 10
)

type ExecConfig struct {
	Binary         string
	WorkDir        string
	Timeout        time.Duration
	MaxOutputBytes int64
	MaxInputFiles  int
	MaxInputBytes  int64
}

type ExecRunner struct {
	config ExecConfig
}

func NewExecRunner(config ExecConfig) *ExecRunner {
	if config.Binary == "" {
		config.Binary = defaultBinary
	}
	if config.WorkDir == "" {
		config.WorkDir = os.TempDir()
	}
	if config.Timeout <= 0 {
		config.Timeout = defaultTimeout
	}
	if config.MaxOutputBytes <= 0 {
		config.MaxOutputBytes = defaultMaxOutputBytes
	}
	if config.MaxInputFiles <= 0 {
		config.MaxInputFiles = defaultMaxInputFiles
	}
	if config.MaxInputBytes <= 0 {
		config.MaxInputBytes = defaultMaxInputBytes
	}
	return &ExecRunner{config: config}
}

func (r *ExecRunner) Plan(ctx context.Context, sourceRoot string) (*Result, error) {
	root, err := filepath.Abs(sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve planner source root: %w", err)
	}
	if err := validateInput(root, r.config.MaxInputFiles, r.config.MaxInputBytes); err != nil {
		return nil, err
	}
	outDir, err := os.MkdirTemp(r.config.WorkDir, "function-mesh-plan-*")
	if err != nil {
		return nil, fmt.Errorf("create planner output directory: %w", err)
	}
	defer os.RemoveAll(outDir)

	commandCtx, cancel := context.WithTimeout(ctx, r.config.Timeout)
	defer cancel()

	diagnostics := &limitedBuffer{limit: maxDiagnosticBytes}
	cmd := exec.CommandContext(commandCtx, r.config.Binary, "deploy", root, "--out", outDir, "--format", "json")
	cmd.Stdout = diagnostics
	cmd.Stderr = diagnostics
	if err := cmd.Run(); err != nil {
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("meta-ast planning timed out after %s", r.config.Timeout)
		}
		message := strings.TrimSpace(diagnostics.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("meta-ast planning failed: %s", message)
	}

	podsData, err := readLimitedFile(filepath.Join(outDir, "metacall.pods.json"), r.config.MaxOutputBytes)
	if err != nil {
		return nil, fmt.Errorf("read metacall.pods.json: %w", err)
	}
	meshData, err := readLimitedFile(filepath.Join(outDir, "metacall.mesh.json"), r.config.MaxOutputBytes)
	if err != nil {
		return nil, fmt.Errorf("read metacall.mesh.json: %w", err)
	}

	var manifest PodManifest
	if err := json.Unmarshal(podsData, &manifest); err != nil {
		return nil, fmt.Errorf("parse metacall.pods.json: %w", err)
	}
	if !json.Valid(meshData) {
		return nil, fmt.Errorf("parse metacall.mesh.json: invalid JSON")
	}
	if err := normalizeAndValidate(root, &manifest); err != nil {
		return nil, err
	}

	return &Result{Pods: manifest, Mesh: json.RawMessage(meshData)}, nil
}

func normalizeAndValidate(root string, manifest *PodManifest) error {
	if manifest.Version != SupportedSchemaVersion {
		return fmt.Errorf("unsupported meta-ast manifest version %q", manifest.Version)
	}

	ids := make(map[int]struct{}, len(manifest.Deployments))
	for i := range manifest.Deployments {
		deployment := &manifest.Deployments[i]
		if deployment.ID < 0 {
			return fmt.Errorf("meta-ast deployment has invalid id %d", deployment.ID)
		}
		if _, duplicate := ids[deployment.ID]; duplicate {
			return fmt.Errorf("meta-ast deployment id %d is duplicated", deployment.ID)
		}
		ids[deployment.ID] = struct{}{}
		if deployment.Language == "" {
			return fmt.Errorf("meta-ast deployment %d has no language", deployment.ID)
		}
		if len(deployment.Files) == 0 {
			return fmt.Errorf("meta-ast deployment %d has no files", deployment.ID)
		}

		seenFiles := map[string]struct{}{}
		for fileIndex, rawPath := range deployment.Files {
			normalized, err := normalizeSourcePath(root, rawPath)
			if err != nil {
				return fmt.Errorf("meta-ast deployment %d file %q: %w", deployment.ID, rawPath, err)
			}
			if _, duplicate := seenFiles[normalized]; duplicate {
				return fmt.Errorf("meta-ast deployment %d contains duplicate file %q", deployment.ID, normalized)
			}
			seenFiles[normalized] = struct{}{}
			deployment.Files[fileIndex] = normalized
		}
		sort.Strings(deployment.Files)
		sort.Slice(deployment.Dependencies, func(a, b int) bool {
			left, right := deployment.Dependencies[a], deployment.Dependencies[b]
			if left.Name != right.Name {
				return left.Name < right.Name
			}
			return left.Language < right.Language
		})
	}

	for _, edge := range manifest.Edges {
		if _, ok := ids[edge.FromPod]; !ok {
			return fmt.Errorf("meta-ast edge references unknown from_pod %d", edge.FromPod)
		}
		if _, ok := ids[edge.ToPod]; !ok {
			return fmt.Errorf("meta-ast edge references unknown to_pod %d", edge.ToPod)
		}
		if edge.Confidence < 0 || edge.Confidence > 1 {
			return fmt.Errorf("meta-ast edge %d -> %d has invalid confidence %f", edge.FromPod, edge.ToPod, edge.Confidence)
		}
	}

	sort.Slice(manifest.Deployments, func(i, j int) bool {
		return manifest.Deployments[i].ID < manifest.Deployments[j].ID
	})
	sort.Slice(manifest.Edges, func(i, j int) bool {
		left, right := manifest.Edges[i], manifest.Edges[j]
		if left.FromPod != right.FromPod {
			return left.FromPod < right.FromPod
		}
		if left.ToPod != right.ToPod {
			return left.ToPod < right.ToPod
		}
		return left.Kind < right.Kind
	})
	return nil
}

func normalizeSourcePath(root, rawPath string) (string, error) {
	if strings.TrimSpace(rawPath) == "" {
		return "", fmt.Errorf("path is empty")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	candidate := filepath.FromSlash(rawPath)
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(rootAbs, candidate)
	}
	candidate, err = filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(rootAbs, candidate)
	if err != nil {
		return "", err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("path escapes source root")
	}
	if err := rejectSymlinkPath(rootAbs, relative); err != nil {
		return "", err
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", fmt.Errorf("source file is unavailable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("source path is not a regular file")
	}
	return filepath.ToSlash(relative), nil
}

func rejectSymlinkPath(root, relative string) error {
	current := root
	for _, component := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("source file is unavailable: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("source path contains a symbolic link")
		}
	}
	return nil
}

func validateInput(root string, maxFiles int, maxBytes int64) error {
	files := 0
	var totalBytes int64
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "__pycache__":
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		files++
		totalBytes += info.Size()
		if files > maxFiles {
			return fmt.Errorf("planner input contains more than %d files", maxFiles)
		}
		if totalBytes > maxBytes {
			return fmt.Errorf("planner input exceeds %d bytes", maxBytes)
		}
		return nil
	})
}

func readLimitedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("planner output exceeds %d bytes", limit)
	}
	return data, nil
}

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = b.buffer.Write(data)
	}
	return originalLength, nil
}

func (b *limitedBuffer) String() string {
	return b.buffer.String()
}
