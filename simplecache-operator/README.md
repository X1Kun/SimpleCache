# SimpleCache Operator

Kubebuilder controller for cache.x1kun.com/v1 SimpleCache. It manages a StatefulSet,
peer/API Services, cache ServiceAccount and namespace discovery Role/Binding.

The [project guide](../docs/design.md#membership-operator-and-lifecycle) is the
single source for the current CR fields, Reconcile behavior and controller
boundaries. See its [deployment instructions](../docs/design.md#kubernetes-deployment),
[validation](../docs/design.md#commands-and-reports), and
[agreed roadmap](../docs/design.md#delivery-plan).

From this module, use make manifests generate, make test, and make lint-fix for
API generation, envtest and lint. Generated files must come from types/markers;
preserve PROJECT and Kubebuilder scaffold markers. Repository CI lives in
../.github/workflows.
