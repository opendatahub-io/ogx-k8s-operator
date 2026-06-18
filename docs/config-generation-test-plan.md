# Config Generation Pipeline - Downstream Test Plan

## 1. Feature Overview

### What

The config generation pipeline allows users to declaratively define OGX server configuration through the OGXServer CR spec instead of manually authoring a full `config.yaml` in a ConfigMap. The operator generates an immutable ConfigMap containing the merged config.yaml and mounts it into the server pod.

### Why

- Reduces configuration complexity (typed fields with admission-time validation vs raw YAML)
- Enables SecretKeyRef for sensitive credentials (auto-injected into pod, no manual env var wiring)
- Catches configuration errors early via CEL/webhook validation
- Preserves base distribution config while allowing targeted overrides per API type
- Enables GitOps workflows where config changes are tracked as CR spec diffs

### Config Modes (Precedence)

| Mode | Trigger | Runtime config source |
|------|---------|----------------------|
| Override (existing) | `spec.overrideConfig` set | User-provided ConfigMap mounted directly |
| Declarative (new) | Any of `spec.providers`, `spec.resources`, `spec.storage`, `spec.disabledAPIs` set | Operator-generated ConfigMap (base + spec merge) |
| Default | Neither of the above | Image built-in config (no mount) |

### Base Config Resolution (Declarative Mode)

When declarative fields are set, the pipeline needs a base config to merge against:

1. `spec.baseConfig` ConfigMap (if set) — takes precedence
2. OCI labels on the resolved distribution image (`com.ogx.distribution.default-config` + `com.ogx.config.<filename>`)

The RH distribution image carries the base config as OCI labels, so `spec.baseConfig` is optional for RH customers.

Base config source: https://github.com/opendatahub-io/ogx-distribution/blob/main/distribution/config.yaml

### Merge Semantics

- **Providers**: Full replacement per API type. If the CR sets `spec.providers.inference`, ALL base inference providers are replaced. Other API types (vector_io, tool_runtime, etc.) are preserved from base.
- **Storage**: Full replacement when `spec.storage` is set. Otherwise base storage is preserved.
- **Models**: User models replace base models when `spec.resources.models` is set.
- **APIs**: Base `apis` list filtered by `spec.disabledAPIs`.
- **Server port**: `spec.network.port` overrides base `server.port`.
- **Everything else** (telemetry, server.auth, vector_stores, registered_resources.vector_dbs, tool_groups): Preserved from base untouched.

This "full replacement per API type" is intentional — it ensures users get exactly what they declared without phantom providers from the base config activating via stray env vars.

---

## 2. RH Distribution Happy Path

### Simplest Path — Use Distribution Defaults (No Declarative Fields)

The simplest deployment uses the RH distribution image's built-in config without any config generation:

1. User creates OGXServer CR with `distribution.name: rh`
2. User sets `workload.overrides.env` with connection details (VLLM_URL, POSTGRES_*, etc.)
3. No `overrideConfig`, no `providers`, no `resources` — none of the config fields are set
4. Operator deploys the server with the image's built-in config.yaml
5. Server starts using the built-in config, env vars are expanded at runtime by the OGX server process

```yaml
apiVersion: ogx.io/v1beta1
kind: OGXServer
metadata:
  name: ogxserver-rh
spec:
  distribution:
    name: rh
  workload:
    replicas: 1
    storage:
      mountPath: /.ogx
      size: 5Gi
    overrides:
      env:
        - name: VLLM_URL
          value: "https://vllm-inference.namespace.svc:8000"
        - name: VLLM_API_TOKEN
          valueFrom:
            secretKeyRef:
              name: vllm-secret
              key: api-token
        - name: POSTGRES_HOST
          value: postgres.namespace.svc.cluster.local
        - name: POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef:
              name: postgres-secret
              key: password
        # ... other env vars as needed by the base config
```

This works because the RH image's built-in config uses `${env.VLLM_URL:+vllm-inference}` conditional provider activation — providers only activate when their trigger env var is set.

### Next Level — Declarative Providers (OCI Label Resolution)

When users want typed fields with SecretKeyRef and admission validation:

1. User creates OGXServer CR with `distribution.name: rh`
2. User adds `spec.providers.inference` with vLLM provider(s) — endpoint as literal URL, apiToken as SecretKeyRef
3. Operator resolves base config from OCI labels on RH image
4. Operator generates config merging base + CR providers
5. Operator creates immutable ConfigMap, mounts it, auto-injects secret env vars
6. Server starts with generated config

### Example CR

