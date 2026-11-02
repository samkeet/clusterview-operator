# clusterview-operator

A Kubernetes operator (Go, controller-runtime, kubebuilder) that watches **Nodes**,
**Pods** and pod **Events**, keeps an aggregated summary in a `ClusterView` custom
resource, and serves a read-only JSON API and an embedded web UI showing the
cluster state.

- **Nodes page:** a card per node with Ready / NotReady / Unknown status, a
  cordon marker, and CPU, memory and pods as allocatable / capacity. Click a node
  for its pods (namespace, phase, restarts).
- **Pod drawer:** click a pod for its header (namespace, phase, restarts, node) and
  Events / Logs / Details tabs. Events shows the 5 latest, newest first, with page
  numbers and Warnings highlighted. Logs is a placeholder for now.
- **Node pools page** (`#/pools`): a capacity map per node pool. Each pod's CPU
  and memory requests are drawn as a share of the node's allocatable, coloured
  by namespace, with a legend filter and a `kubectl describe node`-style table
  per node. The numbers match describe exactly, including init containers and
  sidecars.

The UI is reached with `kubectl port-forward` only. Nothing is exposed to the
internet.

## Quick start

```sh
cp .env.example .env     # first time: fill in KUBECONFIG and credentials
make ui                  # operator already running on the cluster: just open the UI
make up                  # fresh cluster: deploy operator, sample ClusterView, 20 demo pods, then open the UI
```

`make up` **never builds**. It deploys the pinned image in `hack/IMAGE`, which is
already in GHCR, so you don't need podman or registry push access.

`make up` and `make ui` load `.env` themselves (a `KUBECONFIG` you've already
exported takes precedence), so there's no separate `source .env` step. `make up`
is safe to re-run. It ends by running `make ui`, so it **keeps running until
Ctrl-C**. Use `NO_UI=1` to deploy and return. Run `make ui` to reopen the UI
later. [INSTRUCTIONS.md](INSTRUCTIONS.md) has the full walkthrough,
prerequisites, what you should see, and troubleshooting.

