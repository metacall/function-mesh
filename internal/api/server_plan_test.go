package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	meshplanner "github.com/metacall/function-mesh/internal/planner"
)

func TestPackageCreateAcceptsPlanAndReturnsSummary(t *testing.T) {
	var archive bytes.Buffer
	zipWriter := zip.NewWriter(&archive)
	for name, content := range map[string]string{
		"metacall.json": `{"language_id":"py","path":".","scripts":["main.py"]}`,
		"main.py":       "def main():\n    return 1\n",
	} {
		entry, err := zipWriter.Create(name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("id", "demo"); err != nil {
		t.Fatalf("write id: %v", err)
	}
	if err := form.WriteField("plan", PlanAuto); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	file, err := form.CreateFormFile("file", "package.zip")
	if err != nil {
		t.Fatalf("create upload field: %v", err)
	}
	if _, err := file.Write(archive.Bytes()); err != nil {
		t.Fatalf("write upload: %v", err)
	}
	if err := form.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}

	kube := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	server := NewServer(kube, nil, "metacall-functions", "http://router:9090", t.TempDir())
	server.PlannerEnabled = true
	server.Planner = fakePlanner{result: &meshplanner.Result{Pods: meshplanner.PodManifest{
		Version:     meshplanner.SupportedSchemaVersion,
		Deployments: []meshplanner.PodDeployment{{ID: 0, Language: "py", Files: []string{"main.py"}}},
	}}}

	request := httptest.NewRequest(http.MethodPost, "/api/package/create", &body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	var payload struct {
		Plan PlanningSummary `json:"plan"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Plan.Requested != PlanAuto || payload.Plan.Status != "applied" || payload.Plan.PlannedUnits != 1 {
		t.Fatalf("unexpected plan response: %#v", payload.Plan)
	}
}
