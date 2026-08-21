package router
import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/httptrace/otelhttptrace"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
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

	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: otelhttp.NewTransport(http.DefaultTransport,
			otelhttp.WithClientTrace(func(ctx context.Context) *httptrace.ClientTrace {
				return otelhttptrace.NewClientTrace(ctx)
			}),
		),
	}

	return &Server{
		Registry:   NewRegistry(),
		Client:     kube,
		Namespace:  namespace,
		HTTPClient: httpClient,
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
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	tracer := otel.Tracer("router")
	ctx, span := tracer.Start(ctx, "router.handleCall", trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()

	if r.Method != http.MethodPost {
		span.SetAttributes(attribute.Int("http.status_code", http.StatusMethodNotAllowed))
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	deployGroup, functionName := callPathParts(r.URL.Path)
	
	span.SetAttributes(
		attribute.String("deployment_group", deployGroup),
		attribute.String("function_name", functionName),
	)

	if functionName == "" {
		span.SetAttributes(attribute.Int("http.status_code", http.StatusBadRequest))
		http.Error(w, "missing function name", http.StatusBadRequest)
		return
	}

	start := time.Now()
	_, lookupSpan := tracer.Start(ctx, "router.registryLookup")
	route, ok := s.Registry.Lookup(deployGroup, functionName)
	lookupSpan.End()

	if !ok {
		span.SetAttributes(attribute.Int("http.status_code", http.StatusNotFound))
		s.calls.WithLabelValues(functionName, "404").Inc()
		http.Error(w, "function not found", http.StatusNotFound)
		return
	}
	
	span.SetAttributes(attribute.String("target_service", route.ServiceURL))

	status := s.proxy(ctx, w, r, route.ServiceURL+"call/"+functionName)
	
	span.SetAttributes(
		attribute.Int("http.status_code", status),
		attribute.Float64("elapsed_ms", float64(time.Since(start).Milliseconds())),
	)
	s.calls.WithLabelValues(functionName, strconv.Itoa(status)).Inc()
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

func (s *Server) proxy(ctx context.Context, w http.ResponseWriter, r *http.Request, target string) int {
	tracer := otel.Tracer("router")
	ctx, proxySpan := tracer.Start(ctx, "router.proxy", trace.WithSpanKind(trace.SpanKindClient))
	defer proxySpan.End()

	httpClient := s.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target, r.Body)
	if err != nil {
		proxySpan.RecordError(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return http.StatusInternalServerError
	}
	req.Header = r.Header.Clone()
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	resp, err := httpClient.Do(req)
	if err != nil {
		proxySpan.RecordError(err)
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
	
	_, copySpan := tracer.Start(ctx, "router.proxy.responseCopy")
	_, _ = io.Copy(w, resp.Body)
	copySpan.End()
	
	return resp.StatusCode
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
