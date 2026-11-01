package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
)

func TestMapAll(t *testing.T) {
	s := runtime.NewScheme()
	if err := observev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	cv := func(name string) client.Object {
		return &observev1alpha1.ClusterView{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	failList := interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return errors.New("cache not synced")
	}}

	tests := []struct {
		name  string
		views []client.Object
		funcs *interceptor.Funcs
		want  []string
	}{
		{name: "no ClusterViews", want: nil},
		{name: "one", views: []client.Object{cv("cluster")}, want: []string{"cluster"}},
		{name: "many", views: []client.Object{cv("a"), cv("b")}, want: []string{"a", "b"}},
		{name: "list error enqueues nothing", views: []client.Object{cv("a")}, funcs: &failList, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(s).WithObjects(tt.views...)
			if tt.funcs != nil {
				b = b.WithInterceptorFuncs(*tt.funcs)
			}
			r := &ClusterViewReconciler{Client: b.Build()}

			got := r.mapAll(context.Background(), &corev1.Pod{})
			if len(got) != len(tt.want) {
				t.Fatalf("mapAll() returned %d requests, want %d: %v", len(got), len(tt.want), got)
			}
			for i, name := range tt.want {
				if got[i] != (reconcile.Request{NamespacedName: client.ObjectKey{Name: name}}) {
					t.Errorf("request %d = %v, want %s", i, got[i], name)
				}
			}
		})
	}
}
