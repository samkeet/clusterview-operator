/*
Copyright 2026 Samkeet Shah. All rights reserved. See LICENSE.
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
)

// These specs call Reconcile directly with a fake clock and the uncached k8sClient,
// so every step is deterministic: no cache lag, no workqueue timing.
var _ = Describe("ClusterView reconcile", func() {
	const cvName = "cluster"

	var (
		ns    string
		clk   *clocktesting.FakeClock
		r     *ClusterViewReconciler
		cv    *observev1alpha1.ClusterView
		start = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	)

	reconcileOnce := func() reconcile.Result {
		GinkgoHelper()
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKey{Name: cvName}})
		Expect(err).NotTo(HaveOccurred())
		return res
	}
	current := func() *observev1alpha1.ClusterView {
		GinkgoHelper()
		got := &observev1alpha1.ClusterView{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: cvName}, got)).To(Succeed())
		return got
	}

	BeforeEach(func() {
		ns = createNamespace()
		clk = clocktesting.NewFakeClock(start)
		r = &ClusterViewReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: clk}
		cv = createClusterView(cvName, observev1alpha1.ClusterViewSpec{
			Namespaces:      []string{ns},
			RefreshInterval: metav1.Duration{Duration: 10 * time.Second},
		})
	})

	AfterEach(func() {
		deleteClusterView(cv)
		deleteAllNodes()
	})

	It("records counts for the nodes and pods in the cache", func() {
		createNode("node-a", corev1.ConditionTrue)
		createNode("node-b", corev1.ConditionFalse)
		createNode("node-c", corev1.ConditionUnknown)
		createPod(ns, "p1", "node-a", corev1.PodRunning)
		createPod(ns, "p2", "node-a", corev1.PodRunning)
		createPod(ns, "p3", "node-b", corev1.PodPending)
		createPod(ns, "p4", "", corev1.PodPending)
		// Outside the namespace filter, so it must not be counted.
		createPod(createNamespace(), "other", "node-a", corev1.PodRunning)

		res := reconcileOnce()
		Expect(res.RequeueAfter).To(Equal(backstopInterval))

		st := current().Status
		Expect(st.ObservedGeneration).To(Equal(cv.Generation))
		Expect(st.NodeCount).To(BeEquivalentTo(3))
		Expect(st.ReadyNodes).To(BeEquivalentTo(1))
		Expect(st.NotReadyNodes).To(BeEquivalentTo(2))
		Expect(st.PodCount).To(BeEquivalentTo(4))
		Expect(st.PodPhases).To(Equal(map[string]int32{string(corev1.PodRunning): 2, string(corev1.PodPending): 2}))
		Expect(st.Nodes).To(Equal([]observev1alpha1.NodeSummary{
			{Name: "node-a", Status: observev1alpha1.NodeReady, PodCount: 2},
			{Name: "node-b", Status: observev1alpha1.NodeNotReady, PodCount: 1},
			{Name: "node-c", Status: observev1alpha1.NodeUnknown, PodCount: 0},
		}))
		Expect(st.LastUpdated.Time).To(BeTemporally("==", start))

		ready := apimeta.FindStatusCondition(st.Conditions, conditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal(reasonReconciled))
	})

	It("leaves the object untouched when nothing changed", func() {
		createNode("node-a", corev1.ConditionTrue)
		reconcileOnce()
		before := current()

		// Past the window, so only the content comparison can stop the write.
		clk.Step(time.Minute)
		res := reconcileOnce()

		after := current()
		Expect(res.RequeueAfter).To(Equal(backstopInterval))
		Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
		Expect(after.Status.LastUpdated.Time).To(BeTemporally("==", start))
	})

	It("defers a change inside the window, then writes it", func() {
		reconcileOnce()
		rv := current().ResourceVersion

		createPod(ns, "p1", "", corev1.PodRunning)
		clk.Step(2 * time.Second)
		res := reconcileOnce()
		Expect(res.RequeueAfter).To(Equal(8 * time.Second))
		Expect(current().ResourceVersion).To(Equal(rv))
		Expect(current().Status.PodCount).To(BeZero())

		// The requeue the workqueue would have delivered.
		clk.Step(res.RequeueAfter)
		res = reconcileOnce()
		Expect(res.RequeueAfter).To(Equal(backstopInterval))
		st := current().Status
		Expect(st.PodCount).To(BeEquivalentTo(1))
		Expect(st.LastUpdated.Time).To(BeTemporally("==", start.Add(10*time.Second)))
	})

	It("writes a spec change without waiting for the window", func() {
		reconcileOnce()
		other := createNamespace()
		createPod(other, "p1", "", corev1.PodRunning)

		latest := current()
		latest.Spec.Namespaces = []string{ns, other}
		Expect(k8sClient.Update(ctx, latest)).To(Succeed())

		clk.Step(time.Second)
		reconcileOnce()
		st := current().Status
		Expect(st.ObservedGeneration).To(Equal(latest.Generation))
		Expect(st.PodCount).To(BeEquivalentTo(1))
	})

	It("forgets throttle state when the ClusterView is deleted", func() {
		reconcileOnce()
		Expect(r.writes.get(cvName, cv.UID)).NotTo(BeZero())

		deleteClusterView(cv)
		Expect(reconcileOnce()).To(Equal(reconcile.Result{}))
		Expect(r.writes.get(cvName, cv.UID)).To(BeZero())
	})
})

// One real manager with the real clock checks the wiring: watches, predicates,
// fan-out and the workqueue's trailing write. It runs alone, because it would
// otherwise also reconcile the ClusterViews of the direct specs above.
var _ = Describe("ClusterView controller with a running manager", Ordered, func() {
	const (
		cvName  = "wired"
		timeout = 20 * time.Second
	)

	var (
		ns      string
		cv      *observev1alpha1.ClusterView
		stopMgr context.CancelFunc
		stopped chan struct{}
	)

	status := func(g Gomega) observev1alpha1.ClusterViewStatus {
		got := &observev1alpha1.ClusterView{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: cvName}, got)).To(Succeed())
		return got.Status
	}

	BeforeAll(func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:                 scheme.Scheme,
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect((&ClusterViewReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).
			SetupWithManager(mgr)).To(Succeed())

		var mgrCtx context.Context
		mgrCtx, stopMgr = context.WithCancel(ctx)
		stopped = make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(stopped)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()

		ns = createNamespace()
		cv = createClusterView(cvName, observev1alpha1.ClusterViewSpec{
			Namespaces:      []string{ns},
			RefreshInterval: metav1.Duration{Duration: 5 * time.Second},
		})
	})

	AfterAll(func() {
		deleteClusterView(cv)
		deleteAllNodes()
		stopMgr()
		Eventually(stopped).Should(BeClosed())
	})

	It("writes status when the ClusterView is created", func() {
		Eventually(func(g Gomega) {
			st := status(g)
			ready := apimeta.FindStatusCondition(st.Conditions, conditionReady)
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(st.NodeCount).To(BeZero())
		}).WithTimeout(timeout).Should(Succeed())
	})

	It("follows node and pod events through the throttle", func() {
		n := createNode("wired-a", corev1.ConditionTrue)
		Eventually(func(g Gomega) {
			g.Expect(status(g).ReadyNodes).To(BeEquivalentTo(1))
		}).WithTimeout(timeout).Should(Succeed())

		setNodeReady(n, corev1.ConditionFalse)
		Eventually(func(g Gomega) {
			st := status(g)
			g.Expect(st.ReadyNodes).To(BeZero())
			g.Expect(st.NotReadyNodes).To(BeEquivalentTo(1))
		}).WithTimeout(timeout).Should(Succeed())

		createPod(ns, "p1", "wired-a", corev1.PodRunning)
		Eventually(func(g Gomega) {
			st := status(g)
			g.Expect(st.PodCount).To(BeEquivalentTo(1))
			g.Expect(st.Nodes).To(ConsistOf(observev1alpha1.NodeSummary{
				Name: "wired-a", Status: observev1alpha1.NodeNotReady, PodCount: 1,
			}))
		}).WithTimeout(timeout).Should(Succeed())
	})
})
