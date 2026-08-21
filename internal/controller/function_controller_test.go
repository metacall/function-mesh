package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	meshv1 "github.com/metacall/function-mesh/api/v1"
)

func TestGenerateEndpointsUsesDeployGroupAndSkipsSelf(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	auth := testFunction("auth-py", "py", "app")
	processor := testFunction("processor-node", "node", "app")
	analytics := testFunction("analytics-rb", "rb", "other")
	utility := testFunction("utility-go", "go", "")
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(auth, processor, analytics, utility).Build()
	reconciler := &FunctionReconciler{Client: client, Scheme: scheme}

	got, err := reconciler.generateEndpoints(ctx, auth)
	if err != nil {
		t.Fatalf("generate endpoints failed: %v", err)
	}

	want := []string{"http://processor-node.metacall-functions.svc.cluster.local:8080/"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("unexpected endpoints: got %v want %v", got, want)
	}
}

func TestGenerateEndpointsUngroupedFunctionSeesAllPeers(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	auth := testFunction("auth-py", "py", "app")
	processor := testFunction("processor-node", "node", "app")
	utility := testFunction("utility-go", "go", "")
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(auth, processor, utility).Build()
	reconciler := &FunctionReconciler{Client: client, Scheme: scheme}

	got, err := reconciler.generateEndpoints(ctx, utility)
	if err != nil {
		t.Fatalf("generate endpoints failed: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("expected two peer endpoints, got %v", got)
	}
	if got[0] != "http://auth-py.metacall-functions.svc.cluster.local:8080/" ||
		got[1] != "http://processor-node.metacall-functions.svc.cluster.local:8080/" {
		t.Fatalf("unexpected endpoints: %v", got)
	}
}

func TestReconcileCreatesRuntimeResources(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	fn := testFunction("auth-py", "py", "app")
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fn).Build()
	reconciler := &FunctionReconciler{
		Client:                 client,
		Scheme:                 scheme,
		RuntimeImageRepository: "localhost:5000/metacall/function",
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "auth-py", Namespace: "metacall-functions"}}); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var deployment appsv1.Deployment
	if err := client.Get(ctx, types.NamespacedName{Name: "auth-py", Namespace: "metacall-functions"}, &deployment); err != nil {
		t.Fatalf("deployment not created: %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if len(deployment.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("expected source staging init container, got %d", len(deployment.Spec.Template.Spec.InitContainers))
	}
	initContainer := deployment.Spec.Template.Spec.InitContainers[0]
	if initContainer.Name != "stage-source" || initContainer.Image != "localhost:5000/metacall/function:py" {
		t.Fatalf("unexpected init container: %#v", initContainer)
	}
	if container.Image != "localhost:5000/metacall/function:py" {
		t.Fatalf("unexpected image: %s", container.Image)
	}
	if container.Ports[0].ContainerPort != 8080 {
		t.Fatalf("unexpected container port: %d", container.Ports[0].ContainerPort)
	}
	if envValue(container.Env, "FUNCTION_CONFIG") != "/app/metacall-py.json" {
		t.Fatalf("unexpected FUNCTION_CONFIG: %s", envValue(container.Env, "FUNCTION_CONFIG"))
	}
	if envValue(container.Env, "FUNCTION_NAME") != "auth-py" {
		t.Fatalf("unexpected FUNCTION_NAME: %s", envValue(container.Env, "FUNCTION_NAME"))
	}
	if envValue(container.Env, "RPC_CONFIG") != "/mesh/metacall-rpc.json" {
		t.Fatalf("unexpected RPC_CONFIG: %s", envValue(container.Env, "RPC_CONFIG"))
	}
	if envValue(container.Env, "PORT") != "8080" {
		t.Fatalf("unexpected PORT: %s", envValue(container.Env, "PORT"))
	}
	if envValue(container.Env, "METACALL_RPC_RETRY_COUNT") != "1" {
		t.Fatalf("unexpected METACALL_RPC_RETRY_COUNT: %s", envValue(container.Env, "METACALL_RPC_RETRY_COUNT"))
	}
	if envValue(container.Env, "MESH_REMOTE_MAX_RETRIES") != "30" {
		t.Fatalf("unexpected MESH_REMOTE_MAX_RETRIES: %s", envValue(container.Env, "MESH_REMOTE_MAX_RETRIES"))
	}
	if len(container.VolumeMounts) != 2 {
		t.Fatalf("expected source and mesh mounts, got %d", len(container.VolumeMounts))
	}
	assertVolumeMount(t, initContainer.VolumeMounts, "source-code", "/source")
	assertWritableVolumeMount(t, initContainer.VolumeMounts, "app-workdir", "/app")
	assertWritableVolumeMount(t, container.VolumeMounts, "app-workdir", "/app")
	assertVolumeMount(t, container.VolumeMounts, "mesh-endpoints", "/mesh")
	assertConfigMapVolume(t, deployment.Spec.Template.Spec.Volumes, "source-code", "auth-py-code")
	assertEmptyDirVolume(t, deployment.Spec.Template.Spec.Volumes, "app-workdir")
	assertConfigMapVolume(t, deployment.Spec.Template.Spec.Volumes, "mesh-endpoints", "auth-py-endpoints")
	assertHTTPProbe(t, container.StartupProbe, "/health/ready", "http")
	assertHTTPProbe(t, container.ReadinessProbe, "/health/ready", "http")
	assertHTTPProbe(t, container.LivenessProbe, "/health/live", "http")
	if container.StartupProbe.FailureThreshold != 60 {
		t.Fatalf("unexpected startup failure threshold: %d", container.StartupProbe.FailureThreshold)
	}

	var service corev1.Service
	if err := client.Get(ctx, types.NamespacedName{Name: "auth-py", Namespace: "metacall-functions"}, &service); err != nil {
		t.Fatalf("service not created: %v", err)
	}
	if service.Spec.Ports[0].Port != 8080 {
		t.Fatalf("unexpected service port: %d", service.Spec.Ports[0].Port)
	}

	var cm corev1.ConfigMap
	if err := client.Get(ctx, types.NamespacedName{Name: "auth-py-endpoints", Namespace: "metacall-functions"}, &cm); err != nil {
		t.Fatalf("endpoint configmap not created: %v", err)
	}
	if _, ok := cm.Data["metacall-rpc.json"]; !ok {
		t.Fatalf("missing metacall-rpc.json")
	}
	if cm.Data["metacall-rpc.json"] != `{"language_id":"rpc","path":"/mesh","scripts":["endpoints.json"]}` {
		t.Fatalf("unexpected rpc config: %s", cm.Data["metacall-rpc.json"])
	}
	if cm.Data["endpoints.json"] != `{"urls":[]}` {
		t.Fatalf("single function should have no peer endpoints, got %q", cm.Data["endpoints.json"])
	}
}

