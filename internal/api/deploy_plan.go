package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	meshv1 "github.com/metacall/function-mesh/api/v1"
	meshplanner "github.com/metacall/function-mesh/internal/planner"
	"github.com/metacall/function-mesh/internal/sourcebundle"
)

const (
	PlanOff      = "off"
	PlanAuto     = "auto"
	PlanRequired = "required"

	defaultMaxConfigMapBytes = 900 * 1024
)

type PlanningSummary struct {
	Requested      string              `json:"requested"`
	Status         string              `json:"status"`
	SchemaVersion  string              `json:"schemaVersion,omitempty"`
	OriginalUnits  int                 `json:"originalUnits"`
	PlannedUnits   int                 `json:"plannedUnits"`
	Pods           []PlannedPodSummary `json:"pods,omitempty"`
	Edges          []meshplanner.Edge  `json:"edges,omitempty"`
	Warnings       []string            `json:"warnings,omitempty"`
	FallbackReason string              `json:"fallbackReason,omitempty"`
}

type PlannedPodSummary struct {
	ID       int      `json:"id"`
	Language string   `json:"language"`
	Files    []string `json:"files"`
}

type DeployResult struct {
	Functions []DeployedFunction `json:"functions"`
	Planning  PlanningSummary    `json:"plan"`
}

type deploymentUnit struct {
	Name         string
	Language     string
	ConfigMap    string
	Entrypoint   string
	Files        map[string]string
	ScriptPaths  []string
	ConfigRaw    map[string]any
	PlannerPodID *int
}

func (s *Server) deployFromSourceWithPlan(ctx context.Context, sourcePath, deploymentID, requestedMode string) (DeployResult, error) {
	mode := strings.ToLower(strings.TrimSpace(requestedMode))
	if mode == "" {
		mode = PlanOff
	}
	if mode != PlanOff && mode != PlanAuto && mode != PlanRequired {
		return DeployResult{}, fmt.Errorf("invalid planning mode %q: expected off, auto, or required", requestedMode)
	}

	unlock := s.lockDeployment(deploymentID)
	defer unlock()

	legacyUnits, err := buildLegacyUnits(sourcePath, deploymentID)
	if err != nil {
		return DeployResult{}, err
	}
	summary := PlanningSummary{
		Requested:     mode,
		Status:        "disabled",
		OriginalUnits: len(legacyUnits),
		PlannedUnits:  len(legacyUnits),
	}
	desiredUnits := legacyUnits

	if mode != PlanOff {
		if !s.PlannerEnabled || s.Planner == nil {
			if mode == PlanRequired {
				s.recordPlannerRun("error")
				return DeployResult{}, fmt.Errorf("planning is required but the Meta-AST planner is disabled")
			}
			summary.Status = "fallback"
			summary.FallbackReason = "Meta-AST planner is disabled"
			s.recordPlannerRun("fallback")
		} else {
			started := time.Now()
			planResult, planErr := s.Planner.Plan(ctx, sourcePath)
			if s.plannerDuration != nil {
				s.plannerDuration.Observe(time.Since(started).Seconds())
			}
			if planErr == nil {
				var warnings []string
				desiredUnits, warnings, planErr = applyPlannerOverlay(deploymentID, legacyUnits, planResult)
				summary.Warnings = warnings
				summary.SchemaVersion = planResult.Pods.Version
				summary.Pods = summarizePods(planResult.Pods.Deployments)
				summary.Edges = append([]meshplanner.Edge(nil), planResult.Pods.Edges...)
			}
			if planErr == nil {
				planErr = s.validateDeploymentUnits(desiredUnits)
			}
			if planErr != nil {
				if mode == PlanRequired {
					s.recordPlannerRun("error")
					return DeployResult{}, fmt.Errorf("required Meta-AST planning failed: %w", planErr)
				}
				desiredUnits = legacyUnits
				summary.Status = "fallback"
				summary.PlannedUnits = len(legacyUnits)
				summary.FallbackReason = planErr.Error()
				s.recordPlannerRun("fallback")
			} else {
				summary.Status = "applied"
				summary.PlannedUnits = len(desiredUnits)
				s.recordPlannerRun("applied")
			}
		}
	}

	if err := s.validateDeploymentUnits(desiredUnits); err != nil {
		return DeployResult{}, err
	}

	functions, err := s.applyDeploymentUnits(ctx, deploymentID, desiredUnits)
	if err != nil {
		return DeployResult{}, err
	}
	return DeployResult{Functions: functions, Planning: summary}, nil
}

func buildLegacyUnits(sourceRoot, deploymentID string) ([]deploymentUnit, error) {
	configs, err := findMetaCallConfigs(sourceRoot)
	if err != nil {
		return nil, err
	}
	if len(configs) == 0 {
		return nil, fmt.Errorf("no metacall*.json files found")
	}

	units := make([]deploymentUnit, 0, len(configs))
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
		files, entrypoint, scriptPaths, rewrittenRaw, err := sourceFiles(sourceRoot, configPath, config)
		if err != nil {
			return nil, err
		}
		units = append(units, deploymentUnit{
			Name:        name,
			Language:    config.LanguageID,
			ConfigMap:   name + "-code",
			Entrypoint:  entrypoint,
			Files:       files,
			ScriptPaths: scriptPaths,
			ConfigRaw:   rewrittenRaw,
		})
	}
	return units, nil
}

