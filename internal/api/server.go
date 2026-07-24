package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	meshv1 "github.com/metacall/function-mesh/api/v1"
)

type Server struct {
	Client     client.Client
	Clientset  kubernetes.Interface
	Namespace  string
	RouterURL  string
	WorkDir    string
	HTTPClient *http.Client

	requests        *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	deploys         *prometheus.CounterVec
	activeFunctions prometheus.Gauge
	registry        *prometheus.Registry
}

func NewServer(kube client.Client, clientset kubernetes.Interface, namespace, routerURL, workDir string) *Server {
	if workDir == "" {
		workDir = os.TempDir()
	}
	registry := prometheus.NewRegistry()
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "metacall_api_request_total",
		Help: "Total HTTP requests handled by the Function Mesh API.",
	}, []string{"method", "path", "status"})
	requestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "metacall_api_request_duration_seconds",
		Help:    "Duration of Function Mesh API HTTP requests.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})
	deploys := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "metacall_api_deploy_total",
		Help: "Total Function deployment operations.",
	}, []string{"operation", "status"})
	activeFunctions := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "metacall_api_active_functions",
		Help: "Current number of Function resources managed by the API.",
	})
	registry.MustRegister(requests, requestDuration, deploys, activeFunctions)

	return &Server{
		Client:          kube,
		Clientset:       clientset,
		Namespace:       namespace,
		RouterURL:       strings.TrimRight(routerURL, "/"),
		WorkDir:         workDir,
		HTTPClient:      &http.Client{Timeout: 60 * time.Second},
		requests:        requests,
		requestDuration: requestDuration,
		deploys:         deploys,
		activeFunctions: activeFunctions,
		registry:        registry,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/readiness", s.handleReadiness)
	mux.HandleFunc("/validate", s.handleValidate)
	mux.HandleFunc("/api/account/deploy-enabled", s.handleValidate)
	mux.HandleFunc("/api/repository/add", s.handleRepositoryAdd)
	mux.HandleFunc("/api/repository/branchlist", s.handleBranchList)
	mux.HandleFunc("/api/repository/filelist", s.handleFileList)
	mux.HandleFunc("/api/package/create", s.handlePackageCreate)
	mux.HandleFunc("/api/deploy/create", s.handleDeployCreate)
	mux.HandleFunc("/api/deploy/delete", s.handleDeployDelete)
	mux.HandleFunc("/api/deploy/logs", s.handleDeployLogs)
	mux.HandleFunc("/api/inspect", s.handleInspect)
	mux.HandleFunc("/api/billing/", s.handleBillingStub)
	mux.HandleFunc("/", s.handleFallback)
	mux.Handle("/metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	return s.metricsMiddleware(mux)
}

type statusResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			s.updateActiveFunctions(r.Context())
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		recorder := &statusResponseWriter{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		path := metricPath(r)
		statusLabel := strconv.Itoa(status)
		s.requests.WithLabelValues(r.Method, path, statusLabel).Inc()
		s.requestDuration.WithLabelValues(r.Method, path).Observe(time.Since(started).Seconds())
		if operation := deployOperation(path); operation != "" {
			s.deploys.WithLabelValues(operation, statusLabel).Inc()
		}
	})
}

func (s *Server) updateActiveFunctions(ctx context.Context) {
	if s.Client == nil {
		return
	}
	var functions meshv1.FunctionList
	if err := s.Client.List(ctx, &functions, client.InNamespace(s.Namespace)); err == nil {
		s.activeFunctions.Set(float64(len(functions.Items)))
	}
}

func metricPath(r *http.Request) string {
	if strings.Contains(r.URL.Path, "/call/") {
		return "/call/{function}"
	}
	if r.Pattern == "/" || r.Pattern == "" {
		return "/unmatched"
	}
	return r.Pattern
}

func deployOperation(path string) string {
	switch path {
	case "/api/deploy/create", "/api/repository/add", "/api/package/create":
		return "create"
	case "/api/deploy/delete":
		return "delete"
	default:
		return ""
	}
}

func (s *Server) handleReadiness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ready": true})
}

func (s *Server) handleValidate(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true, "deployEnabled": true})
}

func (s *Server) handleBillingStub(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": []any{}, "deploys": []any{}})
}

