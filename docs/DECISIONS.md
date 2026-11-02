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

## UI exposure: port-forward only (final)
- Considered: public LoadBalancer · ClusterIP + `kubectl port-forward` · Ingress · Cloudflare Tunnel + Access
- Chose port-forward only: the API has no auth of its own, so port-forward makes access ride on cluster credentials and RBAC (`create pods/portforward`), and nothing is exposed to the internet. That removes the LoadBalancer, the NodePort-bypass risk and TLS/auth work. TASK.md accepts "port-forward instructions"; they're in `INSTRUCTIONS.md`. Started as "port-forward first, LoadBalancer later"; the user decided against public exposure.

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

## Controller cache reads: zero-copy with a guard test (deferred, #21)
- Considered: keep deep-copying · `client.UnsafeDisableDeepCopy` · zero-copy plus a test that inputs are unchanged
- Chose zero-copy plus the guard test, not implemented yet: `observe()` deep-copies every cached Pod and Node on every reconcile, including throttled ones (10.4 ms and 48.5 MB per reconcile at 500 nodes / 15k pods, vs 0.47 ms and 0.3 MB without the copy). Tradeoff: the items share maps and slices with the informer cache, so an in-place change, such as sorting `node.Status.Conditions`, would silently corrupt it for every reader. The fake client still copies, so only the guard test (deep-copy the input, run `Build`, assert the input is unchanged) catches that.

## Throttle check before rebuilding (deferred, #22)
- Considered: always observe, then decide · return early when the throttle blocks the write and the generation is unchanged
- Chose the early return, not implemented yet: today the rebuilt snapshot is thrown away whenever the throttle blocks the write. The level-triggered design is unchanged, because the requeued run reads the full current cache. Tradeoff: part of `decide()` is duplicated and needs its own table test.

## `GOMEMLIMIT` from the container memory limit (deferred, #23)
- Considered: leave the Go GC unaware of the limit · hard-code a value · set it from `limits.memory` through the downward API
- Chose the downward API, not implemented yet: GC then works harder as the heap nears 256Mi, instead of the pod being OOM-killed. The value follows the manifest automatically.

## API response cost: versioned snapshot, ETag from the version (deferred, #24, #25)
- Considered: rebuild per request (today) · a fixed-time cache · informer-driven version counter with an ETag derived from it and an LRU of encoded bodies · plus a concurrency limit
- Chose the version counter, the LRU and a concurrency limit, not implemented yet: today the ETag is computed from the body, so a 304 costs as much as a 200 (1.31 ms vs 1.35 ms at 15k pods). With an ETag of `hash(version, query, generation)`, a 304 is answered before any work. The concurrency limit (503 when full) caps the total cost, since about 1.3 ms of CPU per request uses up the 500m limit at roughly 370 requests per second. The concurrency limit must land before the public LoadBalancer.

## Dashboard filters: only narrow the CR filter, use indexes and a slim model (deferred, #26)
- Considered: filter in the browser · server-side scans of raw Pods · cache indexes for selective filters plus a slim per-snapshot model for free-text filters
- Chose indexes plus the slim model, design only: if filters are added, they're GET query parameters from an allowlist, validated and size-capped. They're always intersected with `spec.namespaces` and can never widen it, because the CR filter is the admin boundary. Results are capped or paginated. The cache key is (snapshot version, normalized query), building on #24.

## Aggregation: full rebuild, not incremental counters
- Considered: incremental per-node counters updated in event handlers · full level-triggered rebuild
- Chose the full rebuild: counters make each event O(1), but bring relist, missed-event and filter-change edge cases that the level-triggered design avoids, all to save a 0.47 ms rebuild. Revisit at around 100k pods.
## Pod events UI: overlay drawer with Events / Logs / Details tabs
- Considered (mockups in `docs/design/events/`): A overlay drawer with compact rows · B docked timeline with cursor pages · C tabbed pod panel with cards and a Warning filter
- Chose A's drawer and compact rows plus C's pod header (namespace/phase/restarts/node chips) and tabs. The rows are the densest view of the three, and the tabs give pod logs and details a place without a later redesign. B's docked layout squeezes the pod table, and its timeline shows fewer events per screen. C's cards take more space per event.

## Events pagination: offset, 5 per page, numbered pages
- Considered: offset (`?limit=5&offset=N`) · cursor (`?before=<cursor>`, stable as events arrive)
- Chose offset: it allows "1–5 of N" and numbered pages, and pods have few events (Kubernetes deletes them after about an hour), so shifting pages rarely matter. Only page 1 auto-refreshes, so an older page doesn't move under you. Cursor paging was rejected because it can't show page numbers.

## Events source: cached informer indexed by involved object
- Considered: query the API server on each click · informer cache of `events.k8s.io/v1` Events with an index on the object each event is about
- Chose the cache: it keeps the "no API server calls per request" rule, and the index makes a lookup touch only that pod's events. The cost is cluster-wide list/watch on events and memory bounded by the event TTL. A transform drops managedFields. The controller doesn't watch events, because they don't affect CR status. Event messages are returned in full for now and reviewed in the security pass before going public.

## Event time and count: fallback chains that match kubectl
- Considered: `eventTime` only · `series.lastObservedTime` only · a fallback chain
- Chose fallbacks: lastSeen = first non-zero of `series.lastObservedTime` → `deprecatedLastTimestamp` → `eventTime` → `deprecatedFirstTimestamp` → creation time. count = `series.count` → `deprecatedCount` → 1. On k8s 1.37 the kubelet still uses the old recorder (all 43 of its events had no `eventTime`), so `eventTime` alone would leave most pod events without a time. New-API repeats can show one fewer than the true count; we match kubectl rather than guess.

