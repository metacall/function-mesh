package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	meshv1 "github.com/metacall/function-mesh/api/v1"
	meshplanner "github.com/metacall/function-mesh/internal/planner"
	"github.com/metacall/function-mesh/internal/sourcebundle"
)

type fakePlanner struct {
	result *meshplanner.Result
	err    error
}

func (p fakePlanner) Plan(context.Context, string) (*meshplanner.Result, error) {
	return p.result, p.err
}

func TestDeployWithPlanMergesConfiguredServicesByPod(t *testing.T) {
	ctx := context.Background()
	source := plannedSourceFixture(t)
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	server := NewServer(kube, nil, "metacall-functions", "http://router:9090", t.TempDir())
	server.PlannerEnabled = true
	server.Planner = fakePlanner{result: &meshplanner.Result{Pods: meshplanner.PodManifest{
		Version: meshplanner.SupportedSchemaVersion,
		Deployments: []meshplanner.PodDeployment{{
			ID:       3,
			Language: "py",
			Files:    []string{"service-a/a.py", "service-b/b.py"},
		}},
	}}}

	result, err := server.deployFromSourceWithPlan(ctx, source, "demo", PlanAuto)
	if err != nil {
		t.Fatalf("planned deploy failed: %v", err)
	}
	if result.Planning.Status != "applied" || result.Planning.OriginalUnits != 2 || result.Planning.PlannedUnits != 1 {
		t.Fatalf("unexpected planning summary: %#v", result.Planning)
	}
	if len(result.Functions) != 1 || result.Functions[0].Name != "demo-py-p3" {
		t.Fatalf("unexpected planned functions: %#v", result.Functions)
	}

	var cm corev1.ConfigMap
	key := types.NamespacedName{Name: "demo-py-p3-code", Namespace: "metacall-functions"}
	if err := kube.Get(ctx, key, &cm); err != nil {
		t.Fatalf("get merged ConfigMap: %v", err)
	}
	items, indexed, err := sourcebundle.VolumeItems(cm.Data)
	if err != nil || !indexed {
		t.Fatalf("decode merged ConfigMap: indexed=%v err=%v", indexed, err)
	}
	paths := make(map[string]string, len(items))
	for _, item := range items {
		paths[item.Path] = cm.Data[item.Key]
	}
	for _, expected := range []string{"service-a/a.py", "service-b/b.py", "metacall-planned-py-3.json"} {
		if _, exists := paths[expected]; !exists {
			t.Fatalf("merged ConfigMap missing %q: %#v", expected, paths)
		}
	}
	var config struct {
		LanguageID string   `json:"language_id"`
		Path       string   `json:"path"`
		Scripts    []string `json:"scripts"`
	}
	if err := json.Unmarshal([]byte(paths["metacall-planned-py-3.json"]), &config); err != nil {
		t.Fatalf("parse generated MetaCall config: %v", err)
	}
	if config.LanguageID != "py" || config.Path != "." || strings.Join(config.Scripts, ",") != "service-a/a.py,service-b/b.py" {
		t.Fatalf("unexpected generated MetaCall config: %#v", config)
	}
}

func TestDeployWithPlanAutoFallsBackAndRequiredFails(t *testing.T) {
	source := plannedSourceFixture(t)

	t.Run("auto", func(t *testing.T) {
		kube := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
		server := NewServer(kube, nil, "metacall-functions", "http://router:9090", t.TempDir())
		server.PlannerEnabled = true
		server.Planner = fakePlanner{err: errors.New("analysis failed")}

		result, err := server.deployFromSourceWithPlan(context.Background(), source, "demo", PlanAuto)
		if err != nil {
			t.Fatalf("auto deploy should fall back: %v", err)
		}
		if result.Planning.Status != "fallback" || !strings.Contains(result.Planning.FallbackReason, "analysis failed") {
			t.Fatalf("unexpected fallback summary: %#v", result.Planning)
		}
		if len(result.Functions) != 2 {
			t.Fatalf("expected two legacy functions, got %#v", result.Functions)
		}
	})

	t.Run("required", func(t *testing.T) {
		kube := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
		server := NewServer(kube, nil, "metacall-functions", "http://router:9090", t.TempDir())
		server.PlannerEnabled = true
		server.Planner = fakePlanner{err: errors.New("analysis failed")}

		if _, err := server.deployFromSourceWithPlan(context.Background(), source, "demo", PlanRequired); err == nil || !strings.Contains(err.Error(), "required Meta-AST") {
			t.Fatalf("expected required planner error, got %v", err)
		}
		var functions meshv1.FunctionList
		if err := kube.List(context.Background(), &functions); err != nil {
			t.Fatalf("list functions: %v", err)
		}
		if len(functions.Items) != 0 {
			t.Fatalf("required planning failure created resources: %#v", functions.Items)
		}
	})
}