```yaml
apiVersion: ogx.io/v1beta1
kind: OGXServer
metadata:
  name: ogxserver-rh
spec:
  distribution:
    name: rh
  providers:
    inference:
      remote:
        vllm:
        - id: vllm-inference
          endpoint: "https://vllm-inference.namespace.svc:8000"
          apiToken:
            name: vllm-secret
            key: api-token
          maxTokens: 4096
          network:
            tls:
              verify: false
        - id: vllm-embedding
          endpoint: "https://vllm-embedding.namespace.svc:8000"
          apiToken:
            name: vllm-secret
            key: embedding-api-token
          maxTokens: 4096
          network:
            tls:
              verify: false
    vectorIo:
      remote:
        pgvector:
        - id: pgvector
          host: pgvector.namespace.svc.cluster.local
          port: 5432
          db: pgvector
          user: pgvector
          password:
            name: pgvector-secret
            key: password
  resources:
    models:
    - name: my-embedding-model
      provider: vllm-embedding
      modelType: embedding
  workload:
    replicas: 1
    storage:
      mountPath: /.ogx
      size: 5Gi
    overrides:
      env:
        # Only needed for preserved base config storage section (kv_postgres/sql_postgres)
        - name: POSTGRES_HOST
          value: postgres.namespace.svc.cluster.local
        - name: POSTGRES_PORT
          value: '5432'
        - name: POSTGRES_DB
          value: ogx
        - name: POSTGRES_USER
          value: ogx
        - name: POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef:
              name: postgres-secret
              key: password
```

### Expected Status

```yaml
status:
  phase: Ready
  configGeneration:
    configMapName: ogxserver-rh-config-<hash>
    providerCount: 3       # 2 vllm + 1 pgvector
    resourceCount: 1       # 1 model
    configVersion: 2
    observedGeneration: 1
  conditions:
  - type: ConfigGenerated
    status: "True"
    reason: ConfigGenerationSucceeded
    message: "Generated config.yaml with 3 providers and 1 resources"
```

---

## 3. Test Cases

### 3.1 Core Generation

| ID | Scenario | Expected Result |
|----|----------|-----------------|
| TC-1 | CR with `providers.inference` (vLLM) — OCI base resolution | Generated config replaces inference block, preserves all other base sections |
| TC-2 | CR with `providers.vectorIo` (pgvector) | Generated config replaces vector_io block |
| TC-3 | CR with `resources.models` | Models appear under `registered_resources.models` |
| TC-4 | CR with `disabledAPIs: [batches, file_processors]` | APIs removed from `apis` list, provider blocks preserved but inactive |
| TC-5 | CR with `network.port: 9090` | Generated config has `server.port: 9090` |
| TC-6 | Combined: inference + models + disabledAPIs + custom port | All merge behaviors compose correctly |
| TC-7 | Only `resources.models` set (no providers) | Config generation active, base providers preserved, models replaced |

### 3.2 SecretKeyRef Auto-Injection

| ID | Scenario | Expected Result |
|----|----------|-----------------|
| TC-8 | vLLM with `apiToken` SecretKeyRef | Deployment has `OGX_VLLM_INFERENCE_API_TOKEN` env var auto-injected |
| TC-9 | pgvector with `password` SecretKeyRef | Deployment has `OGX_PGVECTOR_PASSWORD` env var auto-injected |
| TC-10 | Multiple providers with different Secrets | All env vars injected correctly, no collisions |
| TC-11 | Secret missing `ogx.io/watch` label | Operator cannot read Secret — reconcile error |
| TC-12 | Secret key doesn't exist in referenced Secret | Clear error in status/events |

### 3.3 Base Config Resolution

| ID | Scenario | Expected Result |
|----|----------|-----------------|
| TC-13 | No `baseConfig`, RH image has OCI labels | Base resolved from OCI labels, generation succeeds |
| TC-14 | Explicit `baseConfig` ConfigMap | ConfigMap takes precedence over OCI labels |
| TC-15 | `baseConfig` ConfigMap missing | Terminal error, no requeue, clear status message |
| TC-16 | `baseConfig` key not found in ConfigMap | Terminal error, clear message |
| TC-17 | No `baseConfig`, image has no OCI labels | `ConfigGenerationFailed`, requeue with error |

### 3.4 Immutable ConfigMap Lifecycle

| ID | Scenario | Expected Result |
|----|----------|-----------------|
| TC-18 | CR spec unchanged across reconciles | Same ConfigMap reused (content-hash match) |
| TC-19 | CR spec changes (add provider) | New ConfigMap created, deployment rolls out with new config |
| TC-20 | Old ConfigMaps cleaned up | Only latest 2 retained (configMapRetention = 2) |

### 3.5 Full Replacement Semantics

| ID | Scenario | Expected Result |
|----|----------|-----------------|
| TC-21 | Set `providers.inference` with 1 vLLM | ALL 9 base inference providers replaced with just 1 vLLM |
| TC-22 | Set `providers.vectorIo` with pgvector only | Base milvus/qdrant providers removed, only pgvector in generated config |
| TC-23 | Set `providers.inference` only, leave vectorIo unset | Base vector_io providers preserved (milvus, pgvector, qdrant from base) |
| TC-24 | Do NOT set `spec.storage` | Base kv_postgres/sql_postgres storage preserved entirely |