func TestReconcileRefreshesPeerEndpointConfigMaps(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	auth := testFunction("auth-py", "py", "app")
	processor := testFunction("processor-node", "node", "app")
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(auth, processor).Build()
	reconciler := &FunctionReconciler{Client: client, Scheme: scheme}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "auth-py", Namespace: "metacall-functions"}}); err != nil {
		t.Fatalf("reconcile auth failed: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "processor-node", Namespace: "metacall-functions"}}); err != nil {
		t.Fatalf("reconcile processor failed: %v", err)
	}

	assertEndpointConfigMap(t, ctx, client, "auth-py-endpoints", "http://processor-node.metacall-functions.svc.cluster.local:8080/")
	assertEndpointConfigMap(t, ctx, client, "processor-node-endpoints", "http://auth-py.metacall-functions.svc.cluster.local:8080/")
	var authDeployment appsv1.Deployment
	if err := client.Get(ctx, types.NamespacedName{Name: "auth-py", Namespace: "metacall-functions"}, &authDeployment); err != nil {
		t.Fatalf("get auth deployment: %v", err)
	}
	hashWithPeer := authDeployment.Spec.Template.Annotations[AnnotationEndpointsHash]
	if hashWithPeer == "" {
		t.Fatalf("missing endpoint hash annotation")
	}

	if err := client.Delete(ctx, processor); err != nil {
		t.Fatalf("delete processor: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "processor-node", Namespace: "metacall-functions"}}); err != nil {
		t.Fatalf("reconcile deleted processor failed: %v", err)
	}

	assertEndpointConfigMap(t, ctx, client, "auth-py-endpoints", "")
	if err := client.Get(ctx, types.NamespacedName{Name: "auth-py", Namespace: "metacall-functions"}, &authDeployment); err != nil {
		t.Fatalf("get auth deployment after peer delete: %v", err)
	}
	hashWithoutPeer := authDeployment.Spec.Template.Annotations[AnnotationEndpointsHash]
	if hashWithoutPeer == "" || hashWithoutPeer == hashWithPeer {
		t.Fatalf("endpoint hash did not change after peer delete: before=%q after=%q", hashWithPeer, hashWithoutPeer)
	}
}

func TestUpdateStatusReadsRuntimeInspect(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	fn := testFunction("auth-py", "py", "app")
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "auth-py", Namespace: "metacall-functions"},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&meshv1.Function{}).
		WithObjects(fn, deployment).
		Build()
	reconciler := &FunctionReconciler{
		Client: client,
		Scheme: scheme,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected runtime method: %s", req.Method)
			}
			var body string
			switch req.URL.Path {
			case "/status":
				body = `{"ready":true,"live":true,"total":0,"loaded":[],"pending":[],"failed":[]}`
			case "/inspect":
				body = `{
					"py": [{
						"scope": {
							"funcs": [
								{"name": "signin"},
								{"name": "signup"}
							]
						}
					}]
				}`
			default:
				t.Fatalf("unexpected runtime URL: %s", req.URL.String())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		})},
	}

	if needsRequeue, err := reconciler.updateStatus(ctx, fn); err != nil {
		t.Fatalf("update status failed: %v", err)
	} else if needsRequeue {
		t.Fatalf("running function should not need requeue")
	}

	var got meshv1.Function
	if err := client.Get(ctx, types.NamespacedName{Name: "auth-py", Namespace: "metacall-functions"}, &got); err != nil {
		t.Fatalf("get function: %v", err)
	}
	if got.Status.Phase != "Live" {
		t.Fatalf("unexpected phase: %s", got.Status.Phase)
	}
	if got.Status.Remote != "0/0" {
		t.Fatalf("unexpected remote status: %s", got.Status.Remote)
	}
	if got.Status.PodCount != 1 {
		t.Fatalf("unexpected pod count: %d", got.Status.PodCount)
	}
	if got.Status.ServiceURL != "http://auth-py.metacall-functions.svc.cluster.local:8080/" {
		t.Fatalf("unexpected service URL: %s", got.Status.ServiceURL)
	}
	if len(got.Status.Functions) != 2 || got.Status.Functions[0] != "signin" || got.Status.Functions[1] != "signup" {
		t.Fatalf("unexpected functions: %v", got.Status.Functions)
	}
}

