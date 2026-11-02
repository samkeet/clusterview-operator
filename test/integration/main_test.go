//go:build integration

// Package integration runs the real operator binary against a kwok cluster and checks
// the ClusterView status and the HTTP API against clusterstate.Build, computed
// independently from a direct (uncached) list of Nodes and Pods.
//
// The operator runs as its own ServiceAccount (via impersonation) on a cluster with
// RBAC enforced, so a missing or excess permission in config/rbac fails the suite.
//
// Requires kwokctl and kubectl on PATH. Run with `make test-integration`.
// CV_IT_KEEP=1 keeps the kwok cluster afterwards and reuses it on the next run.
package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
)

const (
	clusterName = "cv-it"
	operatorNS  = "clusterview-operator-system"
	operatorSA  = "clusterview-operator-controller-manager"
	viewName    = "cluster"
	fakeNodes   = 3
	// refresh is the CRD's minimum interval; it keeps the throttle tests short.
	refresh  = 5 * time.Second
	nsPrefix = "cv-it-"
	// manualLabel marks nodes the tests create and drive by hand.
	manualLabel = "cv-it/manual"
	fieldOwner  = "cv-integration"
)

// kwokConfig limits kwok to the nodes it creates itself (`kwokctl scale node`), so
// nodes the tests create keep whatever status the test sets. kwokctl always passes
// --manage-all-nodes=true, which conflicts with a selector, hence the override.
const kwokConfig = `apiVersion: config.kwok.x-k8s.io/v1alpha1
kind: KwokctlConfiguration
componentsPatches:
- name: kwok-controller
  extraArgs:
  - key: manage-all-nodes
    value: "false"
  - key: manage-nodes-with-annotation-selector
    value: kwok.x-k8s.io/node=fake
`

// manifestKinds are applied from config/default. The Deployment, Service and
// NetworkPolicy are skipped: the operator runs as a local process instead.
var manifestKinds = []string{
	"Namespace", "CustomResourceDefinition", "ServiceAccount",
	"Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding",
}

// env is shared by every test. Tests run sequentially because the ClusterView and
// its counts are cluster-wide.
var env *harness

type harness struct {
	admin    client.WithWatch // cluster-admin, uncached: the oracle and the tests' own writes
	apiURL   string
	http     *http.Client
	operator *exec.Cmd
	exited   chan struct{}
	logPath  string
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	keep := os.Getenv("CV_IT_KEEP") == "1"
	workDir, err := os.MkdirTemp("", "cv-it-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating work dir: %v\n", err)
		return 1
	}

	h, err := setup(context.Background(), workDir, keep)
	code := 1
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration setup failed: %v\n", err)
	} else {
		env = h
		code = m.Run()
	}

	if h != nil && h.operator != nil {
		h.stopOperator()
		h.reportUsage()
	}
	if !keep {
		if err := kwokctl("delete", "cluster", "--name", clusterName); err != nil {
			fmt.Fprintf(os.Stderr, "deleting kwok cluster: %v\n", err)
		}
	}
	if code == 0 && !keep {
		_ = os.RemoveAll(workDir)
	} else {
		fmt.Fprintf(os.Stderr, "work dir kept (operator log, kubeconfigs): %s\n", workDir)
	}
	return code
}

func setup(ctx context.Context, workDir string, keep bool) (*harness, error) {
	for _, bin := range []string{"kwokctl", "kubectl"} {
		if _, err := exec.LookPath(bin); err != nil {
			return nil, fmt.Errorf("%s not found on PATH (brew install kwok / kubectl): %w", bin, err)
		}
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		return nil, fmt.Errorf("resolving repo root: %w", err)
	}

	kubeconfig := filepath.Join(workDir, "admin.kubeconfig")
	if err := ensureCluster(workDir, kubeconfig, keep); err != nil {
		return nil, err
	}

	admin, err := newAdminClient(kubeconfig)
	if err != nil {
		return nil, err
	}
	h := &harness{admin: admin, http: &http.Client{Timeout: 10 * time.Second}}

	if err := purgeLeftovers(ctx, admin); err != nil {
		return nil, err
	}
	if err := applyManifests(ctx, admin, root); err != nil {
		return nil, err
	}
	if err := waitForCRD(ctx, admin); err != nil {
		return nil, err
	}
	if err := resetView(ctx, admin); err != nil {
		return nil, err
	}
	if err := h.startOperator(ctx, root, workDir, kubeconfig); err != nil {
		return h, err
	}
	return h, nil
}