func applyPlannerOverlay(deploymentID string, legacy []deploymentUnit, result *meshplanner.Result) ([]deploymentUnit, []string, error) {
	if result == nil {
		return nil, nil, fmt.Errorf("planner returned no result")
	}
	eligible := map[string]bool{"py": true, "node": true}
	podsByID := make(map[int]meshplanner.PodDeployment, len(result.Pods.Deployments))
	fileToPod := map[string]int{}
	for _, pod := range result.Pods.Deployments {
		podsByID[pod.ID] = pod
		if !eligible[pod.Language] {
			continue
		}
		for _, file := range pod.Files {
			if existing, duplicate := fileToPod[file]; duplicate && existing != pod.ID {
				return nil, nil, fmt.Errorf("planner assigns file %q to multiple pods", file)
			}
			fileToPod[file] = pod.ID
		}
	}

	groups := map[int][]deploymentUnit{}
	var desired []deploymentUnit
	var warnings []string
	for _, unit := range legacy {
		if !eligible[unit.Language] {
			desired = append(desired, unit)
			continue
		}
		podIDs := map[int]struct{}{}
		for _, script := range unit.ScriptPaths {
			if podID, found := fileToPod[script]; found {
				podIDs[podID] = struct{}{}
			}
		}
		switch len(podIDs) {
		case 0:
			desired = append(desired, unit)
			warnings = append(warnings, fmt.Sprintf("%s was not represented by Meta-AST and kept as a legacy unit", unit.Entrypoint))
		case 1:
			for podID := range podIDs {
				pod := podsByID[podID]
				if pod.Language != unit.Language {
					return nil, nil, fmt.Errorf("planner language %q does not match config language %q for %s", pod.Language, unit.Language, unit.Entrypoint)
				}
				groups[podID] = append(groups[podID], unit)
			}
		default:
			return nil, nil, fmt.Errorf("config %s spans multiple planner pods and cannot be split safely", unit.Entrypoint)
		}
	}

	podIDs := make([]int, 0, len(groups))
	for podID := range groups {
		podIDs = append(podIDs, podID)
	}
	sort.Ints(podIDs)
	for _, podID := range podIDs {
		pod := podsByID[podID]
		merged, err := mergePlannerGroup(deploymentID, pod, groups[podID])
		if err != nil {
			return nil, nil, err
		}
		desired = append(desired, merged)
	}
	sort.Slice(desired, func(i, j int) bool { return desired[i].Name < desired[j].Name })
	return desired, warnings, nil
}

func mergePlannerGroup(deploymentID string, pod meshplanner.PodDeployment, units []deploymentUnit) (deploymentUnit, error) {
	if len(units) == 0 {
		return deploymentUnit{}, fmt.Errorf("planner pod %d has no configured deployment", pod.ID)
	}
	baseExtras, err := configExtras(units[0].ConfigRaw)
	if err != nil {
		return deploymentUnit{}, err
	}
	files := map[string]string{}
	scripts := map[string]struct{}{}
	for _, unit := range units {
		extras, err := configExtras(unit.ConfigRaw)
		if err != nil {
			return deploymentUnit{}, err
		}
		if string(extras) != string(baseExtras) {
			return deploymentUnit{}, fmt.Errorf("planner pod %d contains incompatible MetaCall configurations", pod.ID)
		}
		for filePath, content := range unit.Files {
			if previous, exists := files[filePath]; exists && previous != content {
				return deploymentUnit{}, fmt.Errorf("planner pod %d contains conflicting file %q", pod.ID, filePath)
			}
			files[filePath] = content
		}
		for _, script := range unit.ScriptPaths {
			scripts[script] = struct{}{}
		}
	}

	scriptList := make([]string, 0, len(scripts))
	for script := range scripts {
		scriptList = append(scriptList, script)
	}
	sort.Strings(scriptList)
	var generated map[string]any
	if err := json.Unmarshal(baseExtras, &generated); err != nil {
		return deploymentUnit{}, err
	}
	generated["language_id"] = pod.Language
	generated["path"] = "."
	generated["scripts"] = scriptList
	configData, err := json.MarshalIndent(generated, "", "  ")
	if err != nil {
		return deploymentUnit{}, err
	}
	entrypoint := fmt.Sprintf("metacall-planned-%s-%d.json", pod.Language, pod.ID)
	files[entrypoint] = string(configData) + "\n"
	podID := pod.ID
	name := plannedUnitName(deploymentID, pod.Language, pod.ID)
	return deploymentUnit{
		Name:         name,
		Language:     pod.Language,
		ConfigMap:    name + "-code",
		Entrypoint:   entrypoint,
		Files:        files,
		ScriptPaths:  scriptList,
		ConfigRaw:    generated,
		PlannerPodID: &podID,
	}, nil
}

