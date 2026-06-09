# OGX Module

This subtree hosts the ODH module-operator entrypoint for OGX.

It is intentionally separated from the repository root operator so the module
can evolve with its own API, manifests, and controller wiring without changing
the existing standalone `OGXServer` operator bootstrap path.

Current scope:

- dedicated Go module: `[go.mod](../go.mod)`
- dedicated manager entrypoint: `[cmd/ogx-module/main.go](../cmd/ogx-module/main.go)`
- initial ODH-facing `OGX` API: `[pkg/apis/v1alpha1](../pkg/apis/v1alpha1)`
- placeholder config tree for module packaging: `[config](../config)`

Later tasks add the reconciler, deployer, and vendorable manifests.