func (s *Server) handleRepositoryAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		URL    string `json:"url"`
		Branch string `json:"branch"`
		Suffix string `json:"suffix"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}
	deploymentID := deploymentID(req.Suffix, req.URL)
	path, cleanup, err := s.cloneRepository(r.Context(), req.URL, req.Branch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer cleanup()

	deployed, err := s.deployFromSource(r.Context(), path, deploymentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": deploymentID, "deployment": deploymentID, "functions": deployed})
}

func (s *Server) handleBranchList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := exec.CommandContext(r.Context(), "git", "ls-remote", "--heads", req.URL).CombinedOutput()
	if err != nil {
		http.Error(w, strings.TrimSpace(string(out)), http.StatusBadRequest)
		return
	}
	var branches []string
	for _, line := range strings.Split(string(out), "\n") {
		if _, branch, ok := strings.Cut(line, "refs/heads/"); ok {
			branches = append(branches, strings.TrimSpace(branch))
		}
	}
	sort.Strings(branches)
	writeJSON(w, http.StatusOK, map[string][]string{"branches": branches})
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		URL    string `json:"url"`
		Branch string `json:"branch"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	path, cleanup, err := s.cloneRepository(r.Context(), req.URL, req.Branch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer cleanup()

	files, err := listFiles(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"files": files})
}

