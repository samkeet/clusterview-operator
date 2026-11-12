# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

# clusterview-operator
Go k8s operator (controller-runtime, kubebuilder). Watches Nodes, Pods and pod Events, keeps a
counts-only summary in a cluster-scoped `ClusterView` CR (`observe.samkeet.org/v1alpha1`), and serves
a read-only JSON API plus an embedded web UI. README.md has the user-facing detail, docs/DECISIONS.md
the reasoning behind each design choice (read it before changing semantics).

# Commands
- `make test`: manifests, generate, fmt, vet, then unit + envtest suites (excludes `test/e2e`). Required before any Go commit.
- `make lint` / `make lint-fix`: golangci-lint v2 with a custom build that includes `logcheck` (K8s log message style). `sort` is banned by depguard, use `slices`.
- `make manifests generate`: after editing `*_types.go` or any `+kubebuilder` marker.
- `make test-integration`: runs the real binary against a local kwok cluster as its own ServiceAccount (needs `kwokctl`, `kubectl`). Catches RBAC drift. `CV_IT_KEEP=1` reuses the cluster.
- `make test-e2e`: Kind cluster only, never a real one.
- `make up` / `make ui` / `make down`: bring-up and teardown against the cluster in `.env`'s `KUBECONFIG`. `make up` never builds; it deploys the image pinned in `hack/IMAGE`. `make release` (clean tree, podman, GHCR push) builds HEAD and rewrites that pin.
- Single test: pure-Go tests run directly, e.g. `go test ./internal/clusterstate -run TestName`. The controller package needs envtest binaries (`make setup-envtest` once):
  `KUBEBUILDER_ASSETS="$(bin/setup-envtest use -i --bin-dir bin -p path)" go test ./internal/controller -run TestName`.
  Ginkgo specs run under `TestControllers`: add `-run TestControllers -ginkgo.focus "text"`.

# Environment
- `.env` and `.kube/` hold the kubeconfig and tokens. Never read, print or source them; `make up` and `make ui` load `.env` themselves (via `hack/env.sh`).
- podman is the container engine (Docker isn't installed). Images are cross-compiled for linux/amd64, tagged with the git short SHA.

# Architecture
- `cmd/main.go` wires everything: a shared cache trimmed with `ByObject` transforms (`api.StripPod/StripNode/StripEvent`), field indexes (pods by `spec.nodeName`, events by the pod they concern), the reconciler, and the API server as a manager `Runnable`.
- Nothing polls the API server. The reconciler (`internal/controller`) and the HTTP handlers (`internal/api`) both read only from that one cache, so a field a transform drops is invisible to both. Add the field to the transform before reading it.
- `internal/clusterstate` is pure aggregation (summary, node pools, capacity) shared by the controller and the API. Keep it free of client/cache code so it stays table-testable.
- Reconcile is level-triggered: it rebuilds from the full cache, then `decide` (`status.go`) gates the status patch with a write throttle (`spec.refreshInterval`, skipped when content is unchanged, spec edits bypass it). Last-write time is the later of `status.lastUpdated` and the in-memory `writetracker`, which covers cache lag after our own patch. A 5m requeue is the backstop. Time comes from an injected `clock`, so tests use a fake clock, not sleeps.
- `api.Server` runs on every replica (`NeedLeaderElection() == false`), is GET-only, and uses ETag/304. The `ClusterView` named by `--clusterview-name` (default `cluster`) supplies the namespace filter for all pod-level responses.
- The UI (`internal/api/web/`, embedded) is served under a strict CSP and never builds HTML from API data. It is reached only via `kubectl port-forward`; nothing is exposed publicly.

# Generated files (never hand-edit)
`config/crd/bases/*`, `config/rbac/role.yaml`, `**/zz_generated.*.go`, `PROJECT`. Keep every `+kubebuilder:scaffold:*` marker. AGENTS.md has the generic kubebuilder scaffold guidance.

# Rules

## Commits and PRs
- Commit after each logical step, never in one big batch. Don't squash or force-push; PRs merge with merge commits.
- Conventional Commits: feat, fix, test, docs, build, chore, refactor.
- Subject under 72 characters, imperative mood. The body explains WHY and notes tradeoffs.
- Stage files explicitly. Never `git add -A`. Never stage .env, .kube/ or kubeconfigs.
- Before each commit: `make test` if it touches Go code, otherwise `go build ./... && go vet ./...`.
- Images must build for linux/amd64
- Every change must be a pull request. Never push to `main`. PRs are deployed to the cluster and checked before merging.

## Testability
- Table-driven unit tests for pure logic
- envtest for controller behavior (Eventually, not sleeps)
- httptest with a fake client for handlers

## Go quality
- Wrap errors: fmt.Errorf("doing X: %w", err). Never swallow errors
- Structured logging via logr with key/value pairs
- HTTP server timeouts set. API is read-only (GET only)
- gofmt, go vet and golangci-lint clean
- Comments explain why, not what. Keep it simple, no premature abstraction

## RBAC (least privilege)
- Only what the code uses, via kubebuilder markers placed next to the code that needs the permission (the controller for nodes, pods and ClusterViews; `internal/api/events.go` for events). `make manifests` collects them into `config/rbac/role.yaml`.
- The operator has no `get` on pods, no logs, no secrets. A new verb means a marker change and a `make test-integration` run, which enforces RBAC.
- Verify on a cluster with `kubectl auth can-i --as=system:serviceaccount:<ns>:<sa>`

## Deployment (kustomize)
- config/ base with overlays if needed.