// ensureCluster creates the kwok cluster, or reuses it when CV_IT_KEEP is set and it
// already exists. A stale cluster from a crashed run is recreated.
func ensureCluster(workDir, kubeconfig string, keep bool) error {
	out, err := exec.Command("kwokctl", "get", "clusters").Output()
	if err != nil {
		return fmt.Errorf("listing kwok clusters: %w", err)
	}
	exists := slices.Contains(strings.Fields(string(out)), clusterName)

	if exists && keep {
		cfg, err := exec.Command("kwokctl", "get", "kubeconfig", "--name", clusterName).Output()
		if err != nil {
			return fmt.Errorf("reading kubeconfig of kept cluster: %w", err)
		}
		if err := os.WriteFile(kubeconfig, cfg, 0o600); err != nil {
			return fmt.Errorf("writing kubeconfig: %w", err)
		}
		fmt.Fprintf(os.Stderr, "reusing kwok cluster %q\n", clusterName)
		return nil
	}
	if exists {
		if err := kwokctl("delete", "cluster", "--name", clusterName); err != nil {
			return fmt.Errorf("deleting stale kwok cluster: %w", err)
		}
	}

	cfgPath := filepath.Join(workDir, "kwok.yaml")
	if err := os.WriteFile(cfgPath, []byte(kwokConfig), 0o600); err != nil {
		return fmt.Errorf("writing kwok config: %w", err)
	}
	// The binary runtime runs etcd and the control plane as local processes, so no
	// container engine is needed. --kube-authorization turns on RBAC.
	if err := kwokctl("create", "cluster", "--name", clusterName, "--runtime", "binary",
		"--kube-authorization=true", "--config", cfgPath, "--kubeconfig", kubeconfig,
		"--wait", "2m"); err != nil {
		return fmt.Errorf("creating kwok cluster: %w", err)
	}
	if err := kwokctl("scale", "node", "--name", clusterName,
		"--replicas", fmt.Sprint(fakeNodes)); err != nil {
		return fmt.Errorf("scaling kwok nodes: %w", err)
	}
	return nil
}

func kwokctl(args ...string) error {
	cmd := exec.Command("kwokctl", args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kwokctl %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return nil
}

func newScheme() (*k8sruntime.Scheme, error) {
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("adding client-go scheme: %w", err)
	}
	if err := observev1alpha1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("adding observe scheme: %w", err)
	}
	return s, nil
}

func newAdminClient(kubeconfig string) (client.WithWatch, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	// The suite creates pods in bursts; the default 5 QPS would make it the bottleneck.
	cfg.QPS, cfg.Burst = 100, 200
	s, err := newScheme()
	if err != nil {
		return nil, err
	}
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: s})
	if err != nil {
		return nil, fmt.Errorf("creating admin client: %w", err)
	}
	return c, nil
}

// purgeLeftovers removes test namespaces and manual nodes from a crashed or kept run.
func purgeLeftovers(ctx context.Context, c client.Client) error {
	var nsList corev1.NamespaceList
	if err := c.List(ctx, &nsList); err != nil {
		return fmt.Errorf("listing namespaces: %w", err)
	}
	for _, ns := range nsList.Items {
		if strings.HasPrefix(ns.Name, nsPrefix) {
			if err := deleteNamespace(ctx, c, ns.Name); err != nil {
				return err
			}
		}
	}
	if err := c.DeleteAllOf(ctx, &corev1.Node{}, client.HasLabels{manualLabel}); err != nil {
		return fmt.Errorf("deleting manual nodes: %w", err)
	}
	return nil
}

// deleteNamespace force-deletes pods first: pods bound to manual nodes have no kubelet
// to confirm a graceful delete, so they would hold the namespace in Terminating.
func deleteNamespace(ctx context.Context, c client.Client, ns string) error {
	err := c.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(ns), client.GracePeriodSeconds(0))
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting pods in %s: %w", ns, err)
	}
	err = c.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting namespace %s: %w", ns, err)
	}
	return nil
}

// applyManifests server-side-applies the RBAC and CRD that `make deploy` would, so the
// test exercises the exact roles we ship.
func applyManifests(ctx context.Context, c client.Client, root string) error {
	cmd := exec.Command("kubectl", "kustomize", filepath.Join(root, "config", "default"))
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("building config/default with kustomize: %w", err)
	}
	dec := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var u unstructured.Unstructured
		if err := dec.Decode(&u.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decoding manifests: %w", err)
		}
		if len(u.Object) == 0 || !slices.Contains(manifestKinds, u.GetKind()) {
			continue
		}
		if err := c.Apply(ctx, client.ApplyConfigurationFromUnstructured(&u),
			client.FieldOwner(fieldOwner), client.ForceOwnership); err != nil {
			return fmt.Errorf("applying %s %s: %w", u.GetKind(), u.GetName(), err)
		}
	}
}