func TestUpdateStatusTracksRemoteDiscovery(t *testing.T) {
	tests := []struct {
		name        string
		statusBody  string
		wantPhase   string
		wantRemote  string
		wantFailed  int
		wantRequeue bool
	}{
		{
			name:        "initializing",
			statusBody:  `{"live":false,"total":2,"loaded":["http://peer-a/"],"pending":["http://peer-b/"],"failed":[]}`,
			wantPhase:   "Initializing",
			wantRemote:  "1/2",
			wantRequeue: true,
		},
		{
			name:        "degraded",
			statusBody:  `{"live":false,"total":2,"loaded":["http://peer-a/"],"pending":[],"failed":["http://peer-b/"]}`,
			wantPhase:   "Degraded",
			wantRemote:  "1/2",
			wantFailed:  1,
			wantRequeue: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := testScheme(t)
			fn := testFunction("worker-py", "py", "app")
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: fn.Name, Namespace: fn.Namespace},
				Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
			}
			client := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&meshv1.Function{}).
				WithObjects(fn, deployment).
				Build()
			reconciler := &FunctionReconciler{
				Client: client,
				Scheme: scheme,
				HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					body := tt.statusBody
					if req.URL.Path == "/inspect" {
						body = `{}`
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Body:       io.NopCloser(strings.NewReader(body)),
						Header:     make(http.Header),
					}, nil
				})},
			}

			needsRequeue, err := reconciler.updateStatus(ctx, fn)
			if err != nil {
				t.Fatalf("update status failed: %v", err)
			}
			if needsRequeue != tt.wantRequeue {
				t.Fatalf("unexpected requeue: got %v want %v", needsRequeue, tt.wantRequeue)
			}

			var got meshv1.Function
			if err := client.Get(ctx, types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}, &got); err != nil {
				t.Fatalf("get function: %v", err)
			}
			if got.Status.Phase != tt.wantPhase || got.Status.Remote != tt.wantRemote {
				t.Fatalf("unexpected status: %#v", got.Status)
			}
			if len(got.Status.RemoteFailed) != tt.wantFailed {
				t.Fatalf("unexpected remote failures: %v", got.Status.RemoteFailed)
			}
		})
	}
}

