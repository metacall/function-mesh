package monitoring

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestMonitoringAssetsAreValidAndDashboardConfigMapIsCurrent(t *testing.T) {
	root := filepath.Join("..", "..", "deploy", "monitoring")
	dashboardBytes := readAsset(t, filepath.Join(root, "grafana-dashboard.json"))
	var dashboard any
	if err := json.Unmarshal(dashboardBytes, &dashboard); err != nil {
		t.Fatalf("dashboard JSON is invalid: %v", err)
	}

	var dashboardConfigMap struct {
		Data map[string]string `json:"data"`
	}
	if err := yaml.Unmarshal(readAsset(t, filepath.Join(root, "grafana-dashboard-configmap.yaml")), &dashboardConfigMap); err != nil {
		t.Fatalf("dashboard ConfigMap YAML is invalid: %v", err)
	}
	var embeddedDashboard any
	if err := json.Unmarshal([]byte(dashboardConfigMap.Data["function-mesh.json"]), &embeddedDashboard); err != nil {
		t.Fatalf("dashboard embedded in ConfigMap is invalid: %v", err)
	}
	if !reflect.DeepEqual(dashboard, embeddedDashboard) {
		t.Fatal("dashboard ConfigMap does not match grafana-dashboard.json")
	}
	for _, expected := range []string{`"uid":"loki"`, `"title":"Kubernetes Pod Logs"`, `"name":"log_namespace"`} {
		if !strings.Contains(string(dashboardBytes), expected) {
			t.Errorf("dashboard does not contain %s", expected)
		}
	}

	for _, name := range []string{"kube-prometheus-stack-values.yaml", "grafana-dashboard-configmap.yaml"} {
		var document any
		if err := yaml.Unmarshal(readAsset(t, filepath.Join(root, name)), &document); err != nil {
			t.Errorf("%s is invalid YAML: %v", name, err)
		}
	}
	for _, name := range []string{"servicemonitors.yaml", "loki.yaml", "alloy.yaml"} {
		for index, document := range strings.Split(string(readAsset(t, filepath.Join(root, name))), "\n---") {
			var value any
			if err := yaml.Unmarshal([]byte(document), &value); err != nil {
				t.Errorf("%s document %d is invalid: %v", name, index+1, err)
			}
		}
	}

	values := string(readAsset(t, filepath.Join(root, "kube-prometheus-stack-values.yaml")))
	if !strings.Contains(values, "uid: loki") || !strings.Contains(values, "http://loki.monitoring.svc.cluster.local:3100") {
		t.Fatal("Grafana Loki datasource is not provisioned")
	}

	alloy := string(readAsset(t, filepath.Join(root, "alloy.yaml")))
	for _, expected := range []string{`path_targets = discovery.relabel.pod_logs.output`, `targets       = local.file_match.pod_logs.targets`} {
		if !strings.Contains(alloy, expected) {
			t.Errorf("Alloy pod log file discovery does not contain %q", expected)
		}
	}
}

func TestRuntimeManifestIncludesPrometheusClient(t *testing.T) {
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	path := filepath.Join("..", "..", "runtime", "package.json")
	if err := json.Unmarshal(readAsset(t, path), &manifest); err != nil {
		t.Fatalf("runtime package.json is invalid: %v", err)
	}
	if manifest.Dependencies["prom-client"] == "" {
		t.Fatal("runtime package.json does not include prom-client")
	}
}

func readAsset(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
