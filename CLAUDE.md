# clusterview-operator
Go k8s operator (controller-runtime, kubebuilder)

# Commit rules
- Commit after each logical step, never in one big batch. Don't squash or force-push.
- Conventional Commits: feat, fix, test, docs, build, chore, refactor.
- Subject under 72 characters, imperative mood. The body explains WHY and notes tradeoffs.
- Stage files explicitly. Never `git add -A`. Never stage .env, .kube/ or kubeconfigs.
- Run `go build ./...` and `go vet ./...` before each commit.
- Images must build for linux/amd64
- Every change must be a pull request. Never push to master.

## Testability
- Table-driven unit tests for pure logic
- envtest for controller behavior (Eventually, not sleeps)
- httptest with a fake client for handlers
- `make test` must pass before every commit that touches Go code

## Go quality
- Wrap errors: fmt.Errorf("doing X: %w", err). Never swallow errors
- Structured logging via logr with key/value pairs
- HTTP server timeouts set. API is read-only (GET only)
- gofmt, go vet and golangci-lint clean
- Comments explain why, not what. Keep it simple, no premature abstraction

## RBAC (least privilege)
- Only what the code uses, via kubebuilder markers on the controller
- Verify with `kubectl auth can-i --as=system:serviceaccount:<ns>:<sa>`

## Deployment (kustomize)
- config/ base with overlays if needed.
