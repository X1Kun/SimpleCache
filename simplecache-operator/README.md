# SimpleCache Operator

Kubebuilder controller for `cache.x1kun.com/v1` SimpleCache. It manages a
StatefulSet, notification/API Services, namespace discovery RBAC and optional
Envoy Gateway resources. Each cache instance always reads locally. Without a
Gateway, replicas are independent caches; key sharding requires `spec.gateway`.

The notification EndpointSlice port is `cluster`; runtime discovery uses
`CLUSTER_SERVICE`. `hotCacheBytes` was removed from the CRD. See the
[design and migration guide](../docs/design.md) for fields, API contracts,
configuration and incompatible upgrade steps.

After API changes run `make manifests generate`; use `make lint-fix` and
`make test` for lint/envtest. Generated CRDs, RBAC and deepcopy files must come
from markers; preserve PROJECT and Kubebuilder scaffold markers. Real-cluster
Gateway validation runs through the repository's uniquely owned Kind suites.
