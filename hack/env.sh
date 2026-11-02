# Sourced by the hack/ scripts. Loads .env when the caller hasn't, so
# `make up` / `make ui` work without a separate `source .env` step.
# An already-set KUBECONFIG wins, so pointing at another cluster still works.
if [[ -z "${KUBECONFIG:-}" && -f .env ]]; then
  # shellcheck disable=SC1091
  source .env
fi
