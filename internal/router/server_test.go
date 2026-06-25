package router

import (
	"context"
	"io"
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

func TestRegistryRefreshBuildsFunctionRoutes(t *testing.T) {
	scheme := testScheme(t)
	fn := &meshv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "auth-py", Namespace: "metacall-functions"},
		Spec: meshv1.FunctionSpec{
			DeployGroup: "demo",
		},
		Status: meshv1.FunctionStatus{
			ServiceURL: "http://auth-py.metacall-functions.svc.cluster.local:8080/",
			Functions:  []string{"signin", "signup"},
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fn).Build()
	registry := NewRegistry()

	if err := registry.Refresh(context.Background(), client, "metacall-functions"); err != nil {
		t.Fatalf("refresh failed: %v", err)
	}

	route, ok := registry.Lookup("demo", "signin")
	if !ok {
		t.Fatalf("missing signin route")
	}
	if route.Resource != "auth-py" || route.DeployGroup != "demo" || route.ServiceURL != "http://auth-py.metacall-functions.svc.cluster.local:8080/" {
		t.Fatalf("unexpected route: %#v", route)
	}
}

func TestHandleCallProxiesToRuntime(t *testing.T) {
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/call/signin" {
			t.Fatalf("unexpected runtime path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `["admin"]` {
			t.Fatalf("unexpected runtime body: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`"token"`))
	}))
	defer runtime.Close()

	server := NewServer(nil, "")
	mockRoutes := map[string]FunctionRoute{
		routeKey("demo", "signin"): {Name: "signin", DeployGroup: "demo", ServiceURL: runtime.URL + "/"},
	}
	server.Registry.routes.Store(&mockRoutes)

	req := httptest.NewRequest(http.MethodPost, "/call/demo/signin", strings.NewReader(`["admin"]`))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != `"token"` {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := meshv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add mesh scheme: %v", err)
	}
	return scheme
}
