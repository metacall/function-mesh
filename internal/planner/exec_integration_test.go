package planner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExecRunnerFunctionMeshAppIntegration(t *testing.T) {
	binary := os.Getenv("META_AST_INTEGRATION_BINARY")
	if binary == "" {
		t.Skip("set META_AST_INTEGRATION_BINARY to run the real Meta-AST integration test")
	}

	fixture := os.Getenv("META_AST_INTEGRATION_FIXTURE")
	if fixture == "" {
		fixture = filepath.Join("..", "..", "..", "test", "function-mesh-app")
	}
	fixture, err := filepath.Abs(fixture)
	if err != nil {
		t.Fatalf("resolve integration fixture: %v", err)
	}
	if _, err := os.Stat(fixture); err != nil {
		t.Skipf("function-mesh-app fixture is unavailable: %v", err)
	}

	runner := NewExecRunner(ExecConfig{
		Binary:  binary,
		WorkDir: t.TempDir(),
		Timeout: 30 * time.Second,
	})
	result, err := runner.Plan(t.Context(), fixture)
	if err != nil {
		t.Fatalf("plan function-mesh-app: %v", err)
	}
	if result.Pods.Version != SupportedSchemaVersion {
		t.Fatalf("unexpected manifest version %q", result.Pods.Version)
	}

	referenceEdges := 0
	for _, edge := range result.Pods.Edges {
		if edge.Kind == "reference" && edge.CrossLanguage {
			referenceEdges++
		}
	}
	if referenceEdges != 3 {
		t.Fatalf("expected three cross-language reference edges, got %d: %#v", referenceEdges, result.Pods.Edges)
	}

	invoicePodID := -1
	orderPodID := -1
	for _, deployment := range result.Pods.Deployments {
		for _, file := range deployment.Files {
			if filepath.IsAbs(file) {
				t.Fatalf("planner path was not normalized relative to the fixture: %q", file)
			}
			switch file {
			case "invoice-client/invoice_client.py":
				invoicePodID = deployment.ID
			case "order-client/order_client.py":
				orderPodID = deployment.ID
			}
		}
	}
	if invoicePodID < 0 || orderPodID < 0 {
		t.Fatalf("Python clients are missing from the plan: %#v", result.Pods.Deployments)
	}
	if invoicePodID != orderPodID {
		t.Fatalf("expected Python clients in the same pod, got invoice pod %d and order pod %d", invoicePodID, orderPodID)
	}
}
