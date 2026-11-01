package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
)

// fakeCVName is the ClusterView used by the fake-client tests.
const fakeCVName = "main"

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
		{name: "one", views: []client.Object{cv(fakeCVName)}, want: []string{fakeCVName}},
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

// List failures and patch conflicts are hard to provoke in envtest, so these use the
// fake client with interceptors.
func TestReconcileFailurePaths(t *testing.T) {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{observev1alpha1.AddToScheme, corev1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	listErr := errors.New("cache not synced")
	failListOf := func(match func(client.ObjectList) bool) interceptor.Funcs {
		return interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, o ...client.ListOption) error {
			if match(l) {
				return listErr
			}
			return c.List(ctx, l, o...)
		}}
	}
	isNodes := func(l client.ObjectList) bool { _, ok := l.(*corev1.NodeList); return ok }
	isPods := func(l client.ObjectList) bool { _, ok := l.(*corev1.PodList); return ok }
	conflict := interceptor.Funcs{SubResourcePatch: func(context.Context, client.Client, string, client.Object,
		client.Patch, ...client.SubResourcePatchOption) error {
		return apierrors.NewConflict(schema.GroupResource{Resource: "clusterviews"}, fakeCVName, errors.New("stale"))
	}}

	tests := []struct {
		name       string
		funcs      interceptor.Funcs
		wantErr    bool
		wantAfter  time.Duration
		wantReason string // empty: status must be unwritten
	}{
		{name: "node list fails", funcs: failListOf(isNodes), wantErr: true, wantReason: reasonListNodesFailed},
		{name: "pod list fails", funcs: failListOf(isPods), wantErr: true, wantReason: reasonListPodsFailed},
		{name: "patch conflict retries without error", funcs: conflict, wantAfter: conflictRetry},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cv := &observev1alpha1.ClusterView{
				ObjectMeta: metav1.ObjectMeta{Name: fakeCVName, Generation: 1},
				Spec:       observev1alpha1.ClusterViewSpec{RefreshInterval: metav1.Duration{Duration: 10 * time.Second}},
			}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(cv).
				WithStatusSubresource(&observev1alpha1.ClusterView{}).
				WithInterceptorFuncs(tt.funcs).Build()
			r := &ClusterViewReconciler{Client: c, Clock: clocktesting.NewFakeClock(testNow)}

			res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Name: fakeCVName}})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, listErr) {
				t.Errorf("error %v should wrap the list error", err)
			}
			if res.RequeueAfter != tt.wantAfter {
				t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, tt.wantAfter)
			}

			got := &observev1alpha1.ClusterView{}
			if err := c.Get(context.Background(), client.ObjectKey{Name: fakeCVName}, got); err != nil {
				t.Fatal(err)
			}
			ready := apimeta.FindStatusCondition(got.Status.Conditions, conditionReady)
			if tt.wantReason == "" {
				if ready != nil || !r.writes.get(fakeCVName, got.UID).IsZero() {
					t.Errorf("a conflicted patch must not count as a write: %+v", got.Status)
				}
				return
			}
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != tt.wantReason {
				t.Errorf("Ready condition = %+v, want False/%s", ready, tt.wantReason)
			}
		})
	}
}
