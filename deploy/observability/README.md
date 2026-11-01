# Historical monitoring example

`legacy-static-prometheus.yaml` was previously the root `values.yaml`.
It is a Kubernetes manifest, not a Helm values file. It retains its original
five-Pod static scrape targets and `monitoring` namespace; it is not part of the
default installation or validation workflows. Align its namespace and targets
with an explicitly chosen deployment before use.

The current cache API exposes `/metrics`; the Operator supports Prometheus
configuration under `simplecache-operator/config/prometheus`.
