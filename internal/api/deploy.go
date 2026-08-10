package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	meshv1 "github.com/metacall/function-mesh/api/v1"
	"github.com/metacall/function-mesh/internal/sourcebundle"
)

const (
	labelDeployGroup     = "metacall.io/deploy-group"
	labelComponent       = "metacall.io/component"
	labelLanguage        = "metacall.io/language"
	annotationSourceHash = "metacall.io/source-hash"
)

var supportedLanguages = map[string]struct{}{
	"py":   {},
	"rb":   {},
	"node": {},
	"wasm": {},
	"java": {},
}

// depManifests are dependency files needed by the runtime's installDependencies().
// If not found beside metacall.json, we search upward toward the repo root.
var depManifests = []string{"package.json", "requirements.txt", "Gemfile"}

type DeployedFunction struct {
	Name       string `json:"name"`
	Language   string `json:"language"`
	ConfigMap  string `json:"configMap"`
	Entrypoint string `json:"entrypoint"`
}

type metacallConfig struct {
	Path       string
	LanguageID string
	Scripts    []string
	Raw        map[string]any
}

func (s *Server) deployFromSource(ctx context.Context, sourcePath, deploymentID string) ([]DeployedFunction, error) {
	result, err := s.deployFromSourceWithPlan(ctx, sourcePath, deploymentID, PlanOff)
	if err != nil {
		return nil, err
	}
	return result.Functions, nil
}

func findMetaCallConfigs(root string) ([]string, error) {
	var configs []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		name := strings.ToLower(entry.Name())
		if strings.HasPrefix(name, "metacall") && strings.HasSuffix(name, ".json") {
			configs = append(configs, path)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(configs)
	return configs, nil
}

func readMetaCallConfig(path string) (metacallConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return metacallConfig{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return metacallConfig{}, err
	}

	cfg := metacallConfig{Path: ".", Raw: raw}
	if value, ok := raw["path"].(string); ok && value != "" {
		cfg.Path = value
	}
	if value, ok := raw["language_id"].(string); ok {
		cfg.LanguageID = value
	}
	if values, ok := raw["scripts"].([]any); ok {
		for _, value := range values {
			if script, ok := value.(string); ok && script != "" {
				cfg.Scripts = append(cfg.Scripts, script)
			}
		}
	}
	return cfg, nil
}

func sourceFiles(packageRoot, configPath string, config metacallConfig) (map[string]string, string, []string, map[string]any, error) {
	packageRoot, err := filepath.Abs(packageRoot)
	if err != nil {
		return nil, "", nil, nil, err
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return nil, "", nil, nil, err
	}
	configDir := filepath.Dir(configPath)
	sourceRoot := filepath.Clean(filepath.Join(configDir, config.Path))
	if _, err := relativeSourcePath(packageRoot, sourceRoot); err != nil {
		return nil, "", nil, nil, fmt.Errorf("config source path %q: %w", config.Path, err)
	}
	if err := ensureContainedPath(packageRoot, sourceRoot, false); err != nil {
		return nil, "", nil, nil, fmt.Errorf("config source path %q: %w", config.Path, err)
	}

	files := map[string]string{}
	if err := filepath.WalkDir(sourceRoot, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		relative, err := relativeSourcePath(packageRoot, filePath)
		if err != nil {
			return err
		}
		files[relative] = string(data)
		return nil
	}); err != nil {
		return nil, "", nil, nil, err
	}

	// Dependency manifests may live above the configured source directory, but
	// must never be read from outside the uploaded package root.
	for dir := sourceRoot; ; dir = filepath.Dir(dir) {
		for _, manifest := range depManifests {
			if _, exists := files[manifest]; exists {
				continue
			}
			candidate := filepath.Join(dir, manifest)
			if err := ensureContainedPath(packageRoot, candidate, true); err != nil {
				continue
			}
			data, err := os.ReadFile(candidate)
			if err != nil {
				return nil, "", nil, nil, err
			}
			// The runtime installs dependencies from /app before loading the
			// nested entrypoint, so keep the nearest manifest at bundle root.
			files[manifest] = string(data)
		}
		if filepath.Clean(dir) == filepath.Clean(packageRoot) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}

	sourceRootRelative, err := relativeSourcePath(packageRoot, sourceRoot)
	if err != nil {
		return nil, "", nil, nil, err
	}
	if sourceRootRelative == "" {
		sourceRootRelative = "."
	}
	scripts := make([]string, 0, len(config.Scripts))
	scriptPaths := make([]string, 0, len(config.Scripts))
	for _, script := range config.Scripts {
		normalizedScript, err := sourcebundle.NormalizePath(script)
		if err != nil {
			return nil, "", nil, nil, fmt.Errorf("invalid configured script %q: %w", script, err)
		}
		scriptPath := filepath.Join(sourceRoot, filepath.FromSlash(normalizedScript))
		if err := ensureContainedPath(packageRoot, scriptPath, true); err != nil {
			return nil, "", nil, nil, fmt.Errorf("configured script %q: %w", script, err)
		}
		packageRelative, err := relativeSourcePath(packageRoot, scriptPath)
		if err != nil {
			return nil, "", nil, nil, fmt.Errorf("configured script %q: %w", script, err)
		}
		if _, exists := files[packageRelative]; !exists {
			data, readErr := os.ReadFile(scriptPath)
			if readErr != nil {
				return nil, "", nil, nil, readErr
			}
			files[packageRelative] = string(data)
		}
		scripts = append(scripts, normalizedScript)
		scriptPaths = append(scriptPaths, packageRelative)
	}
	sort.Strings(scriptPaths)

	rawData, err := json.Marshal(config.Raw)
	if err != nil {
		return nil, "", nil, nil, err
	}
	var rewrittenRaw map[string]any
	if err := json.Unmarshal(rawData, &rewrittenRaw); err != nil {
		return nil, "", nil, nil, err
	}
	rewrittenRaw["path"] = sourceRootRelative
	rewrittenRaw["scripts"] = scripts
	rewritten, err := json.MarshalIndent(rewrittenRaw, "", "  ")
	if err != nil {
		return nil, "", nil, nil, err
	}
	entrypoint, err := relativeSourcePath(packageRoot, configPath)
	if err != nil {
		return nil, "", nil, nil, err
	}
	files[entrypoint] = string(rewritten) + "\n"
	return files, entrypoint, scriptPaths, rewrittenRaw, nil
}

