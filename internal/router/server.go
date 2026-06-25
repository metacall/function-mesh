package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Server struct {
	Registry   *Registry
	Client     client.Client
	Namespace  string
	HTTPClient *http.Client

	calls    *prometheus.CounterVec
	duration *prometheus.HistogramVec
	registry *prometheus.Registry
}

func NewServer(kube client.Client, namespace string) *Server {
	promRegistry := prometheus.NewRegistry()
	calls := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "metacall_call_total",
		Help: "Total external function calls routed by function-mesh.",
	}, []string{"function", "status"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "metacall_call_duration_seconds",
		Help:    "External function call routing duration.",
		Buckets: prometheus.DefBuckets,
	}, []string{"function"})
	promRegistry.MustRegister(calls, duration)

	return &Server{
		Registry:   NewRegistry(),
		Client:     kube,
		Namespace:  namespace,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		calls:      calls,
		duration:   duration,
		registry:   promRegistry,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/functions", s.handleFunctions)
	mux.HandleFunc("/call/", s.handleCall)
	mux.Handle("/metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	return mux
}

func (s *Server) SyncRegistry(ctx context.Context, interval time.Duration) {
	if s.Client == nil {
		return
	}
	_ = s.Registry.Refresh(ctx, s.Client, s.Namespace)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.Registry.Refresh(ctx, s.Client, s.Namespace)
		}
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleFunctions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]FunctionRoute{"functions": s.Registry.List()})
}

func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	deployGroup, functionName := callPathParts(r.URL.Path)
	if functionName == "" {
		http.Error(w, "missing function name", http.StatusBadRequest)
		return
	}

	start := time.Now()
	route, ok := s.Registry.Lookup(deployGroup, functionName)
	if !ok {
		s.calls.WithLabelValues(functionName, "404").Inc()
		http.Error(w, "function not found", http.StatusNotFound)
		return
	}

	status := s.proxy(w, r, route.ServiceURL+"call/"+functionName)
	s.calls.WithLabelValues(functionName, http.StatusText(status)).Inc()
	s.duration.WithLabelValues(functionName).Observe(time.Since(start).Seconds())
}

func callPathParts(path string) (string, string) {
	rest := strings.Trim(strings.TrimPrefix(path, "/call/"), "/")
	if rest == "" {
		return "", ""
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 1 {
		return "", parts[0]
	}
	return parts[0], parts[len(parts)-1]
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, target string) int {
	httpClient := s.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return http.StatusInternalServerError
	}
	req.Header = r.Header.Clone()

	resp, err := httpClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return http.StatusBadGateway
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return resp.StatusCode
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
