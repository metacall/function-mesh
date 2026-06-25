package main

import (
	"flag"
	"net/http"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	meshv1 "github.com/metacall/function-mesh/api/v1"
	meshapi "github.com/metacall/function-mesh/internal/api"
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

	flag.StringVar(&listenAddr, "listen", ":9000", "HTTP listen address.")
	flag.StringVar(&namespace, "namespace", "metacall-functions", "Namespace containing Function resources.")
	flag.StringVar(&routerURL, "router-url", "http://function-mesh-router.metacall-system.svc.cluster.local:9090", "Router base URL.")
	flag.StringVar(&workDir, "work-dir", os.TempDir(), "Working directory for repository and package staging.")
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
	ctrl.Log.Info("starting function-mesh API", "listen", listenAddr)
	if err := http.ListenAndServe(listenAddr, server.Handler()); err != nil {
		ctrl.Log.Error(err, "api server stopped")
		os.Exit(1)
	}
}
