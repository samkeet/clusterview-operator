# Commit rules
- Commit after each logical step, never in one big batch. Don't squash or force-push.
- Conventional Commits: feat, fix, test, docs, build, chore, refactor.
- Subject under 72 characters, imperative mood. The body explains WHY and notes tradeoffs.
- Stage files explicitly. Never `git add -A`. Never stage .env, .kube/ or kubeconfigs.
- Run `go build ./...` and `go vet ./...` before each commit.
