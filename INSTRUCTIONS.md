# Reaching the ClusterView UI

The UI is not exposed publicly. You reach it with `kubectl port-forward`, using
your own cluster credentials.

## Before you start

- Load the environment from the repo root, so `kubectl` (and Claude Code, if
  you use it) gets the cluster credentials and kubeconfig path:

  ```sh
  cp .env.example .env    # first time only, then fill in the placeholders
  source .env && claude   # or just `source .env` if you're not using Claude Code
  ```

  `.env` exports `KUBECONFIG` (the cluster's kubeconfig under `.kube/`) and
  the Claude Code settings: the OpenRouter API key, base URL, model overrides
  and config dir. `.env.example` lists every key with placeholder values.
  `.env` itself is gitignored, so never commit it or paste its contents
  anywhere. Check you're on the right cluster with
  `kubectl config current-context`.

- Your user needs `get services`, `get pods` and `create pods/portforward` in
  the `clusterview-operator-system` namespace. Check with:

  ```sh
  kubectl auth can-i create pods/portforward -n clusterview-operator-system
  ```

- The cluster must be running the operator. Check that the pod is `Running` and
  `1/1` ready:

  ```sh
  kubectl -n clusterview-operator-system get pods
  ```

## Open the UI

```sh
kubectl -n clusterview-operator-system port-forward svc/clusterview-operator-ui 8080:80
```

Leave that running, then open <http://localhost:8080>.

The Service is named `clusterview-operator-ui`, not `ui`: kustomize adds the
`clusterview-operator-` prefix to every resource.

### If port 8080 is already in use

Pick another local port, for example 9090:

```sh
kubectl -n clusterview-operator-system port-forward svc/clusterview-operator-ui 9090:80
```

Then open <http://localhost:9090>.

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

The terminal running `port-forward` prints `Handling connection for 8080` for
each request. If the port-forward drops, the page keeps showing the last data,
dimmed, with a "Can't reach API" banner. If it stays on "Loading…" or the
port-forward exits, check that the operator pod is running (see above) and
restart the port-forward.

The same summary is on the `ClusterView` resource itself:

```sh
kubectl get clusterview
```

The JSON API is available on the same port, for example:

```sh
curl -s localhost:8080/api/v1/summary
curl -s localhost:8080/api/v1/nodes
curl -s localhost:8080/api/v1/nodes/<node-name>/pods
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
