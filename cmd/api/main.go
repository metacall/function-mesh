package main

import (
	"flag"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	meshv1 "github.com/metacall/function-mesh/api/v1"
	meshapi "github.com/metacall/function-mesh/internal/api"
	meshplanner "github.com/metacall/function-mesh/internal/planner"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(meshv1.AddToScheme(scheme))
}

func main() {
	var listenAddr string
	var namespace string
	var routerURL string
	var workDir string
	var plannerEnabled bool
	var plannerBinary string
	var plannerTimeout time.Duration
	var plannerMaxOutputBytes int64
	var plannerMaxInputFiles int
	var plannerMaxInputBytes int64
	var maxConfigMapBytes int

	flag.StringVar(&listenAddr, "listen", ":9000", "HTTP listen address.")
	flag.StringVar(&namespace, "namespace", "metacall-functions", "Namespace containing Function resources.")
	flag.StringVar(&routerURL, "router-url", "http://function-mesh-router.metacall-system.svc.cluster.local:9090", "Router base URL.")
	flag.StringVar(&workDir, "work-dir", os.TempDir(), "Working directory for repository and package staging.")
	flag.BoolVar(&plannerEnabled, "planner-enabled", false, "Enable Meta-AST deployment planning.")
	flag.StringVar(&plannerBinary, "planner-binary", "/usr/local/bin/meta-ast", "Path to the Meta-AST executable.")
	flag.DurationVar(&plannerTimeout, "planner-timeout", 30*time.Second, "Maximum duration of one Meta-AST planning run.")
	flag.Int64Var(&plannerMaxOutputBytes, "planner-max-output-bytes", 5<<20, "Maximum size of each Meta-AST output artifact.")
	flag.IntVar(&plannerMaxInputFiles, "planner-max-input-files", 5000, "Maximum source file count accepted by the planner.")
	flag.Int64Var(&plannerMaxInputBytes, "planner-max-input-bytes", 50<<20, "Maximum source bytes accepted by the planner.")
	flag.IntVar(&maxConfigMapBytes, "max-configmap-bytes", 900*1024, "Maximum encoded source ConfigMap payload size.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	config := ctrl.GetConfigOrDie()
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		ctrl.Log.Error(err, "unable to create Kubernetes client")
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		ctrl.Log.Error(err, "unable to create Kubernetes clientset")
		os.Exit(1)
	}

	server := meshapi.NewServer(kube, clientset, namespace, routerURL, workDir)
	server.PlannerEnabled = plannerEnabled
	server.MaxConfigMapBytes = maxConfigMapBytes
	if plannerEnabled {
		server.Planner = meshplanner.NewExecRunner(meshplanner.ExecConfig{
			Binary: plannerBinary, WorkDir: workDir, Timeout: plannerTimeout,
			MaxOutputBytes: plannerMaxOutputBytes,
			MaxInputFiles:  plannerMaxInputFiles, MaxInputBytes: plannerMaxInputBytes,
		})
	}
	ctrl.Log.Info("starting function-mesh API", "listen", listenAddr, "plannerEnabled", plannerEnabled)
	if err := http.ListenAndServe(listenAddr, server.Handler()); err != nil {
		ctrl.Log.Error(err, "api server stopped")
		os.Exit(1)
	}
}
