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
- Chose 2 agents (clusterstate + controller; HTTP API + UI + `cmd/main.go`) in separate worktrees, after a shared contract is merged. Each agent proposes its design decisions first. They're reviewed (by me, then you) before any implementation. Only the lead deploys, and agents never edit DECISIONS.md.
