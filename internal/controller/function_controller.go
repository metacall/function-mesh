package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	meshv1 "github.com/metacall/function-mesh/api/v1"
)

const (
	LabelFunction    = "metacall.io/function"
	LabelLanguage    = "metacall.io/language"
	LabelComponent   = "metacall.io/component"
	LabelDeployGroup = "metacall.io/deploy-group"

	AnnotationEndpointsHash = "metacall.io/endpoints-hash"

	defaultRuntimeImageRepository = "localhost:5000/metacall/function"
	defaultRuntimeImagePullPolicy = corev1.PullIfNotPresent
	defaultPort                   = int32(8080)
	defaultEntrypoint             = "metacall.json"
)

type FunctionReconciler struct {
	client.Client
	Scheme                 *runtime.Scheme
	RuntimeImageRepository string
	RuntimeImagePullPolicy corev1.PullPolicy
	HTTPClient             *http.Client
}

func (r *FunctionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var function meshv1.Function
	if err := r.Get(ctx, req.NamespacedName, &function); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.reconcileAllEndpointMaps(ctx, req.Namespace)
		}
		return ctrl.Result{}, err
	}

	if err := r.reconcileEndpointConfigMap(ctx, &function); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileDeployment(ctx, &function); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileService(ctx, &function); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileAllEndpointMaps(ctx, function.Namespace); err != nil {
		return ctrl.Result{}, err
	}
	needsRequeue, err := r.updateStatus(ctx, &function)
	if err != nil {
		logger.Error(err, "failed to update Function status")
		return ctrl.Result{}, err
	}
	if needsRequeue {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

func (r *FunctionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&meshv1.Function{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}

func (r *FunctionReconciler) reconcileDeployment(ctx context.Context, fn *meshv1.Function) error {
	var deployment appsv1.Deployment
	key := types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}
	if err := r.Get(ctx, key, &deployment); err != nil && !apierrors.IsNotFound(err) {
		return err
	} else if apierrors.IsNotFound(err) {
		deployment = appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: fn.Name, Namespace: fn.Namespace}}
	}

	mutate := func() error {
		deployment.Labels = mergeLabels(deployment.Labels, functionLabels(fn))
		replicas := int32(1)
		if fn.Spec.Runtime.Replicas != nil {
			replicas = *fn.Spec.Runtime.Replicas
		}
		deployment.Spec.Replicas = ptr.To(replicas)
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(fn)}
		deployment.Spec.Template.Labels = mergeLabels(deployment.Spec.Template.Labels, functionLabels(fn))

		// hashing the endpoints to trigger a redeploy when the endpoints change
		deployment.Spec.Template.Annotations = mergeLabels(deployment.Spec.Template.Annotations, map[string]string{
			AnnotationEndpointsHash: hashEndpoints(r.generateEndpoints(ctx, fn)),
		})

		deployment.Spec.Template.Spec.InitContainers = []corev1.Container{{
			Name:            "stage-source",
			Image:           r.runtimeImage(fn),
			ImagePullPolicy: r.imagePullPolicy(),
			Command:         []string{"sh", "-c", "cp -R /source/. /app/"},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "source-code", MountPath: "/source", ReadOnly: true},
				{Name: "app-workdir", MountPath: "/app"},
			},
		}}
		deployment.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:            "runtime",
			Image:           r.runtimeImage(fn),
			ImagePullPolicy: r.imagePullPolicy(),
			Ports: []corev1.ContainerPort{{
				Name:          "http",
				ContainerPort: defaultPort,
				Protocol:      corev1.ProtocolTCP,
			}},
			Env: []corev1.EnvVar{
				{Name: "FUNCTION_CONFIG", Value: "/app/" + entrypoint(fn)},
				{Name: "RPC_CONFIG", Value: "/mesh/metacall-rpc.json"},
				{Name: "PORT", Value: fmt.Sprintf("%d", defaultPort)},
			},
			Resources: fn.Spec.Runtime.Resources,
			VolumeMounts: []corev1.VolumeMount{
				{Name: "app-workdir", MountPath: "/app"},
				{Name: "mesh-endpoints", MountPath: "/mesh", ReadOnly: true},
			},
			ReadinessProbe: healthProbe(),
			LivenessProbe:  healthProbe(),
		}}
		deployment.Spec.Template.Spec.Volumes = []corev1.Volume{
			{
				Name: "source-code",
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: fn.Spec.Source.ConfigMap},
				}},
			},
			{
				Name: "app-workdir",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
			{
				Name: "mesh-endpoints",
				VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: endpointsConfigMapName(fn.Name)},
				}},
			},
		}
		// wiring the deployment to the function
		return controllerutil.SetControllerReference(fn, &deployment, r.Scheme)
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, &deployment, mutate)
	return err
}

func (r *FunctionReconciler) reconcileService(ctx context.Context, fn *meshv1.Function) error {
	var service corev1.Service
	key := types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}
	if err := r.Get(ctx, key, &service); err != nil && !apierrors.IsNotFound(err) {
		return err
	} else if apierrors.IsNotFound(err) {
		service = corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: fn.Name, Namespace: fn.Namespace}}
	}

	mutate := func() error {
		service.Labels = mergeLabels(service.Labels, functionLabels(fn))
		service.Spec.Type = corev1.ServiceTypeClusterIP
		service.Spec.Selector = selectorLabels(fn)
		service.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       defaultPort,
			TargetPort: intstr.FromString("http"),
			Protocol:   corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(fn, &service, r.Scheme)
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, &service, mutate)
	return err
}

func (r *FunctionReconciler) reconcileEndpointConfigMap(ctx context.Context, fn *meshv1.Function) error {
	return r.upsertEndpointConfigMap(ctx, fn, fn)
}

