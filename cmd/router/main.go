package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	meshv1 "github.com/metacall/function-mesh/api/v1"
	meshrouter "github.com/metacall/function-mesh/internal/router"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(meshv1.AddToScheme(scheme))
}

func main() {
	var listenAddr string
	var namespace string
	var refreshInterval time.Duration

	flag.StringVar(&listenAddr, "listen", ":9090", "HTTP listen address.")
	flag.StringVar(&namespace, "namespace", "metacall-functions", "Namespace containing Function resources.")
	flag.DurationVar(&refreshInterval, "refresh-interval", 5*time.Second, "Function registry refresh interval.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	kube, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		ctrl.Log.Error(err, "unable to create Kubernetes client")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tp, err := meshrouter.InitTracer(ctx)
	if err != nil {
		ctrl.Log.Error(err, "unable to initialize tracer")
	} else if tp != nil {
		defer func() {
			if err := tp.Shutdown(context.Background()); err != nil {
				ctrl.Log.Error(err, "error shutting down tracer")
			}
		}()
	}

	server := meshrouter.NewServer(kube, namespace)

	go server.SyncRegistry(ctx, refreshInterval)

	ctrl.Log.Info("starting function-mesh router", "listen", listenAddr)
	if err := http.ListenAndServe(listenAddr, server.Handler()); err != nil {
		ctrl.Log.Error(err, "router stopped")
		os.Exit(1)
	}
}