func (s *Server) handlePackageCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	deploymentID := sanitizeName(r.FormValue("id"))
	if deploymentID == "" || deploymentID == "function" {
		deploymentID = sanitizeName(r.FormValue("suffix"))
	}
	if deploymentID == "" || deploymentID == "function" {
		deploymentID = fmt.Sprintf("package-%d", time.Now().Unix())
	}

	path, cleanup, err := s.extractPackage(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer cleanup()

	deployed, err := s.deployFromSource(r.Context(), path, deploymentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": deploymentID, "deployment": deploymentID, "functions": deployed})
}

func (s *Server) handleDeployCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Suffix       string   `json:"suffix"`
		Path         string   `json:"path"`
		ResourceType string   `json:"resourceType"`
		Release      string   `json:"release"`
		Env          []string `json:"env"`
		Plan         string   `json:"plan"`
		Version      string   `json:"version"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	suffix := sanitizeName(req.Suffix)
	if req.Path != "" {
		deployed, err := s.deployFromSource(r.Context(), req.Path, suffix)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"prefix":    hostname(),
			"suffix":    suffix,
			"version":   version(req.Version),
			"functions": deployed,
		})
		return
	}
	if suffix == "" || suffix == "function" {
		http.Error(w, "suffix is required", http.StatusBadRequest)
		return
	}
	exists, err := s.deploymentExists(r.Context(), suffix)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists {
		http.Error(w, "invalid deployment id: "+suffix, http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"prefix":  hostname(),
		"suffix":  suffix,
		"version": version(req.Version),
	})
}

func (s *Server) handleDeployDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Suffix string `json:"suffix"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.deleteDeployment(r.Context(), sanitizeName(req.Suffix)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

func (s *Server) handleDeployLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Suffix string `json:"suffix"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if s.Clientset == nil {
		writeJSON(w, http.StatusOK, map[string]string{"logs": ""})
		return
	}
	pods, err := s.Clientset.CoreV1().Pods(s.Namespace).List(r.Context(), metav1.ListOptions{
		LabelSelector: labelDeployGroup + "=" + sanitizeName(req.Suffix),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tailLines := int64(200)
	sinceSeconds := int64(3600)
	var logs strings.Builder
	for _, pod := range pods.Items {
		// Skip pods that aren't running yet — no logs to read
		if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
			_, _ = fmt.Fprintf(&logs, "==> %s [%s]\n", pod.Name, pod.Status.Phase)
			continue
		}

		// Use a per-pod timeout so one stuck pod doesn't block the entire request
		podCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		stream, err := s.Clientset.CoreV1().Pods(s.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			TailLines:    &tailLines,
			SinceSeconds: &sinceSeconds,
		}).Stream(podCtx)
		if err != nil {
			cancel()
			continue
		}
		_, _ = fmt.Fprintf(&logs, "==> %s\n", pod.Name)
		_, _ = io.Copy(&logs, stream)
		_ = stream.Close()
		cancel()
	}
	writeJSON(w, http.StatusOK, map[string]string{"logs": logs.String()})
}

func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var list meshv1.FunctionList
	if err := s.Client.List(r.Context(), &list, client.InNamespace(s.Namespace)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, inspectDeployments(list.Items))
}

func (s *Server) handleFallback(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/call/") {
		s.forwardCall(w, r)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) forwardCall(w http.ResponseWriter, r *http.Request) {
	prefix, functionName, ok := strings.Cut(r.URL.Path, "/call/")
	if !ok || strings.Trim(functionName, "/") == "" {
		http.Error(w, "missing function name", http.StatusBadRequest)
		return
	}
	suffix := callSuffix(prefix)
	target := s.RouterURL + "/call/" + strings.Trim(functionName, "/")
	if suffix != "" {
		target = s.RouterURL + "/call/" + suffix + "/" + strings.Trim(functionName, "/")
	}
	httpClient := s.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	args, err := requestArgs(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	body, err := json.Marshal(args)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header = r.Header.Clone()
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func requestArgs(r *http.Request) ([]any, error) {
	if r.Method == http.MethodGet || r.Body == nil {
		return []any{}, nil
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return []any{}, nil
	}
	var payload any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	switch value := payload.(type) {
	case nil:
		return []any{}, nil
	case []any:
		return value, nil
	case map[string]any:
		if args, ok := value["args"].([]any); ok {
			return args, nil
		}
		return orderedObjectValues(data)
	default:
		return []any{value}, nil
	}
}

func orderedObjectValues(data []byte) ([]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return nil, fmt.Errorf("expected JSON object")
	}
	var values []any
	for decoder.More() {
		if _, err := decoder.Token(); err != nil {
			return nil, err
		}
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func callSuffix(prefix string) string {
	parts := strings.Split(strings.Trim(prefix, "/"), "/")
	if len(parts) >= 2 {
		return sanitizeName(parts[len(parts)-2])
	}
	return ""
}

func (s *Server) cloneRepository(ctx context.Context, url, branch string) (string, func(), error) {
	dir, err := os.MkdirTemp(s.WorkDir, "function-mesh-repo-*")
	if err != nil {
		return "", nil, err
	}
	args := []string{"clone", "--depth", "1"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	args = append(args, url, dir)
	if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("git clone failed: %s", strings.TrimSpace(string(out)))
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

func (s *Server) extractPackage(r *http.Request) (string, func(), error) {
	dir, err := os.MkdirTemp(s.WorkDir, "function-mesh-package-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	var reader io.Reader
	file, _, err := r.FormFile("file")
	if err != nil {
		file, _, err = r.FormFile("package")
	}
	if err == nil {
		defer file.Close()
		reader = file
	} else {
		reader = r.Body
	}

	zipPath := filepath.Join(dir, "package.zip")
	out, err := os.Create(zipPath)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if _, err := io.Copy(out, reader); err != nil {
		_ = out.Close()
		cleanup()
		return "", nil, err
	}
	_ = out.Close()
	if err := unzip(zipPath, dir); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

func unzip(zipPath, dest string) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer reader.Close()
	for _, file := range reader.File {
		target := filepath.Join(dest, file.Name)
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("invalid zip path %s", file.Name)
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		src, err := file.Open()
		if err != nil {
			return err
		}
		dst, err := os.Create(target)
		if err != nil {
			_ = src.Close()
			return err
		}
		_, copyErr := io.Copy(dst, src)
		_ = src.Close()
		_ = dst.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	return nil
}

func listFiles(root string) ([]string, error) {
	var files []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func deploymentID(suffix, seed string) string {
	if suffix != "" {
		return sanitizeName(suffix)
	}
	base := strings.TrimSuffix(filepath.Base(seed), ".git")
	return sanitizeName(base)
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "function-mesh"
	}
	return name
}

func version(value string) string {
	if value == "" {
		return "v1"
	}
	return value
}

func inspectDeployments(functions []meshv1.Function) []map[string]any {
	type group struct {
		status   string
		ports    []int
		packages map[string][]map[string]any
	}
	groups := map[string]*group{}
	for _, fn := range functions {
		suffix := fn.Spec.DeployGroup
		if suffix == "" {
			suffix = fn.Name
		}
		item := groups[suffix]
		if item == nil {
			item = &group{status: "ready", ports: []int{8080}, packages: map[string][]map[string]any{}}
			groups[suffix] = item
		}
		if fn.Status.Phase == "Failed" || fn.Status.Phase == "Degraded" {
			item.status = "failed"
		} else if fn.Status.Phase != "Live" && item.status != "failed" {
			item.status = "create"
		}
		funcs := make([]map[string]any, 0, len(fn.Status.Functions))
		for _, functionName := range fn.Status.Functions {
			funcs = append(funcs, map[string]any{
				"name":  functionName,
				"async": false,
				"signature": map[string]any{
					"args": []any{},
					"ret":  map[string]any{"type": map[string]any{"id": 0}},
				},
			})
		}
		item.packages[fn.Spec.Language] = append(item.packages[fn.Spec.Language], map[string]any{
			"scope": map[string]any{"funcs": funcs},
		})
	}

	out := make([]map[string]any, 0, len(groups))
	for suffix, item := range groups {
		out = append(out, map[string]any{
			"prefix":   hostname(),
			"suffix":   suffix,
			"version":  "v1",
			"status":   item.status,
			"ports":    item.ports,
			"packages": item.packages,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["suffix"].(string) < out[j]["suffix"].(string)
	})
	return out
}
