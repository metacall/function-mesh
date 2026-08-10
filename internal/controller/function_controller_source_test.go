package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/metacall/function-mesh/internal/sourcebundle"
)

func TestReconcileMountsIndexedSourceAtNestedPaths(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	fn := testFunction("nested-py", "py", "app")
	fn.Spec.Source.Entrypoint = "service/metacall.json"
	data, err := sourcebundle.Encode(map[string]string{
		"service/metacall.json": `{"language_id":"py","path":".","scripts":["main.py"]}`,
		"service/main.py":       "def main():\n    return 1\n",
	})
	if err != nil {
		t.Fatalf("encode source bundle: %v", err)
	}
	source := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: fn.Spec.Source.ConfigMap, Namespace: fn.Namespace,
	}, Data: data}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, source).Build()
	reconciler := &FunctionReconciler{Client: kube, Scheme: scheme}

	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: fn.Name, Namespace: fn.Namespace}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var deployment appsv1.Deployment
	if err := kube.Get(ctx, request.NamespacedName, &deployment); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got := deployment.Spec.Template.Annotations[AnnotationSourceHash]; got == "" {
		t.Fatalf("missing source hash annotation")
	}
	var sourceVolume *corev1.ConfigMapVolumeSource
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == "source-code" {
			sourceVolume = volume.ConfigMap
			break
		}
	}
	if sourceVolume == nil || len(sourceVolume.Items) != 2 {
		t.Fatalf("unexpected source volume: %#v", sourceVolume)
	}
	if sourceVolume.Items[0].Path != "service/main.py" || sourceVolume.Items[1].Path != "service/metacall.json" {
		t.Fatalf("nested source paths were not mounted: %#v", sourceVolume.Items)
	}

	before := deployment.Spec.Template.Annotations[AnnotationSourceHash]
	source.Data[sourcebundle.IndexKey] += " "
	if err := kube.Update(ctx, source); err != nil {
		t.Fatalf("update source ConfigMap: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile updated source failed: %v", err)
	}
	if err := kube.Get(ctx, request.NamespacedName, &deployment); err != nil {
		t.Fatalf("get updated deployment: %v", err)
	}
	if after := deployment.Spec.Template.Annotations[AnnotationSourceHash]; after == "" || after == before {
		t.Fatalf("source hash did not change: before=%q after=%q", before, after)
	}
}
