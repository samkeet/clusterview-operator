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
