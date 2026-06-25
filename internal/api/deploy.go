package api

import (
	"context"
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
)

const (
	labelDeployGroup = "metacall.io/deploy-group"
	labelComponent   = "metacall.io/component"
	labelLanguage    = "metacall.io/language"
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
	configs, err := findMetaCallConfigs(sourcePath)
	if err != nil {
		return nil, err
	}
	if len(configs) == 0 {
		return nil, fmt.Errorf("no metacall*.json files found")
	}

	deployed := make([]DeployedFunction, 0, len(configs))
	seen := map[string]int{}
	for _, configPath := range configs {
		config, err := readMetaCallConfig(configPath)
		if err != nil {
			return nil, err
		}
		if config.LanguageID == "" {
			return nil, fmt.Errorf("%s missing language_id", configPath)
		}
		if _, ok := supportedLanguages[config.LanguageID]; !ok {
			return nil, fmt.Errorf("language %q is not supported by builder-cli/function-mesh", config.LanguageID)
		}

		baseName := sanitizeName(deploymentID + "-" + config.LanguageID)
		seen[baseName]++
		name := baseName
		if seen[baseName] > 1 {
			name = fmt.Sprintf("%s-%d", baseName, seen[baseName])
		}
		configMapName := name + "-code"
		entrypoint := filepath.Base(configPath)

		files, err := sourceFiles(configPath, config)
		if err != nil {
			return nil, err
		}
		if err := s.upsertSourceConfigMap(ctx, configMapName, deploymentID, config.LanguageID, files); err != nil {
			return nil, err
		}
		if err := s.upsertFunction(ctx, name, deploymentID, config.LanguageID, configMapName, entrypoint); err != nil {
			return nil, err
		}

		deployed = append(deployed, DeployedFunction{
			Name:       name,
			Language:   config.LanguageID,
			ConfigMap:  configMapName,
			Entrypoint: entrypoint,
		})
	}
	return deployed, nil
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

func sourceFiles(configPath string, config metacallConfig) (map[string]string, error) {
	configDir := filepath.Dir(configPath)
	files := map[string]string{}
	scripts := make([]string, 0, len(config.Scripts))
	sourceRoot := filepath.Clean(filepath.Join(configDir, config.Path))

	if err := filepath.WalkDir(sourceRoot, func(path string, entry os.DirEntry, err error) error {
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
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// ConfigMap volume keys cannot preserve arbitrary nested paths. The
		// current supported examples keep runtime files beside metacall.json.
		files[filepath.Base(path)] = string(data)
		return nil
	}); err != nil {
		return nil, err
	}

	// Search upward from the source directory toward the repo root for
	// dependency manifests (package.json, requirements.txt, Gemfile) that
	// the runtime's installDependencies() needs to run npm/pip/bundle install.
	repoRoot := findRepoRoot(configDir)
	for dir := sourceRoot; ; dir = filepath.Dir(dir) {
		for _, manifest := range depManifests {
			if _, exists := files[manifest]; exists {
				continue
			}
			candidate := filepath.Join(dir, manifest)
			if data, err := os.ReadFile(candidate); err == nil {
				files[manifest] = string(data)
			}
		}
		if dir == repoRoot || dir == filepath.Dir(dir) {
			break
		}
	}

	for _, script := range config.Scripts {
		scriptPath := filepath.Clean(filepath.Join(configDir, config.Path, script))
		key := filepath.Base(script)
		if _, ok := files[key]; !ok {
			data, err := os.ReadFile(scriptPath)
			if err != nil {
				return nil, err
			}
			files[key] = string(data)
		}
		scripts = append(scripts, key)
	}

	config.Raw["path"] = "."
	config.Raw["scripts"] = scripts
	rewritten, err := json.MarshalIndent(config.Raw, "", "  ")
	if err != nil {
		return nil, err
	}
	files[filepath.Base(configPath)] = string(rewritten) + "\n"
	return files, nil
}

func (s *Server) upsertSourceConfigMap(ctx context.Context, name, deploymentID, language string, files map[string]string) error {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.Client.Get(ctx, key, &cm); err != nil && !apierrors.IsNotFound(err) {
		return err
	} else if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}}
	}

	_, err := controllerutil.CreateOrUpdate(ctx, s.Client, &cm, func() error {
		cm.Labels = deployLabels(deploymentID, language, "source")
		cm.Data = files
		return nil
	})
	return err
}

func (s *Server) upsertFunction(ctx context.Context, name, deploymentID, language, configMap, entrypoint string) error {
	var fn meshv1.Function
	key := types.NamespacedName{Name: name, Namespace: s.Namespace}
	if err := s.Client.Get(ctx, key, &fn); err != nil && !apierrors.IsNotFound(err) {
		return err
	} else if apierrors.IsNotFound(err) {
		fn = meshv1.Function{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}}
	}

	_, err := controllerutil.CreateOrUpdate(ctx, s.Client, &fn, func() error {
		fn.Labels = deployLabels(deploymentID, language, "function")
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