func TestDeployWithPlanFallsBackWhenMergedConfigMapIsTooLarge(t *testing.T) {
	source := plannedSourceFixture(t)
	for _, script := range []string{"service-a/a.py", "service-b/b.py"} {
		writeFixtureFile(t, filepath.Join(source, filepath.FromSlash(script)), strings.Repeat("x", 400))
	}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	server := NewServer(kube, nil, "metacall-functions", "http://router:9090", t.TempDir())
	server.PlannerEnabled = true
	server.MaxConfigMapBytes = 1200
	server.Planner = fakePlanner{result: &meshplanner.Result{Pods: meshplanner.PodManifest{
		Version: meshplanner.SupportedSchemaVersion,
		Deployments: []meshplanner.PodDeployment{{
			ID: 4, Language: "py", Files: []string{"service-a/a.py", "service-b/b.py"},
		}},
	}}}

	result, err := server.deployFromSourceWithPlan(context.Background(), source, "demo", PlanAuto)
	if err != nil {
		t.Fatalf("deploy should use size fallback: %v", err)
	}
	if result.Planning.Status != "fallback" || !strings.Contains(result.Planning.FallbackReason, "exceeding") {
		t.Fatalf("unexpected size fallback: %#v", result.Planning)
	}
	if len(result.Functions) != 2 {
		t.Fatalf("expected two legacy functions after size fallback, got %#v", result.Functions)
	}
}
func TestDeployPrunesStaleResourcesInDeploymentGroup(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	writeFixtureFile(t, filepath.Join(source, "metacall.json"), `{"language_id":"py","path":".","scripts":["main.py"]}`)
	writeFixtureFile(t, filepath.Join(source, "main.py"), "def main():\n    return 1\n")

	labels := map[string]string{labelDeployGroup: "demo", labelComponent: "function"}
	oldFunction := &meshv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "demo-old", Namespace: "metacall-functions", Labels: labels}}
	oldConfigMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      "demo-old-code",
		Namespace: "metacall-functions",
		Labels:    map[string]string{labelDeployGroup: "demo", labelComponent: "source"},
	}}
	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(oldFunction, oldConfigMap).Build()
	server := NewServer(kube, nil, "metacall-functions", "http://router:9090", t.TempDir())

	if _, err := server.deployFromSourceWithPlan(ctx, source, "demo", PlanOff); err != nil {
		t.Fatalf("deploy failed: %v", err)
	}
	if err := kube.Get(ctx, types.NamespacedName{Name: oldFunction.Name, Namespace: oldFunction.Namespace}, &meshv1.Function{}); err == nil {
		t.Fatalf("stale Function was not pruned")
	}
	if err := kube.Get(ctx, types.NamespacedName{Name: oldConfigMap.Name, Namespace: oldConfigMap.Namespace}, &corev1.ConfigMap{}); err == nil {
		t.Fatalf("stale source ConfigMap was not pruned")
	}
}

func plannedSourceFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, service := range []struct {
		dir    string
		script string
		body   string
	}{
		{dir: "service-a", script: "a.py", body: "def a():\n    return 'a'\n"},
		{dir: "service-b", script: "b.py", body: "def b():\n    return 'b'\n"},
	} {
		dir := filepath.Join(root, service.dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create fixture directory: %v", err)
		}
		writeFixtureFile(t, filepath.Join(dir, "metacall.json"), `{"language_id":"py","path":".","scripts":["`+service.script+`"]}`)
		writeFixtureFile(t, filepath.Join(dir, service.script), service.body)
	}
	return root
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}