func TestRuntimeResourcesDefaultsMissingValuesAndPreservesExplicitValues(t *testing.T) {
	defaults := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
	explicit := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}

	got := runtimeResources(explicit, defaults)
	if got.Requests.Cpu().String() != "250m" {
		t.Fatalf("explicit CPU request was overwritten: %s", got.Requests.Cpu().String())
	}
	if got.Requests.Memory().String() != "128Mi" {
		t.Fatalf("missing memory request was not defaulted: %s", got.Requests.Memory().String())
	}
	if got.Limits.Cpu().String() != "500m" {
		t.Fatalf("missing CPU limit was not defaulted: %s", got.Limits.Cpu().String())
	}
	if got.Limits.Memory().String() != "1Gi" {
		t.Fatalf("explicit memory limit was overwritten: %s", got.Limits.Memory().String())
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := meshv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add mesh scheme: %v", err)
	}
	return scheme
}

func testFunction(name, language, deployGroup string) *meshv1.Function {
	return &meshv1.Function{
		TypeMeta: metav1.TypeMeta{
			APIVersion: meshv1.GroupVersion.String(),
			Kind:       "Function",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "metacall-functions",
		},
		Spec: meshv1.FunctionSpec{
			Language:    language,
			DeployGroup: deployGroup,
			Source: meshv1.SourceSpec{
				Type:       "configmap",
				ConfigMap:  name + "-code",
				Entrypoint: "metacall-" + language + ".json",
			},
		},
	}
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, item := range env {
		if item.Name == name {
			return item.Value
		}
	}
	return ""
}

func assertVolumeMount(t *testing.T, mounts []corev1.VolumeMount, name, path string) {
	t.Helper()
	for _, mount := range mounts {
		if mount.Name == name {
			if mount.MountPath != path || !mount.ReadOnly {
				t.Fatalf("unexpected mount %s: %#v", name, mount)
			}
			return
		}
	}
	t.Fatalf("missing mount %s", name)
}

func assertWritableVolumeMount(t *testing.T, mounts []corev1.VolumeMount, name, path string) {
	t.Helper()
	for _, mount := range mounts {
		if mount.Name == name {
			if mount.MountPath != path || mount.ReadOnly {
				t.Fatalf("unexpected writable mount %s: %#v", name, mount)
			}
			return
		}
	}
	t.Fatalf("missing mount %s", name)
}

func assertConfigMapVolume(t *testing.T, volumes []corev1.Volume, name, configMap string) {
	t.Helper()
	for _, volume := range volumes {
		if volume.Name == name {
			if volume.ConfigMap == nil || volume.ConfigMap.Name != configMap {
				t.Fatalf("unexpected volume %s: %#v", name, volume)
			}
			return
		}
	}
	t.Fatalf("missing volume %s", name)
}

func assertEmptyDirVolume(t *testing.T, volumes []corev1.Volume, name string) {
	t.Helper()
	for _, volume := range volumes {
		if volume.Name == name {
			if volume.EmptyDir == nil {
				t.Fatalf("unexpected volume %s: %#v", name, volume)
			}
			return
		}
	}
	t.Fatalf("missing volume %s", name)
}

func assertHTTPProbe(t *testing.T, probe *corev1.Probe, path, port string) {
	t.Helper()
	if probe == nil || probe.HTTPGet == nil {
		t.Fatalf("missing HTTP probe")
	}
	if probe.HTTPGet.Path != path || probe.HTTPGet.Port.String() != port {
		t.Fatalf("unexpected probe: %#v", probe.HTTPGet)
	}
}

func assertEndpointConfigMap(t *testing.T, ctx context.Context, client crclient.Client, name, endpoints string) {
	t.Helper()
	var cm corev1.ConfigMap
	if err := client.Get(ctx, types.NamespacedName{Name: name, Namespace: "metacall-functions"}, &cm); err != nil {
		t.Fatalf("get endpoint configmap %s: %v", name, err)
	}

	urls := []string{}
	if endpoints != "" {
		urls = strings.Split(endpoints, "\n")
	}
	data := map[string]interface{}{
		"urls": urls,
	}
	b, _ := json.Marshal(data)
	expectedJSON := string(b)

	if cm.Data["endpoints.json"] != expectedJSON {
		t.Fatalf("unexpected endpoints in %s: got %q want %q", name, cm.Data["endpoints.json"], expectedJSON)
	}
	if cm.Data["metacall-rpc.json"] != `{"language_id":"rpc","path":"/mesh","scripts":["endpoints.json"]}` {
		t.Fatalf("unexpected rpc config in %s: %s", name, cm.Data["metacall-rpc.json"])
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