func (r *FunctionReconciler) reconcileAllEndpointMaps(ctx context.Context, namespace string) error {
	var list meshv1.FunctionList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return err
	}
	for i := range list.Items {
		fn := &list.Items[i]
		if err := r.upsertEndpointConfigMap(ctx, fn, fn); err != nil {
			return err
		}
		if err := r.reconcileDeployment(ctx, fn); err != nil {
			return err
		}
	}
	return nil
}

func (r *FunctionReconciler) upsertEndpointConfigMap(ctx context.Context, owner *meshv1.Function, target *meshv1.Function) error {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Name: endpointsConfigMapName(target.Name), Namespace: target.Namespace}
	if err := r.Get(ctx, key, &cm); err != nil && !apierrors.IsNotFound(err) {
		return err
	} else if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: endpointsConfigMapName(target.Name), Namespace: target.Namespace}}
	}

	mutate := func() error {
		cm.Labels = mergeLabels(cm.Labels, functionLabels(target))
		cm.Data = map[string]string{
			"endpoints.txt":     strings.Join(r.generateEndpoints(ctx, target), "\n"),
			"metacall-rpc.json": rpcConfigJSON(),
		}
		return controllerutil.SetControllerReference(owner, &cm, r.Scheme)
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, &cm, mutate)
	return err
}

func (r *FunctionReconciler) generateEndpoints(ctx context.Context, fn *meshv1.Function) []string {
	var list meshv1.FunctionList
	if err := r.List(ctx, &list, client.InNamespace(fn.Namespace)); err != nil {
		return nil
	}

	urls := make([]string, 0, len(list.Items))
	for _, other := range list.Items {
		if other.Name == fn.Name {
			continue
		}
		if !sameDeployGroup(fn, &other) {
			continue
		}
		urls = append(urls, serviceURL(&other))
	}
	sort.Strings(urls)
	return urls
}

func (r *FunctionReconciler) updateStatus(ctx context.Context, fn *meshv1.Function) (bool, error) {
	var deployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, &deployment); err != nil {
		return false, err
	}

	next := fn.DeepCopy()
	next.Status.ServiceURL = serviceURL(fn)
	next.Status.PodCount = deployment.Status.ReadyReplicas
	needsRequeue := deployment.Status.ReadyReplicas == 0
	if deployment.Status.ReadyReplicas > 0 {
		next.Status.Phase = "Running"
	} else if deployment.Status.UnavailableReplicas > 0 {
		next.Status.Phase = "Failed"
	} else {
		next.Status.Phase = "Pending"
	}

	functions, err := r.inspectFunctions(ctx, fn)
	if err == nil {
		next.Status.Functions = functions
	} else if deployment.Status.ReadyReplicas > 0 {
		needsRequeue = true
	}

	if reflect.DeepEqual(fn.Status, next.Status) {
		return needsRequeue, nil
	}
	if err := r.Status().Update(ctx, next); err != nil && apierrors.IsNotFound(err) {
		return needsRequeue, nil
	} else {
		return needsRequeue, err
	}
}

func (r *FunctionReconciler) inspectFunctions(ctx context.Context, fn *meshv1.Function) ([]string, error) {
	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serviceURL(fn)+"inspect", nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("inspect returned %s", resp.Status)
	}

	var data map[string][]struct {
		Scope struct {
			Funcs []struct {
				Name string `json:"name"`
			} `json:"funcs"`
		} `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	var names []string
	for _, scripts := range data {
		for _, script := range scripts {
			for _, fn := range script.Scope.Funcs {
				if fn.Name != "" {
					names = append(names, fn.Name)
				}
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

func functionLabels(fn *meshv1.Function) map[string]string {
	labels := selectorLabels(fn)
	labels[LabelLanguage] = fn.Spec.Language
	labels[LabelComponent] = "function"
	if fn.Spec.DeployGroup != "" {
		labels[LabelDeployGroup] = fn.Spec.DeployGroup
	}
	return labels
}

func selectorLabels(fn *meshv1.Function) map[string]string {
	return map[string]string{LabelFunction: fn.Name}
}

func mergeLabels(base map[string]string, values map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(values))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range values {
		out[k] = v
	}
	return out
}

func (r *FunctionReconciler) runtimeImage(fn *meshv1.Function) string {
	repository := r.RuntimeImageRepository
	if repository == "" {
		repository = defaultRuntimeImageRepository
	}
	if fn.Spec.Language == "" {
		return repository
	}
	return repository + ":" + fn.Spec.Language
}

func (r *FunctionReconciler) imagePullPolicy() corev1.PullPolicy {
	if r.RuntimeImagePullPolicy != "" {
		return r.RuntimeImagePullPolicy
	}
	return defaultRuntimeImagePullPolicy
}

func entrypoint(fn *meshv1.Function) string {
	if fn.Spec.Source.Entrypoint != "" {
		return fn.Spec.Source.Entrypoint
	}
	return defaultEntrypoint
}

func endpointsConfigMapName(name string) string {
	return name + "-endpoints"
}

func serviceURL(fn *meshv1.Function) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/", fn.Name, fn.Namespace, defaultPort)
}

func sameDeployGroup(a, b *meshv1.Function) bool {
	if a.Spec.DeployGroup == "" {
		return true
	}
	return a.Spec.DeployGroup == b.Spec.DeployGroup
}

func rpcConfigJSON() string {
	return `{"language_id":"rpc","path":"/mesh","scripts":["endpoints.txt"]}`
}

func hashEndpoints(endpoints []string) string {
	sum := sha256.Sum256([]byte(strings.Join(endpoints, "\n")))
	return hex.EncodeToString(sum[:])
}

func healthProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromString("http"),
			},
		},
	}
}
