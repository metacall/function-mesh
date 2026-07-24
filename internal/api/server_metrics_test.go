package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	meshv1 "github.com/metacall/function-mesh/api/v1"
)

func TestMetricsMiddlewareAndEndpoint(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add Kubernetes scheme: %v", err)
	}
	if err := meshv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Function scheme: %v", err)
	}
	function := &meshv1.Function{ObjectMeta: metav1.ObjectMeta{
		Name: "hello-py", Namespace: "metacall-functions",
	}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(function).Build()
	server := NewServer(kube, nil, "metacall-functions", "http://router", t.TempDir())

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/readiness", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("readiness status: %d", response.Code)
	}
	deletion := httptest.NewRecorder()
	server.Handler().ServeHTTP(deletion, httptest.NewRequest(
		http.MethodPost,
		"/api/deploy/delete",
		strings.NewReader(`{"suffix":"missing"}`),
	))
	if deletion.Code != http.StatusOK {
		t.Fatalf("delete status: %d", deletion.Code)
	}

	metrics := httptest.NewRecorder()
	server.Handler().ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := metrics.Body.String()
	for _, expected := range []string{
		`metacall_api_request_total{method="GET",path="/api/readiness",status="200"} 1`,
		`metacall_api_request_duration_seconds_count{method="GET",path="/api/readiness"} 1`,
		`metacall_api_deploy_total{operation="delete",status="200"} 1`,
		`metacall_api_active_functions 1`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("metrics missing %q:\n%s", expected, body)
		}
	}
}
