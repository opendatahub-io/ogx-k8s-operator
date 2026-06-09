# OGX Module

This directory introduces a new, separate ODH module-operator subtree for OGX.

The main change is architectural: instead of extending the repository root
operator bootstrap path, we are adding a dedicated `ogx-module/` module with its
own Go module, manager entrypoint, API package, config tree, and documentation.

## Why this was added

The repository root still contains the standalone OGX operator that reconciles
`OGXServer` resources under the `ogx.io` API group. That path is kept intact.

The new `ogx-module/` subtree is being introduced so ODH-facing module support
can evolve independently:

- its own binary and startup flow
- its own ODH-facing API surface
- its own module manifests
- its own controller and packaging structure

This avoids coupling ODH module work to the standalone operator's existing
`main.go`, config layout, and release path.

## Important separation from the root operator

The root operator and the new module subtree serve different purposes:

- Root operator:
  - standalone OGX operator
  - reconciles `OGXServer`
  - uses the existing repository-wide bootstrap and packaging flow
- `ogx-module/`:
  - ODH module-operator path
  - will reconcile the ODH-facing `OGX` custom resource
  - will carry its own module manifests and controller wiring

In other words, this directory does not replace the root operator. It creates a
parallel module-oriented implementation path for ODH integration.

## Feature overview

The OGX module feature is intended to provide an ODH-facing module operator
path with the following characteristics:

- a dedicated `OGX` custom resource under
  `components.platform.opendatahub.io`
- a singleton, platform-oriented control plane for OGX
- module-specific manifests and packaging that can be vendored by ODH
- separate controller wiring and startup flow from the standalone operator
- room for ODH-specific configuration, lifecycle, and status reporting without
  changing the root operator contract

This subtree is the place where the ODH module implementation lives, while the
root of the repository remains the home of the standalone `OGXServer`
operator.

## What this module includes

The `ogx-module/` subtree is intended to contain the ODH-facing OGX module
implementation end to end. That includes:

- the `OGX` module API under `components.platform.opendatahub.io`
- the module controller and its reconciliation flow
- module-specific bootstrap manifests for deployment by ODH
- module configuration consumed from the ODH-provided ConfigMap
- status reporting back through the `OGX` custom resource

It does not replace the standalone `OGXServer` API. Instead, it adds the ODH
module control plane that ODH can deploy, configure, and monitor as part of
the platform.

## Architecture and ownership

The intended ownership model is:

```mermaid
flowchart TD
  odhOperator["ODH operator"] -->|"deploys module bootstrap manifests"| ogxModule["ogx-module controller"]
  moduleConfig["module ConfigMap"] -->|"platform configuration"| ogxModule
  ogxModule -->|"reconciles"| ogxCR["OGX\ncomponents.platform.opendatahub.io"]
  ogxCR -->|"drives deployment of"| rootManifests["root OGX operator manifests"]
  rootManifests --> rootResources["Deployment / Service / RBAC / ConfigMaps / supporting resources"]
  rootResources --> rootOperator["root OGX operator"]
  rootOperator -->|"reconciles"| ogxServer["OGXServer\nogx.io"]
```

At a high level:

- ODH is responsible for deploying the module operator itself
- the module operator is responsible for reconciling the ODH-facing `OGX` CR
- the module operator manages the deployment of the **root OGX operator
  manifests**
- the deployed root OGX operator remains the controller that reconciles
  `OGXServer`

## How OGX is deployed in ODH

With this structure, ODH no longer needs to wire OGX by extending the root
operator bootstrap directly. Instead, the deployment flow becomes:

1. ODH vendors the module bootstrap manifests produced from `ogx-module/`.
2. ODH deploys the OGX module operator into the target ODH namespace.
3. ODH creates or manages the singleton `OGX` custom resource.
4. The OGX module operator reconciles that `OGX` resource and deploys the
   manifests of the root OGX operator.
5. The deployed root OGX operator manages `OGXServer` resources and the
   associated OGX runtime resources.
6. Status is reported back through the `OGX` CR so ODH can aggregate module
   health.

This gives OGX an ODH-native module path with its own API, controller, and
packaging lifecycle, instead of mixing ODH module behavior into the standalone
root operator entrypoint.

## Directory layout

```text
ogx-module/
  README.md
  go.mod
  cmd/ogx-module/main.go
  config/
  docs/
  pkg/apis/v1alpha1/
  tests/scripts/
```
