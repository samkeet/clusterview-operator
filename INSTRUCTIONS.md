# Reaching the ClusterView UI

The UI is not exposed publicly. You reach it with `kubectl port-forward`, using
your own cluster credentials. The `make` targets below do that for you.

## Quick start

```sh
cp .env.example .env     # first time only, then fill in the placeholders
make ui                  # the operator is already running on the shared cluster: just open the UI
```

On a fresh cluster, or if `kubectl -n clusterview-operator-system get pods`
shows nothing, run `make up` first. It deploys everything and then opens the UI.
**Nothing is built on your machine.** The operator image is already in GHCR,
so you don't need podman or push access.

There's no need to `source .env` first: `make up` and `make ui` load it
themselves when `KUBECONFIG` isn't already set. An exported `KUBECONFIG` wins,
so you can still point them at another cluster.

`.env` holds `KUBECONFIG` (the cluster's kubeconfig under `.kube/`) and the
Claude Code settings. `.env.example` lists every key with placeholder values.
`.env` is gitignored, so never commit it or paste its contents anywhere.

## What `make up` does

One command brings up everything on the cluster `KUBECONFIG` points at. It's
safe to re-run: on a cluster that's already up, it changes nothing.

1. Ensures the `ghcr-pull` image pull secret.
2. Deploys the operator (CRD, RBAC, NetworkPolicy, Deployment, UI Service)
   using the **pinned image** in `hack/IMAGE`
   (`ghcr.io/samkeet/clusterview-operator:<sha>`). The cluster pulls it from
   GHCR, and nothing is built.
3. Applies the sample `ClusterView` and a 20-pod demo workload.
4. Opens the UI, the same as `make ui`.

```sh
make up NO_UI=1                        # deploy only, don't open the UI
make up IMG=<image>                    # deploy another image that's already in GHCR
```

`make up` reuses the `ghcr-pull` secret in `clusterview-operator-system` if it
exists, creates it from `GHCR_PULL_TOKEN` (a `read:packages`-only PAT, see
`.env.example`) if that's set, and otherwise stops and prints the command to
create it. The GHCR package is private, so a fresh cluster always needs that
secret.

Maintainers publish a new image with `make release`. It builds and pushes the
image for HEAD (podman, linux/amd64, push access to GHCR) and writes its tag
to `hack/IMAGE`. Deploy it with `make up`, check it on the cluster, then commit
`hack/IMAGE` in the release PR.

## What `make ui` does

1. Checks your kubectl context, that you may port-forward in
   `clusterview-operator-system`, and that the operator is ready.
2. Port-forwards `svc/clusterview-operator-ui` to 8080, or a random free port
   if 8080 is taken, and opens it in your browser. The URL is printed too.
3. Keeps running and reconnects if the forward drops (e.g. after a redeploy).
   **Ctrl-C** stops it.

```sh
make ui PORT=9090        # prefer another local port
make ui NO_BROWSER=1     # print the URL only
```

Your user needs `get services`, `get pods` and `create pods/portforward` in
`clusterview-operator-system`; `make ui` tells you if that's missing.

Other targets:

| Command | What it does |
|---|---|
| `make demo` / `make demo-down` | Apply or delete only the demo workload |
| `make down` | Remove the demo pods, the sample `ClusterView` and the operator. This deletes the operator namespace, **including the `ghcr-pull` secret**, so set `GHCR_PULL_TOKEN` before the next `make up` |

The demo workload (`config/demo/`) stays running for demos. It puts 20 pods in
`demo-shop`, `demo-payments` and `demo-batch`:

- Burstable, Guaranteed and BestEffort pods, plus a native sidecar
  (`catalog`), so the capacity numbers cover the interesting cases.
- `crashloop` restarts every few seconds and `not-ready` fails its readiness
  probe, so both produce **Warning** events in the pod drawer.

Together they request about 1.2 CPU cores and 1.5Gi of memory.

## Without make

`make ui` wraps a plain port-forward. To run it by hand:

```sh
source .env
kubectl -n clusterview-operator-system port-forward svc/clusterview-operator-ui 8080:80
```

Then open <http://localhost:8080>. The Service is named
`clusterview-operator-ui`, not `ui`: kustomize adds the `clusterview-operator-`
prefix to every resource. If 8080 is taken (often by a port-forward you left
running: `lsof -iTCP:8080 -sTCP:LISTEN`), use another local port such as
`9090:80`.

## What you should see

- A **ClusterView** header with a summary line: node count, ready / not-ready
  nodes, total pods and pod phases. Below it, "Updated Ns ago" refreshes on
  its own.
- One card per node: its name, a status badge (Ready / NotReady / Unknown, plus
  "Cordoned" if unschedulable), and CPU, memory and pod count as
  allocatable / capacity. On this cluster that's 3 nodes, all Ready. Unknown
  means the control plane has lost contact with the node's kubelet.
- Click a node to open **Pods on &lt;node&gt;**: a table of namespace, name,
  phase and restart count. **Close** hides it.
- Click a pod name to open the **pod drawer** on the right: the pod's
  namespace, phase, restarts and node, with **Events / Logs / Details** tabs.
  Events shows the 5 latest, newest first, with page numbers. Warnings are
  marked, and clicking a long message expands it. Kubernetes keeps events for
  about an hour. **Esc** or **×** closes the drawer. Logs is a placeholder for
  now. Try `demo-batch/crashloop-…` to see Warning events.
- **Node pools** (header nav, or `#/pools`): one section per node pool
  (`s-4vcpu-8gb · 3 nodes · 39 pods · CPU req % · Mem req %`). Each node row
  has two stacked bars, CPU and memory requests, where **each segment is one
  pod's request as a share of the node's allocatable**, coloured by namespace
  (`kube-system` grey). Hover a segment for the pod and its share. Click a
  namespace in the legend to hide or show it. Click a node for a
  `kubectl describe node`-style table: QoS, CPU and memory requests and
  limits, each with % of allocatable. Percentages are truncated, like describe.
  Pods with no requests are listed as "+N with no requests". An unset limit
  shows `0`.

If the port-forward drops, the page keeps showing the last data, dimmed, with
a "Can't reach API" banner until `make ui` reconnects. If it stays on
"Loading…", check the operator pod with
`kubectl -n clusterview-operator-system get pods`.

The same summary is on the `ClusterView` resource itself:

```sh
kubectl get clusterview
```

The JSON API is available on the same port, for example:

```sh
curl -s localhost:8080/api/v1/summary
curl -s localhost:8080/api/v1/nodes
curl -s localhost:8080/api/v1/nodes/<node-name>/pods
curl -s 'localhost:8080/api/v1/namespaces/<ns>/pods/<pod>/events?limit=5&offset=0'
curl -s localhost:8080/api/v1/pools
```

The API is read-only: it answers `GET` only, and every other method gets
`405 Method Not Allowed`.

## Why port-forward only

The API has no authentication of its own. Keeping the Service as `ClusterIP`
and reaching it through `kubectl port-forward` means access is controlled by
cluster credentials: only someone whose kubeconfig and RBAC allow
port-forwarding into `clusterview-operator-system` can see the UI. Nothing is
exposed to the internet, so there's no LoadBalancer, Ingress or tunnel.

## Note on the NetworkPolicy

The `clusterview-operator-manager-traffic` NetworkPolicy blocks other pods in the cluster from
reaching the API port (8080) and only allows the health port (8081).
`kubectl port-forward` isn't affected: it goes through the API server and
kubelet and connects to the pod over its loopback interface, which
NetworkPolicy doesn't filter. No policy change is needed for port-forward to
work.
