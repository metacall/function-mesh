package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	meshv1 "github.com/metacall/function-mesh/api/v1"
)

func TestDeployFromSourceCreatesConfigMapAndFunction(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "metacall.json"), []byte(`{
		"language_id": "py",
		"path": ".",
		"scripts": ["hello.py"]
	}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "hello.py"), []byte("def hello():\n    return 'hi'\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	scheme := testScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	server := NewServer(client, nil, "metacall-functions", "http://router:9090", t.TempDir())

	deployed, err := server.deployFromSource(ctx, source, "demo")
	if err != nil {
		t.Fatalf("deploy failed: %v", err)
	}
	if len(deployed) != 1 || deployed[0].Name != "demo-py" {
		t.Fatalf("unexpected deployed functions: %#v", deployed)
	}

	var cm corev1.ConfigMap
	if err := client.Get(ctx, types.NamespacedName{Name: "demo-py-code", Namespace: "metacall-functions"}, &cm); err != nil {
		t.Fatalf("get configmap: %v", err)
	}
	if cm.Labels[labelDeployGroup] != "demo" || cm.Data["hello.py"] == "" {
		t.Fatalf("unexpected configmap: %#v", cm)
	}

	var fn meshv1.Function
	if err := client.Get(ctx, types.NamespacedName{Name: "demo-py", Namespace: "metacall-functions"}, &fn); err != nil {
		t.Fatalf("get function: %v", err)
	}
	if fn.Spec.Language != "py" || fn.Spec.Source.ConfigMap != "demo-py-code" || fn.Spec.DeployGroup != "demo" {
		t.Fatalf("unexpected function spec: %#v", fn.Spec)
	}
}

func TestDeployFromSourceRejectsUnsupportedLanguage(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "metacall.json"), []byte(`{
		"language_id": "go",
		"path": ".",
		"scripts": ["main.go"]
	}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	scheme := testScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	server := NewServer(client, nil, "metacall-functions", "http://router:9090", t.TempDir())

	if _, err := server.deployFromSource(context.Background(), source, "demo"); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected unsupported language error, got %v", err)
	}
}

func TestForwardCallUsesSuffixAndOrderedArgs(t *testing.T) {
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/call/demo/sum" {
			t.Fatalf("unexpected router path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `[1,2]` {
			t.Fatalf("unexpected router body: %s", body)
		}
		_, _ = w.Write([]byte(`3`))
	}))
	defer router.Close()

	server := NewServer(nil, nil, "metacall-functions", router.URL, t.TempDir())
	req := httptest.NewRequest(http.MethodPost, "/host/demo/v1/call/sum", strings.NewReader(`{"a":1,"b":2}`))
	rec := httptest.NewRecorder()

	server.forwardCall(rec, req)

	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `3` {
		t.Fatalf("unexpected response: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDeleteDeploymentRemovesFunctionsAndConfigMaps(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	fn := &meshv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-py",
			Namespace: "metacall-functions",
			Labels:    map[string]string{labelDeployGroup: "demo"},
		},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-py-code",
			Namespace: "metacall-functions",
			Labels:    map[string]string{labelDeployGroup: "demo"},
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, cm).Build()
	server := NewServer(client, nil, "metacall-functions", "http://router:9090", t.TempDir())

	if err := server.deleteDeployment(ctx, "demo"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	if err := client.Get(ctx, types.NamespacedName{Name: "demo-py", Namespace: "metacall-functions"}, &meshv1.Function{}); err == nil {
		t.Fatalf("function still exists")
	}
	if err := client.Get(ctx, types.NamespacedName{Name: "demo-py-code", Namespace: "metacall-functions"}, &corev1.ConfigMap{}); err == nil {
		t.Fatalf("configmap still exists")
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