| Command | What it does |
|---|---|
| `make up` | Ensure the pull secret, deploy the pinned image from `hack/IMAGE` (no build), apply the sample `ClusterView` and the demo workload, then run `make ui`. A no-op on a cluster that's already up |
| `make ui` | Check you can port-forward and the operator is ready, port-forward the UI (8080, or a random free port if that's taken), open the browser, and reconnect if the forward drops (e.g. after a redeploy). Ctrl-C stops it |
| `make demo` / `make demo-down` | Apply or delete only the 20-pod demo workload (`config/demo/`) |
| `make down` | Remove the demo, the sample and the operator. This also deletes the `ghcr-pull` secret; set `GHCR_PULL_TOKEN` in `.env` so the next `make up` can recreate it |
| `make release` | Maintainers: build and push the image for HEAD, then pin it in `hack/IMAGE` (see [Releasing](#releasing)) |
| `make test` / `make lint` | Unit + envtest suites / golangci-lint |

Options:

| Option | Applies to | Effect |
|---|---|---|
| `IMG=<image>` | `make up` | Deploy another image that's already in GHCR instead of the pinned one |
| `NO_UI=1` | `make up` | Deploy only. Don't start the port-forward or open the browser |
| `PORT=<port>` | `make ui` | Prefer another local port (default 8080). It still falls back to a random free port if that one's taken |
| `NO_BROWSER=1` | `make ui` | Print the URL instead of opening a browser |

`make help` lists every target.

## The `ClusterView` resource

Cluster-scoped, `observe.samkeet.org/v1alpha1`:

```yaml
apiVersion: observe.samkeet.org/v1alpha1
kind: ClusterView
metadata:
  name: cluster              # the API reads the namespace filter from this name
spec:
  namespaces: []             # count pods in these namespaces only; empty = all
  refreshInterval: 10s       # minimum time between status writes (5s–10m)
```

```console
$ kubectl get clusterview
NAME      NODES   READY   PODS   AGE
cluster   3       3       39     2h
```

The status holds counts only: nodes total / ready / not ready, pods per phase,
and per-node readiness and pod counts, plus a `Ready` condition. Per-pod detail
comes from the API, which keeps the CR small however big the cluster is.

## HTTP API

Read-only: `GET` only, and every other method gets `405`. Responses carry an
`ETag`, so an unchanged poll costs a `304`.

| Endpoint | Returns |
|---|---|
| `GET /api/v1/summary` | Cluster counts: nodes ready / not ready, pods by phase |
| `GET /api/v1/nodes` | `{summary, nodes}`: per node, status, conditions, capacity vs allocatable, pod count |
| `GET /api/v1/nodes/{name}/pods` | Pods on a node: namespace, name, phase, restarts |
| `GET /api/v1/namespaces/{ns}/pods/{name}/events?limit=5&offset=0` | `{total, limit, offset, events}`, newest first |
| `GET /api/v1/pools` | `{filtered, pools}`: per pool (`doks.digitalocean.com/node-pool`, else `(unpooled)`), nodes with allocatable, request/limit totals, and every non-terminated pod's requests, limits and QoS |
| `GET /healthz` | Liveness |

The `ClusterView`'s namespace filter applies to every pod-level response.

## How it works

```
             watch (informers)                      read (cache only)
API server ─────────────────────▶  shared cache  ◀──────────────────── HTTP API + UI
  Nodes, Pods, pod Events          (indexed,                           (:8080, GET only,
                                    trimmed)                            ETag / 304)
                                       │
                                       ▼
                           ClusterView reconciler ── throttled status patch ──▶ ClusterView CR
```

- **No polling of the API server.** Informers stream changes, and the reconciler
  and the API read only from the shared cache. Pods are indexed by
  `spec.nodeName` and Events by the pod they're about, so a request touches only
  the objects it needs. Cache transforms drop fields nothing reads.
- **Status writes are throttled and skipped when nothing changed.**
  `refreshInterval` caps how often status is written, not how often state is
  read. The reconcile is level-triggered, so a deferred write never loses an
  event. Spec edits skip the throttle.
- **Least privilege.** The operator can list/watch nodes, pods and
  `events.k8s.io` events, read ClusterViews and patch their status, and nothing
  else (no `get` on pods, no pod logs, no secrets). The leader-election Role is
  narrowed to the verbs the Lease lock uses.
- **Locked-down pod.**
  - Runs as non-root with a read-only filesystem, on amd64 nodes only.
  - A NetworkPolicy allows ingress to the health port only (port-forward isn't
    affected) and egress to TCP 443 only.
  - The UI is served with a strict CSP and never builds HTML from API data.

Every decision, with the options considered and why each was chosen, is in
[docs/DECISIONS.md](docs/DECISIONS.md). UI mockups are in
[docs/design/](docs/design/).

## Repository layout

| Path | Contents |
|---|---|
| `api/v1alpha1/` | `ClusterView` types and CRD markers |
| `internal/controller/` | Reconciler, write throttle, event predicates |
| `internal/clusterstate/` | Pure aggregation shared by the controller and the API |
| `internal/api/` | HTTP server, handlers, cache index and transforms, embedded UI (`web/`) |
| `cmd/main.go` | Manager wiring: cache options, indexes, controller, API server |
| `config/` | Kustomize: CRD, RBAC, manager, UI Service, NetworkPolicy, samples, `demo/` workload |
| `hack/` | `up.sh` (`make up`), `ui.sh` (`make ui`), `env.sh` (loads `.env`) |

## Development

```sh
make test        # unit tests + envtest (controller), runs make manifests first
make lint        # golangci-lint (v2.15, custom build with logcheck)
make manifests   # regenerate CRD and RBAC from kubebuilder markers
```

Images are built with podman for `linux/amd64` and tagged with the git short SHA.
Base images are pinned by multi-platform digest.

### Releasing

`make up` deploys whatever `hack/IMAGE` names, so publishing a new image is a
deliberate step:

```sh
make release     # needs a clean tree, podman and GHCR push: builds + pushes HEAD, writes hack/IMAGE
make up          # deploy it, then check it on the cluster
git add hack/IMAGE && git commit -m "build: pin image <sha>"   # in the release PR
``` Changes go through pull
requests, and are deployed to the cluster and checked before merging. See
[CLAUDE.md](CLAUDE.md) for the commit and quality rules.
