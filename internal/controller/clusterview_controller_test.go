/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
		Expect(st.PodPhases).To(Equal(map[string]int32{"Running": 2, "Pending": 2}))
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
})
