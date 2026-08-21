package router

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"

	"sigs.k8s.io/controller-runtime/pkg/client"

	meshv1 "github.com/metacall/function-mesh/api/v1"
)

const defaultRuntimePort = 8080

type FunctionRoute struct {
	Name        string `json:"name"`
	Resource    string `json:"resource"`
	Namespace   string `json:"namespace"`
	ServiceURL  string `json:"serviceURL"`
	DeployGroup string `json:"deployGroup,omitempty"`
}

type Registry struct {
	routes atomic.Pointer[map[string]FunctionRoute]
}

func NewRegistry() *Registry {
	r := &Registry{}
	initial := make(map[string]FunctionRoute)
	r.routes.Store(&initial)
	return r
}

func (r *Registry) Lookup(deployGroup, name string) (FunctionRoute, bool) {
	routes := *r.routes.Load()
	if deployGroup != "" {
		route, ok := routes[routeKey(deployGroup, name)]
		return route, ok
	}
	route, ok := routes[routeKey("", name)]
	return route, ok
}

func (r *Registry) List() []FunctionRoute {
	routesMap := *r.routes.Load()
	routes := make([]FunctionRoute, 0, len(routesMap))
	for _, route := range routesMap {
		routes = append(routes, route)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Name == routes[j].Name {
			return routes[i].Resource < routes[j].Resource
		}
		return routes[i].Name < routes[j].Name
	})
	return routes
}

func (r *Registry) Refresh(ctx context.Context, kube client.Client, namespace string) error {
	var functions meshv1.FunctionList
	opts := []client.ListOption{}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := kube.List(ctx, &functions, opts...); err != nil {
		return err
	}

	next := make(map[string]FunctionRoute)
	for _, fn := range functions.Items {
		serviceURL := fn.Status.ServiceURL
		if serviceURL == "" {
			serviceURL = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/", fn.Name, fn.Namespace, defaultRuntimePort)
		}
		for _, functionName := range fn.Status.Functions {
			if functionName == "" {
				continue
			}
			route := FunctionRoute{
				Name:        functionName,
				Resource:    fn.Name,
				Namespace:   fn.Namespace,
				ServiceURL:  serviceURL,
				DeployGroup: fn.Spec.DeployGroup,
			}
			next[routeKey(fn.Spec.DeployGroup, functionName)] = route
			if _, exists := next[routeKey("", functionName)]; !exists {
				next[routeKey("", functionName)] = route
			}
		}
	}

	r.routes.Store(&next)
	return nil
}

func routeKey(deployGroup, functionName string) string {
	return deployGroup + "/" + functionName
}