### 3.6 Server Startup

| ID | Scenario | Expected Result |
|----|----------|-----------------|
| TC-25 | Generated config mounted at `/etc/ogx/config.yaml` | Server starts, passes health check at `/v1/health` |
| TC-26 | Provider env vars resolve correctly at runtime | Server connects to vLLM/postgres using injected credentials |
| TC-27 | Status shows providers from `/v1/providers` endpoint | status.distributionConfig.providers populated |

---

## 4. Backward Compatibility - Existing `overrideConfig` Customers

### Guarantee

Existing customers using `spec.overrideConfig` are **not affected**. The declarative pipeline is only activated when declarative fields are set. `overrideConfig` continues to work exactly as before.

### Test Cases

| ID | Scenario | Expected Result |
|----|----------|-----------------|
| TC-28 | CR with only `overrideConfig` (no declarative fields) | Generation skipped, user ConfigMap mounted, `ConfigGenerated=False/Inactive` |
| TC-29 | CR with only `distribution.name` (no override, no declarative) | No config mounted, image default used, `ConfigGenerated=False/Inactive` |
| TC-30 | Operator upgrade with existing `overrideConfig` CR | No behavioral change |
| TC-31 | `overrideConfig` + `providers` on same CR | Rejected at admission (CEL validation) |
| TC-32 | `overrideConfig` + `baseConfig` on same CR | Rejected at admission (CEL validation) |
| TC-33 | `overrideConfig` + `disabledAPIs` on same CR | Rejected at admission (CEL validation) |

### CEL Mutual Exclusivity Rules

`overrideConfig` is mutually exclusive with: `providers`, `resources`, `storage`, `disabledAPIs`, `baseConfig`.

---

## 5. Migration Path for Existing Customers

### From `overrideConfig` to Declarative

1. Ensure Secrets have `ogx.io/watch: "true"` label
2. Remove `spec.overrideConfig` from the CR
3. Add declarative fields (`spec.providers`, `spec.resources`, etc.)
4. The operator resolves base config from OCI labels (or optionally via `spec.baseConfig`)
5. Operator generates config, mounts it, auto-injects secret env vars

### What Stays in `workload.overrides.env`

Only env vars consumed by **preserved base config sections**. For RH distribution, this means:
- `POSTGRES_*` env vars (storage section uses `kv_postgres`/`sql_postgres` with `${env.VAR}`)
- Any env vars used by base config sections you chose NOT to override

Provider credentials declared via SecretKeyRef are auto-injected — they do NOT go in `workload.overrides.env`.

### Migration Test Cases

| ID | Scenario | Expected Result |
|----|----------|-----------------|
| TC-34 | Remove `overrideConfig`, add `providers` | Transitions to generated mode, server restarts |
| TC-35 | Generated config produces equivalent behavior | Server functions identically after migration |
| TC-36 | Rollback: remove declarative fields, re-add `overrideConfig` | Reverts cleanly to override mode |

---

## 6. Current Release Guidance - What to Avoid

### Only override what you explicitly want to control

Each declarative field (`providers.<apiType>`, `storage`, `resources`) performs a **full replacement** of that section in the base config. If the base config has settings you want to keep, do not set the corresponding spec field — the base config section will be preserved automatically.

Examples:
- If the base config has storage backends you want to keep, do NOT set `spec.storage` — the entire storage section is preserved when the field is unset.
- If the base config has multiple conditional inference providers and you only use one, setting `spec.providers.inference` removes all others. Only set it if you want to own the full provider list for that API type.
- If the base config has vector_io providers you don't need to change, leave `spec.providers.vectorIo` unset.

**Rule of thumb**: if you're happy with what the base config provides for a section, don't declare it in the CR. Supply the required env vars via `workload.overrides.env` and let the base config handle it at runtime.

### Do NOT set `baseConfig` without declarative fields

`baseConfig` alone does not trigger config generation. It is only consumed as input when `providers`, `resources`, `storage`, or `disabledAPIs` is also set. Setting only `baseConfig` without any declarative field has no effect.

### Do NOT mix override and declarative

CEL validation prevents this at admission, but be aware there is no "partial override + partial declarative" mode. Choose one approach per CR.

### Known Follow-up Items

- [RHAIENG-5697](https://redhat.atlassian.net/browse/RHAIENG-5697) — Storage spec improvements (kv_postgres support, individual postgres fields)
- [RHAIENG-5698](https://redhat.atlassian.net/browse/RHAIENG-5698) — `endpointFrom`/`hostFrom` SecretKeyRef fields for non-secret string fields
- [RHAIENG-5699](https://redhat.atlassian.net/browse/RHAIENG-5699) — OCI label validation and caching improvements

Feature epic: [RHAISTRAT-1061](https://redhat.atlassian.net/browse/RHAISTRAT-1061)