func plannedUnitName(deploymentID, language string, podID int) string {
	suffix := "-" + sanitizeName(language) + "-p" + strconv.Itoa(podID)
	prefix := sanitizeName(deploymentID)
	maxPrefix := 52 - len(suffix)
	if maxPrefix < 1 {
		return sanitizeName("function" + suffix)
	}
	if len(prefix) > maxPrefix {
		prefix = strings.TrimRight(prefix[:maxPrefix], "-")
	}
	if prefix == "" {
		prefix = "function"
	}
	return prefix + suffix
}

func configExtras(raw map[string]any) ([]byte, error) {
	copy := make(map[string]any, len(raw))
	for key, value := range raw {
		if key != "language_id" && key != "path" && key != "scripts" {
			copy[key] = value
		}
	}
	return json.Marshal(copy)
}

func (s *Server) validateDeploymentUnits(units []deploymentUnit) error {
	limit := s.MaxConfigMapBytes
	if limit <= 0 {
		limit = defaultMaxConfigMapBytes
	}
	seenNames := map[string]struct{}{}
	for _, unit := range units {
		if _, duplicate := seenNames[unit.Name]; duplicate {
			return fmt.Errorf("deployment unit name %q is duplicated", unit.Name)
		}
		seenNames[unit.Name] = struct{}{}
		if _, supported := supportedLanguages[unit.Language]; !supported {
			return fmt.Errorf("language %q is not supported by builder-cli/function-mesh", unit.Language)
		}
		if _, exists := unit.Files[unit.Entrypoint]; !exists {
			return fmt.Errorf("deployment unit %s is missing entrypoint %q", unit.Name, unit.Entrypoint)
		}
		data, err := sourcebundle.Encode(unit.Files)
		if err != nil {
			return fmt.Errorf("encode deployment unit %s: %w", unit.Name, err)
		}
		if size := sourcebundle.EstimateSize(data); size > limit {
			return fmt.Errorf("deployment unit %s ConfigMap payload is %d bytes, exceeding the %d byte limit", unit.Name, size, limit)
		}
	}
	return nil
}

func (s *Server) applyDeploymentUnits(ctx context.Context, deploymentID string, units []deploymentUnit) ([]DeployedFunction, error) {
	desiredFunctions := make(map[string]struct{}, len(units))
	desiredConfigMaps := make(map[string]struct{}, len(units))
	deployed := make([]DeployedFunction, 0, len(units))
	sourceHashes := make(map[string]string, len(units))

	for _, unit := range units {
		sourceHash, err := s.upsertSourceConfigMap(ctx, unit.ConfigMap, deploymentID, unit.Language, unit.Files)
		if err != nil {
			return nil, err
		}
		sourceHashes[unit.ConfigMap] = sourceHash
		desiredConfigMaps[unit.ConfigMap] = struct{}{}
	}
	for _, unit := range units {
		if err := s.upsertFunction(ctx, unit.Name, deploymentID, unit.Language, unit.ConfigMap, unit.Entrypoint, sourceHashes[unit.ConfigMap]); err != nil {
			return nil, err
		}
		desiredFunctions[unit.Name] = struct{}{}
		deployed = append(deployed, DeployedFunction{
			Name: unit.Name, Language: unit.Language, ConfigMap: unit.ConfigMap, Entrypoint: unit.Entrypoint,
		})
	}

	selector := client.MatchingLabels{labelDeployGroup: deploymentID}
	var existingFunctions meshv1.FunctionList
	if err := s.Client.List(ctx, &existingFunctions, client.InNamespace(s.Namespace), selector); err != nil {
		return nil, err
	}
	for i := range existingFunctions.Items {
		if _, keep := desiredFunctions[existingFunctions.Items[i].Name]; !keep {
			if err := s.Client.Delete(ctx, &existingFunctions.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return nil, err
			}
		}
	}

	var existingConfigMaps corev1.ConfigMapList
	if err := s.Client.List(ctx, &existingConfigMaps, client.InNamespace(s.Namespace), client.MatchingLabels{labelDeployGroup: deploymentID, labelComponent: "source"}); err != nil {
		return nil, err
	}
	for i := range existingConfigMaps.Items {
		if _, keep := desiredConfigMaps[existingConfigMaps.Items[i].Name]; !keep {
			if err := s.Client.Delete(ctx, &existingConfigMaps.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return nil, err
			}
		}
	}
	sort.Slice(deployed, func(i, j int) bool { return deployed[i].Name < deployed[j].Name })
	return deployed, nil
}

func (s *Server) lockDeployment(deploymentID string) func() {
	value, _ := s.deployLocks.LoadOrStore(deploymentID, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func (s *Server) recordPlannerRun(status string) {
	if s.plannerRuns != nil {
		s.plannerRuns.WithLabelValues(status).Inc()
	}
}

func summarizePods(pods []meshplanner.PodDeployment) []PlannedPodSummary {
	summaries := make([]PlannedPodSummary, 0, len(pods))
	for _, pod := range pods {
		files := append([]string(nil), pod.Files...)
		summaries = append(summaries, PlannedPodSummary{ID: pod.ID, Language: pod.Language, Files: files})
	}
	return summaries
}
