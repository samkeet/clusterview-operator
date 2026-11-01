# Decisions

One block per decision: the options we considered, then what we picked and why. Newest at the bottom.

## Scaffolding: kubebuilder v4
- Considered: kubebuilder · operator-sdk · hand-rolled controller-runtime
- Chose kubebuilder: it's the upstream controller-runtime scaffold, its RBAC/CRD markers feed controller-gen, and envtest and kustomize come wired in. operator-sdk only adds OLM bundling, which we don't need.

## ClusterView scope: cluster-scoped
- Considered: namespaced · cluster-scoped
- Chose cluster-scoped: the CR describes the whole cluster (nodes aren't namespaced), so tying it to a namespace would be arbitrary.

## Image registry: GHCR (private during development)
- Considered: GHCR · DigitalOcean Container Registry · Docker Hub
- Chose GHCR: it sits next to the repo and auths with the existing gh CLI. DOCR needs doctl plus a DO token, and Docker Hub needs a separate account.

## Image pull auth: read:packages-only PAT in a `ghcr-pull` secret
- Considered: make the package public · gh CLI token as pull secret · dedicated read:packages PAT
- Chose a dedicated PAT: the repo stays private until it's ready, and the gh token has `repo`/`delete_repo` scope, which is far too broad to store in a cluster. The secret is created by hand and never committed.

## Container engine: podman, cross-compiled to linux/amd64
- Considered: install Docker · podman with qemu emulation · podman with a `$BUILDPLATFORM` builder and `GOARCH` cross-compile
- Chose cross-compile: podman is already installed, the nodes are amd64, and a native builder avoids slow emulated Go builds (it works because CGO is off).

## Image tags: git short SHA
- Considered: `:latest` · semver · git short SHA
- Chose the SHA: every deploy rolls a distinct, traceable image, with no stale `:latest` pulls caused by `IfNotPresent`.

## Delivery: one PR per step, merge commits, deploy before merge
- Considered: push to main · squash-merge PRs · merge-commit PRs
- Chose merge commits: CLAUDE.md requires PRs and forbids squashing, so small commits keep their history. Deploying the branch before merging means main is always something that ran on the cluster.

## UI exposure: port-forward first, public LoadBalancer later
- Considered: public LoadBalancer now · ClusterIP + `kubectl port-forward` · Ingress
- Chose port-forward first: the API has no auth, so we test locally before exposing cluster data publicly. The LoadBalancer comes once things have been checked.

## `refreshInterval` semantics: minimum time between status writes
- Considered: poll the API server every N · write on every relevant event · fixed timer only · event-driven reconcile with a write throttle
- Chose event-driven + throttle: informers already stream changes, so reads never hit the API server. Status writes are the only load we control, so they're capped at one per interval (default 10s, 5s–10m) and skipped entirely when nothing changed.

## Missed events: level-triggered reconcile + 5m backstop
- Considered: process each event (edge-triggered) · rebuild from the full cache on every reconcile (level-triggered)
- Chose level-triggered: a throttled reconcile defers work but never drops it. The next run reads the whole current state, the workqueue guarantees a trailing run, and a 5m requeue (cache read only) is the safety net. Tradeoff: status is a sample, so a transition shorter than the window never shows in the CR (the UI reads the live cache and does see it).

## Node readiness: `Ready` condition, three states
- Considered: `ready bool` · `Ready`/`NotReady`/`Unknown` · fold in pressure conditions and cordons
- Chose three states: True → Ready, False → NotReady, Unknown or missing → Unknown. `notReadyNodes` counts NotReady + Unknown. Cordons and pressure conditions show in the API but don't change the count, which matches Kubernetes' own semantics.

## Last-write time: `status.lastUpdated` + in-memory map, take the later of the two
- Considered: in-memory only · annotation on the CR · condition `lastTransitionTime` · `status.lastUpdated` alone · `status.lastUpdated` + in-memory map
- Chose both: `lastUpdated` survives restarts and shows in kubectl. The map covers the informer-cache lag right after our own patch, which would otherwise let a second write slip through early. An annotation would add the very write we're avoiding.

## Clock: injected `k8s.io/utils/clock.PassiveClock`
- Considered: call `time.Now()` directly · a custom clock interface · `k8s.io/utils/clock` with `clocktesting.FakeClock`
- Chose `k8s.io/utils/clock`: it's already a controller-runtime dependency, and the fake clock lets tests step through the throttle window deterministically with no sleeps.

## Last-write map concurrency: mutex-guarded struct
- Considered: plain map (safe only while MaxConcurrentReconciles=1) · `sync.Map` · map + `sync.Mutex` in a small type
- Chose map + mutex: it stays correct if reconcile concurrency is ever raised, it's typed (unlike `sync.Map`), and contention is negligible with one CR.

## Parallel work: 2 subagents, decisions reviewed before code
- Considered: build everything sequentially · 5 agents, one per area · 2 agents with strict file ownership
- Chose 2 agents (controller; HTTP API + UI + `cmd/main.go`) in separate worktrees, after a shared contract is merged. The lead wrote `clusterstate` in Wave 0, so both agents build against real code instead of stubs and neither waits on the other. Each agent proposes its design decisions first. They're reviewed (by me, then you) before any implementation. Only the lead deploys, and agents never edit DECISIONS.md.

## Least-privilege deploy: no metrics endpoint, no helper roles
- Considered: keep the scaffold's secure metrics + admin/editor/viewer ClusterRoles · drop them
- Chose to drop them: secure metrics needs a cluster-wide create on tokenreviews/subjectaccessreviews, and nothing scrapes it. Unbound helper roles only widen what a mis-binding could grant. Both are commented out, so they're easy to restore.

## Status patch: merge patch with optimistic lock
- Considered: plain `MergeFrom` · full-status patch · `MergeFrom` + `MergeFromWithOptimisticLock`
- Chose the optimistic lock: a stale cached base gets a 409 and retries after 1s, instead of a diff that silently leaves out a field the server has changed.

## Spec edits bypass the write throttle
- Considered: always throttle · write right away when `generation != status.observedGeneration`
- Chose bypass: spec edits are rare and made by a person, so waiting up to 10m to see one take effect is worse than at most one extra write per edit.

## ClusterView watch: generation-changed only
- Considered: every update · `GenerationChangedPredicate`
- Chose generation-changed: otherwise our own status patches retrigger reconciles.

## Node event filter: create/delete + Ready changes only
- Considered: Ready + capacity/allocatable/cordon · Ready only
- Chose Ready only: it's the only node data the CR status holds. The UI reads capacity and cordons live from the cache, so other events would only cause reconciles that write nothing.

## Throttle state: keyed by name + UID, dropped on NotFound, no hash
- Considered: never clean up · finalizer · forget on NotFound; store a content hash or not
- Chose forget on NotFound with a UID check: no extra writes or finalizer, and a recreated CR doesn't inherit the old window. No hash, because the ≥5s window is far longer than cache lag and the optimistic lock catches stale copies.

## Controller tests: direct Reconcile + fake clock, plus one real-manager wiring test
- Considered: a running manager with a fake clock · direct Reconcile only · both
- Chose both: the workqueue's delays use real time, so throttle tests call Reconcile directly with a FakeClock. One Ordered envtest with a real manager covers watches and predicates using `Eventually`.

## API reads: `mgr.GetCache()` with list/watch RBAC only
- Considered: `mgr.GetClient()` · the cache as `client.Reader`
- Chose the cache: a request can never fall through to the API server, and cache Gets need only list/watch, so no `get` verb on nodes or pods.

## API routing: stdlib `http.ServeMux`, GET only
- Considered: chi/gorilla · stdlib method patterns (which also answer HEAD) · stdlib + strict GET wrapper
- Chose stdlib + strict GET: no dependency, and every method other than GET gets 405, matching "API is read-only (GET only)".

## UI polling payload: `/api/v1/nodes` returns `{summary, nodes}`
- Considered: two calls (`/summary` + `/nodes`) · one snapshot call
- Chose one call: one build per poll, and the header always matches the cards. `/summary` stays for API consumers.

## ETags: strong, sha256 of the response body
- Considered: weak/strong; FNV/sha256; hash of the source state vs. the body
- Chose a strong sha256 of the body: it's exactly what was sent, including the effect of the namespace filter, and cheap at these sizes. The UI fetches with `cache: no-store` so the 304 reaches the JS.

## Cache transforms: strip Pod bulk and Node `status.images`
- Considered: no transform · `DefaultTransform` · per-type `ByObject` transforms
- Chose per-type: Pods drop managedFields, annotations, env/envFrom/volumeMounts/volumes; Nodes drop managedFields and `status.images` (often the largest field). ClusterView is untouched. A test checks that `clusterstate.Build` gives the same result on transformed objects.

## API server runnable: no leader election
- Considered: leader-only · every replica
- Chose every replica: it's read-only and the cache runs on non-leaders too. It starts after the cache syncs, so it never serves a cold cache.

## CR status holds counts only, never per-pod lists
- Considered: full pod list in status · per-node pod lists · counts only (per-phase, per-node)
- Chose counts only: etcd caps an object at about 1.5MB, and every status write sends the whole status. Pod and resource detail is served by the HTTP API from the cache.

## Namespace filter applies to pods only
- Considered: filter pods and nodes · pods only
- Chose pods only: nodes aren't namespaced, so node counts and readiness always cover the whole cluster.

## Sample `ClusterView` named `cluster`, applied separately
- Considered: include `config/samples` in `config/default` · apply it on its own
- Chose apply it on its own: kustomize's `namePrefix` would rename it to `clusterview-operator-cluster`, which breaks the API's lookup by name (`--clusterview-name=cluster`). Apply it with `kubectl apply -f config/samples/`.

## `clusterstate` is pure and deterministic
- Considered: compute inside the controller and API separately · one shared package that takes a client · one pure package (no client, no clock)
- Chose a pure shared package: the CR status and the API can't disagree, and it's fully table-testable. Restarts include init containers (crash-looping init should show). Resources are plain integers (milli-CPU, bytes) so the UI doesn't parse Quantities. Heartbeat times are dropped and nodes sorted, so the output is stable for change detection and ETags.

## Namespace-filter read failure: return 500, don't fall back
- Considered: fall back to all namespaces · return 500
- Chose 500: a fallback would show namespaces the user had excluded. A missing ClusterView (NotFound) still means all namespaces.

## UI-side security: no CORS, strict CSP, no HTML from data (CSP/headers deferred)
- Considered: permissive CORS · no CORS; no headers · CSP + nosniff + X-Frame-Options + Referrer-Policy; `innerHTML` · `textContent`
- Chose no CORS, `textContent` only, and JS/CSS in separate files. Without CORS, other websites can't read a localhost port-forward. `textContent` means API data is never parsed as HTML. The CSP (`default-src 'self'; frame-ancestors 'none'`) and the nosniff, X-Frame-Options and Referrer-Policy headers **landed early** in the API PR. They were already committed before the deferral reached the agent, and removing them was blocked as weakening security, so we kept them. They're still reviewed in the end-of-project security pass.

## Manager pinned to amd64 nodes, 256Mi memory limit
- Considered: no affinity · multi-arch image · amd64 affinity; scaffold 128Mi · 256Mi
- Chose amd64 affinity: the image is amd64-only, and an arm64 node would crash-loop it with exec format errors. A multi-arch build isn't needed for this cluster. Memory is dominated by the cluster-wide Pod/Node cache, so 256Mi; raise it for larger clusters.

## golangci-lint v2.15.0
- Considered: pin Go back to 1.26 · golangci-lint v2.13.1 · v2.15.0
- Chose v2.15.0: Homebrew upgraded Go to 1.27, and v2.13.1 can't read its export data. Note that `.custom-gcl.yml` pins its own version and overrides the Makefile, so bump both together.

## Removed manifests are deleted by hand
- Considered: `kubectl apply --prune` · ApplySets · delete orphans by hand
- Chose by hand for now: `make deploy` runs `kubectl apply`, which never deletes objects dropped from the manifests (e.g. the old metrics roles). `--prune` with label selectors can delete the wrong things, and that rarely happens here. Revisit if manifests start changing often.

## Status RBAC: `patch` only on `clusterviews/status`
- Considered: `get;update;patch` (scaffold) · `get;patch` (issue #7) · `patch`
- Chose `patch`: the only status write is `Status().Patch`, and status reads go through the cached ClusterView (list/watch). Note for checking: `kubectl auth can-i ... clusterviews/status` treats `status` as an object name, so use `--subresource=status`.

## Leader-election Role: leases get/create/update, events create/patch, no ConfigMaps
- Considered: keep the scaffold · drop ConfigMaps only · drop ConfigMaps and narrow verbs · also pin `resourceNames`
- Chose narrowed verbs: client-go's Lease lock only calls Get/Create/Update (releasing the lease is also an Update), and the event recorder only calls Create/Patch. controller-runtime defaults to Leases, so the ConfigMaps rule was dead and let the pod rewrite any ConfigMap in the namespace. No `resourceNames`: ours is the only lease there, and pinning it would tie the Role to `LeaderElectionID`.

## Ingress NetworkPolicy: health port only until the LoadBalancer
- Considered: no policy · allow 8080 + 8081 from anywhere · allow only 8081, with no pod sources on 8080
- Chose 8081 only: without it, any pod in the cluster can read the node/pod inventory from :8080 with no RBAC. `kubectl port-forward` reaches the pod over loopback and kubelet probes come from the node, so neither is affected. 8080 is opened deliberately at the LoadBalancer step.

## Egress NetworkPolicy: TCP 443 to any destination
- Considered: no restriction · standard `ipBlock` on the API server IP (`10.100.0.2/32`) · TCP 443 anywhere · CiliumNetworkPolicy `toEntities: kube-apiserver`
- Chose TCP 443 anywhere (keep it simple): standard NetworkPolicy, works unchanged on any cluster, and blocks DNS and all other ports. The operator only talks to the API server by IP, and image pulls are done by the kubelet. Tradeoff: a compromised pod could still reach any HTTPS host. The `ipBlock` was rejected because the IP is cluster-specific and would break if DigitalOcean moved it, and the Cilium policy because it's CNI-specific.

## API payload: drop node condition `message` and `kubeletVersion`
- Considered: keep everything · trim before going public · trim now
- Chose trim now: condition messages are free-form kubelet/CNI text (versions, paths, runtime errors), and the kubelet version makes CVE targeting easier. The UI shows neither. Kept: `type`/`status`/`reason`, node name, instance type, arch, resources.

## Before a public LoadBalancer: check the NodePort path
- Considered: rely on `loadBalancerSourceRanges` alone · also confirm NodePorts aren't publicly reachable
- Chose to confirm first: a LoadBalancer Service also opens a NodePort on every node. If the DOKS firewall allows 30000-32767, traffic to a node's public IP skips the LB's source-range firewall, and only app-level auth would cover it. The LB access method (source ranges vs auth, TLS) is decided at that step.
