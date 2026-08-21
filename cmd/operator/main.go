package main

import (
	"flag"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	meshv1 "github.com/metacall/function-mesh/api/v1"
	"github.com/metacall/function-mesh/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(meshv1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var runtimeImage string
	var runtimeCPURequest string
	var runtimeCPULimit string
	var runtimeMemoryRequest string
	var runtimeMemoryLimit string
	var leaderElect bool
	var l7Visibility bool
	var tracingEnabled bool
	var tracingEndpoint string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&runtimeImage, "runtime-image", "localhost:5000/metacall/function", "Runtime image repository used for Function pods.")
	flag.StringVar(&runtimeCPURequest, "runtime-cpu-request", "100m", "Default runtime CPU request.")
	flag.StringVar(&runtimeCPULimit, "runtime-cpu-limit", "500m", "Default runtime CPU limit.")
	flag.StringVar(&runtimeMemoryRequest, "runtime-memory-request", "128Mi", "Default runtime memory request.")
	flag.StringVar(&runtimeMemoryLimit, "runtime-memory-limit", "512Mi", "Default runtime memory limit.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for controller manager.")
	flag.BoolVar(&l7Visibility, "l7-visibility", false, "Enable L7 proxy visibility for Hubble.")
	flag.BoolVar(&tracingEnabled, "tracing-enabled", false, "Enable OpenTelemetry tracing for function pods.")
	flag.StringVar(&tracingEndpoint, "tracing-endpoint", "", "OpenTelemetry OTLP endpoint (e.g. http://alloy...:4318)")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "function-mesh-operator.metacall.io",
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.FunctionReconciler{
		Client:                 mgr.GetClient(),
		Scheme:                 mgr.GetScheme(),
		RuntimeImageRepository: runtimeImage,
		RuntimeDefaultResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(runtimeCPURequest),
				corev1.ResourceMemory: resource.MustParse(runtimeMemoryRequest),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(runtimeCPULimit),
				corev1.ResourceMemory: resource.MustParse(runtimeMemoryLimit),
			},
		},
		L7Visibility: l7Visibility,
		TracingEnabled: tracingEnabled,
		TracingEndpoint: tracingEndpoint,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create Function controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting function-mesh operator")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}
}