## Pod events lookup: kind/ns/name index, UID filter, deleted pods allowed
- Considered: 404 if the pod isn't cached · always return events; key `ns/name` · `kind/ns/name`
- Chose `kind/ns/name` (ready for node events) and no pod-existence requirement: events outlive deleted pods, which is exactly when you want them. When the pod is cached, only events with its UID are kept, so an earlier pod with the same name (StatefulSet `web-0`) doesn't leak in. Unknown pod → 200 with `total: 0`. A namespace outside the filter → 404; that only reveals config, not pod data.

## Events cache: pod events only, 1 KiB message cap, memory measured later
- Considered: cache all events · field selector `regarding.kind=Pod` · scope by namespace filter; raise the memory limit now or after measuring
- Chose `regarding.kind=Pod` with a transform that drops managedFields, annotations and unused fields, and caps messages at 1 KiB (shown with "…"). That's 37% fewer events on this cluster, and node events later need a one-line cache change. The namespace filter is dynamic and per-CR, so it's applied when a request is read, not in the cache. The 256Mi limit stays until we measure (see issue #23 on GOMEMLIMIT).

## Events response: whole-body ETag, `name` per event, page-1 polling for "new events"
- Considered: ETag on events only · whole body; detect new events by `total` · by page 1's ETag
- Chose the whole-body ETag ("1–5 of N" changing is a real change) and a stable `name` per event, so expanded rows survive re-renders. On older pages the UI still polls page 1 with its ETag, which also catches ×count bumps that `total` misses. Polling runs only while the drawer is open on the Events tab.

## Node pool view: capacity map (B), minimal scope
- Considered (mockups in `docs/design/pools/`): A pool → node accordion · B capacity map with one stacked segment per pod, coloured by namespace · C namespace × pool matrix
- Chose B: it answers "which pods are using up this node, and from which namespace" at a glance, with a namespace legend filter and a click-through `kubectl describe`-style pod table. A shows totals but hides who uses them. C is a separate question we don't need yet.
- In scope: a `#/pools` page with Nodes | Node pools nav, and one `GET /api/v1/pools` endpoint (pools → nodes → pods with namespace, name, CPU/memory requests and limits, QoS) with an ETag. The node table reads from the same response.
- Out of scope for now: actual usage, a configurable pool label, the namespace matrix, sorting, deep links.

## Capacity numbers: requests and limits as % of allocatable, like kubectl describe
- Considered: requests/limits from pod specs · real usage from metrics-server
- Chose requests/limits: metrics-server isn't installed, and they need no new permissions or components. Effective pod requests are computed like kubectl (init/sidecar containers, overhead, pod-level resources) with `k8s.io/component-helpers/resource`. Only non-terminated pods count. Limits above 100% are flagged as overcommitted.

## Pool grouping: hardcoded DOKS label, "unpooled" fallback
- Considered: hardcode `doks.digitalocean.com/node-pool` · a `--nodepool-label` flag · try a list of known cloud labels
- Chose hardcoded, to keep scope minimal: this cluster is DOKS. Nodes without the label show as "unpooled" rather than disappearing. A flag is a small change if another provider is ever needed.

## `/api/v1/pools` payload: every pod, minimal fields
- Considered: per-pod data loaded per node (like the Nodes page) · everything in one response
- Chose one response: the capacity map draws every node's per-pod segments at once. Each pod carries only 7 small fields and the poll is ETag'd, so it costs a few hundred bytes per pod only when something changes. Tradeoff: payload grows with the pod count. If that becomes a problem, segments under ~1% can be folded into "other".

## Pool capacity math: kubectl's own helper, truncated percentages
- Considered: hand-written max(init)/sum(containers) · copying kubectl's code · `k8s.io/component-helpers/resource`
- Chose the helper with kubectl 1.37's options (`UseStatusResources`, pod-level resources on): it's the code `kubectl describe node` runs, so sidecars, init containers, overhead and in-place resize can't drift. Percentages are truncated, as describe does (310m/3890m prints 7%, not 8%). Segment widths use the exact value. Adds one module (`k8s.io/component-helpers v0.37.0`) with no version bumps. Cilium's limit coming from an init container is the real-cluster test case.

## Pool view details (approved picks)
- The events drawer closes when switching to `#/pools`.
- With a namespace filter set, node totals cover the included namespaces only and are labelled "(filtered namespaces)". There's no hidden "other" aggregate (minimal scope).
- Pools show `instanceType` when all their nodes share one.
- An unset limit shows `0` like describe, with a "no limit" hover.
- The fallback group is named `(unpooled)`. Parentheses aren't valid in label values, so a real pool can't collide with it.
- Contract changes: nodes gain `schedulable`; the response is `{pools:[…]}`; an empty label counts as unpooled.
- Namespace colours: `kube-system` is grey, the others get sticky session slots from a 6-colour palette (no red or green, to avoid confusion with status). They're CSS classes, and widths are set through the CSSOM, so CSP holds.

## Demo workload and one-command bring-up
- Considered: ad-hoc throwaway pods · a committed `config/demo/` workload; README steps · a `make up` target
- Chose a committed 20-pod workload in 3 namespaces (`demo-shop`, `demo-payments`, `demo-batch`), kept running for the demo. It covers Burstable, Guaranteed and BestEffort QoS, a native sidecar, a crash loop and a failing readiness probe, so both the capacity map and the events drawer have real data. `make up` (via `hack/up.sh`) checks the env, builds and pushes (or reuses an image with `SKIP_BUILD=1`), ensures the pull secret, deploys, applies the sample ClusterView and the demo, then prints the port-forward command. `make down` reverses it. Steady pods use `registry.k8s.io/pause` to avoid Docker Hub rate limits.