func relativeSourcePath(root, target string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return "", err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("path escapes package root")
	}
	if relative == "." {
		return "", nil
	}
	return sourcebundle.NormalizePath(filepath.ToSlash(relative))
}
func ensureContainedPath(root, target string, requireRegular bool) error {
	relative, err := relativeSourcePath(root, target)
	if err != nil {
		return err
	}
	current, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	var info os.FileInfo
	if relative == "" {
		info, err = os.Lstat(current)
	} else {
		for _, component := range strings.Split(filepath.FromSlash(relative), string(filepath.Separator)) {
			current = filepath.Join(current, component)
			info, err = os.Lstat(current)
			if err != nil {
				break
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("path contains a symbolic link")
			}
		}
	}
	if err != nil {
		return err
	}
	if requireRegular && !info.Mode().IsRegular() {
		return fmt.Errorf("path is not a regular file")
	}
	return nil
}
func (s *Server) upsertSourceConfigMap(ctx context.Context, name, deploymentID, language string, files map[string]string) (string, error) {
	data, err := sourcebundle.Encode(files)
	if err != nil {
		return "", fmt.Errorf("encode source ConfigMap %s: %w", name, err)
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.Client.Get(ctx, key, &cm); err != nil && !apierrors.IsNotFound(err) {
		return "", err
	} else if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}}
	}

	_, err = controllerutil.CreateOrUpdate(ctx, s.Client, &cm, func() error {
		cm.Labels = deployLabels(deploymentID, language, "source")
		cm.Data = data
		return nil
	})
	return hashSourceData(data), err
}

func (s *Server) upsertFunction(ctx context.Context, name, deploymentID, language, configMap, entrypoint, sourceHash string) error {
	var fn meshv1.Function
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.Client.Get(ctx, key, &fn); err != nil && !apierrors.IsNotFound(err) {
		return err
	} else if apierrors.IsNotFound(err) {
		fn = meshv1.Function{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}}
	}

	_, err := controllerutil.CreateOrUpdate(ctx, s.Client, &fn, func() error {
		fn.Labels = deployLabels(deploymentID, language, "function")
		if fn.Annotations == nil {
			fn.Annotations = map[string]string{}
		}
		fn.Annotations[annotationSourceHash] = sourceHash
		fn.Spec = meshv1.FunctionSpec{
			Language:    language,
			DeployGroup: deploymentID,
			Source: meshv1.SourceSpec{
				Type:       "configmap",
				ConfigMap:  configMap,
				Entrypoint: entrypoint,
			},
		}
		return nil
	})
	return err
}

func hashSourceData(data map[string]string) string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		_, _ = hash.Write([]byte(key))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(data[key]))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (s *Server) deleteDeployment(ctx context.Context, deploymentID string) error {
	selector := client.MatchingLabels{labelDeployGroup: deploymentID}

	var functions meshv1.FunctionList
	if err := s.Client.List(ctx, &functions, client.InNamespace(s.Namespace), selector); err != nil {
		return err
	}
	for i := range functions.Items {
		if err := s.Client.Delete(ctx, &functions.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	var configMaps corev1.ConfigMapList
	if err := s.Client.List(ctx, &configMaps, client.InNamespace(s.Namespace), selector); err != nil {
		return err
	}
	for i := range configMaps.Items {
		if err := s.Client.Delete(ctx, &configMaps.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (s *Server) deploymentExists(ctx context.Context, deploymentID string) (bool, error) {
	var functions meshv1.FunctionList
	if err := s.Client.List(ctx, &functions, client.InNamespace(s.Namespace), client.MatchingLabels{labelDeployGroup: deploymentID}); err != nil {
		return false, err
	}
	return len(functions.Items) > 0, nil
}

func deployLabels(deploymentID, language, component string) map[string]string {
	return map[string]string{
		labelDeployGroup: deploymentID,
		labelLanguage:    language,
		labelComponent:   component,
	}
}

var invalidNameChars = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeName(value string) string {
	name := strings.ToLower(value)
	name = invalidNameChars.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name) > 52 {
		name = strings.TrimRight(name[:52], "-")
	}
	if name == "" {
		return "function"
	}
	return name
}

// findRepoRoot walks upward from dir looking for a .git directory.
// Returns the directory containing .git, or dir itself if none is found.
func findRepoRoot(dir string) string {
	dir = filepath.Clean(dir)
	for {
		if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}