func waitForCRD(ctx context.Context, c client.Client) error {
	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			if err := c.Get(ctx, client.ObjectKey{Name: "clusterviews.observe.samkeet.org"}, crd); err != nil {
				return false, nil
			}
			conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
			for _, cond := range conds {
				m, _ := cond.(map[string]any)
				if m["type"] == "Established" && m["status"] == "True" {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		return fmt.Errorf("waiting for ClusterView CRD to be established: %w", err)
	}
	return nil
}

// resetView creates the ClusterView, or resets its spec when the cluster is reused.
func resetView(ctx context.Context, c client.Client) error {
	var cv observev1alpha1.ClusterView
	err := c.Get(ctx, client.ObjectKey{Name: viewName}, &cv)
	if apierrors.IsNotFound(err) {
		cv = observev1alpha1.ClusterView{
			ObjectMeta: metav1.ObjectMeta{Name: viewName},
			Spec:       observev1alpha1.ClusterViewSpec{RefreshInterval: metav1.Duration{Duration: refresh}},
		}
		if err := c.Create(ctx, &cv); err != nil {
			return fmt.Errorf("creating ClusterView: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting ClusterView: %w", err)
	}
	cv.Spec = observev1alpha1.ClusterViewSpec{RefreshInterval: metav1.Duration{Duration: refresh}}
	if err := c.Update(ctx, &cv); err != nil {
		return fmt.Errorf("resetting ClusterView spec: %w", err)
	}
	return nil
}

// startOperator builds cmd/ and runs it as the operator's ServiceAccount, so the run
// covers the real flag wiring, cache transforms, field index and RBAC.
func (h *harness) startOperator(ctx context.Context, root, workDir, adminKubeconfig string) error {
	bin := filepath.Join(workDir, "manager")
	build := exec.Command("go", "build", "-o", bin, "./cmd")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("building operator: %w\n%s", err, out)
	}

	kubeconfig := filepath.Join(workDir, "operator.kubeconfig")
	if err := writeImpersonatingKubeconfig(adminKubeconfig, kubeconfig); err != nil {
		return err
	}
	apiPort, err := freePort()
	if err != nil {
		return err
	}
	probePort, err := freePort()
	if err != nil {
		return err
	}

	h.logPath = filepath.Join(workDir, "operator.log")
	logFile, err := os.Create(h.logPath)
	if err != nil {
		return fmt.Errorf("creating operator log: %w", err)
	}
	// Leader election stays off: out of cluster there is no namespace to elect in.
	h.operator = exec.Command(bin,
		fmt.Sprintf("--api-bind-address=127.0.0.1:%d", apiPort),
		fmt.Sprintf("--health-probe-bind-address=127.0.0.1:%d", probePort),
		"--metrics-bind-address=0",
		"--leader-elect=false",
		"--clusterview-name="+viewName,
	)
	h.operator.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	h.operator.Stdout, h.operator.Stderr = logFile, logFile
	if err := h.operator.Start(); err != nil {
		return fmt.Errorf("starting operator: %w", err)
	}
	h.exited = make(chan struct{})
	go func() {
		_ = h.operator.Wait()
		_ = logFile.Close()
		close(h.exited)
	}()

	h.apiURL = fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	// The API server starts only after the informer cache has synced, so a healthy
	// /healthz also means the operator sees the cluster.
	err = wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, time.Minute, true,
		func(context.Context) (bool, error) {
			select {
			case <-h.exited:
				return false, fmt.Errorf("operator exited during startup, see %s", h.logPath)
			default:
			}
			resp, err := h.http.Get(h.apiURL + "/healthz")
			if err != nil {
				return false, nil
			}
			_ = resp.Body.Close()
			return resp.StatusCode == http.StatusOK, nil
		})
	if err != nil {
		return fmt.Errorf("waiting for operator API: %w", err)
	}
	return nil
}

// writeImpersonatingKubeconfig copies the admin kubeconfig but impersonates the
// operator's ServiceAccount, with the groups the API server would assign it.
func writeImpersonatingKubeconfig(src, dst string) error {
	cfg, err := clientcmd.LoadFromFile(src)
	if err != nil {
		return fmt.Errorf("loading admin kubeconfig: %w", err)
	}
	kctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return fmt.Errorf("admin kubeconfig has no current context")
	}
	user := cfg.AuthInfos[kctx.AuthInfo]
	user.Impersonate = serviceAccountUser()
	user.ImpersonateGroups = serviceAccountGroups()
	if err := clientcmd.WriteToFile(*cfg, dst); err != nil {
		return fmt.Errorf("writing operator kubeconfig: %w", err)
	}
	return nil
}

func serviceAccountUser() string {
	return "system:serviceaccount:" + operatorNS + ":" + operatorSA
}

func serviceAccountGroups() []string {
	return []string{"system:serviceaccounts", "system:serviceaccounts:" + operatorNS, "system:authenticated"}
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("finding a free port: %w", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (h *harness) stopOperator() {
	select {
	case <-h.exited:
		return
	default:
	}
	_ = h.operator.Process.Signal(syscall.SIGTERM)
	select {
	case <-h.exited:
	case <-time.After(10 * time.Second):
		_ = h.operator.Process.Kill()
		<-h.exited
	}
}

// reportUsage prints the operator's peak memory and total CPU time for the run,
// read from the exited process, so no metrics-server is needed.
func (h *harness) reportUsage() {
	ps := h.operator.ProcessState
	if ps == nil {
		return
	}
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok {
		return
	}
	maxRSS := ru.Maxrss * 1024 // Linux reports KiB
	if runtime.GOOS == "darwin" {
		maxRSS = ru.Maxrss // macOS reports bytes
	}
	fmt.Fprintf(os.Stderr, "operator usage: peak RSS %.1f MiB, CPU user %s sys %s\n",
		float64(maxRSS)/(1<<20), ps.UserTime().Round(time.Millisecond), ps.SystemTime().Round(time.Millisecond))
}
