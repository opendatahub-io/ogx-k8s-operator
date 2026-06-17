# Running LlamaStack Operator with ODH

This guide provides instructions for deploying and using LlamaStack with ODH/OpenShift AI KServe component.

## Prerequisites

- ODH/OpenShift AI [installed](https://github.com/opendatahub-io/opendatahub-operator?tab=readme-ov-file#installation).
- Cluster configured with GPUs:
  - Minimum: 1 GPU with 16GB VRAM
  - Recommended: 2 GPUs with 24GB VRAM each
- Minimum 10 GiB memory available for the model server

## Deploying vLLM via KServe

### 1. Setup KServe in Standard Mode

#### Configure DSCInitialization (DSCI)
In the DSCI resource, set the `.spec.serviceMesh.managementState` to `Removed`:

```yaml
spec:
  applicationsNamespace: opendatahub
  monitoring: opendatahub
    managementState: Managed
    namespace: opendatahub
  serviceMesh:
    controlPlane:
      metricsCollection: Istio
      name: data-science-smcp
      namespace: istio-system
    managementState: Removed
```

#### Configure DataScienceCluster (DSC)
In the DSC resource, configure the `.spec.components.kserve` component:

```yaml
spec:
  components:
    kserve:
      defaultDeploymentMode: RawDeployment
      RawDeploymentServiceConfig: Headed
      managementState: Managed
      serving:
        managementState: Removed
        name: knative-serving
    llamastackoperator:
      managementState: Managed
```

Verify the setup by checking that `kserve-controller-manager` and `odh-model-controller` pods are running:

```bash
oc get pods -n opendatahub | grep -E 'kserve-controller-manager|odh-model-controller'
```

### 2. Deploy LLaMA 3.2 Model via KServe UI

1. Create a Connection:
   - Go to ODH dashboard -> Create Project -> Connections tab -> Create connection
   - **Project Name:** llamastack
   - **Connection Type:** URI - v1
   - **Connection name:** llama-3.2-3b-instruct
   - **URI:** `oci://quay.io/redhat-ai-services/modelcar-catalog:llama-3.2-3b-instruct`

2. Deploy the Model:
   - Go to Models tab -> Single-model serving platform -> Deploy model
   - **Model deployment name:** llama-3.2-3b-instruct
   - **Serving runtime:** vLLM NVIDIA GPU ServingRuntime for KServe
   - **Model server size:** Custom (1 CPU core, 10 GiB memory)
   - **Accelerator:** NVIDIA GPU (1)
   - **Additional serving runtime arguments:**
     ```
     --dtype=half
     --max-model-len=20000
     --gpu-memory-utilization=0.95
     --enable-chunked-prefill
     --enable-auto-tool-choice
     --tool-call-parser=llama3_json
     --chat-template=/app/data/template/tool_chat_template_llama3.2_json.jinja
     ```

3. Verify the model deployment:
```bash
oc get inferenceservice -n llamastack
oc get pods -n llamastack | grep llama
```

### 3. Create LlamaStackDistribution CR

```yaml
apiVersion: llamastack.io/v1alpha1
kind: LlamaStackDistribution
metadata:
  name: llamastack-custom-distribution
  namespace: llamastack
spec:
  replicas: 1
  server:
    containerSpec:
      env:
        - name: VLLM_URL
          value: 'https://llama32-3b.llamastack.svc.cluster.local/v1'
        - name: INFERENCE_MODEL
          value: llama32-3b
        - name: MILVUS_DB_PATH
          value: ~/.llama/milvus.db
        - name: VLLM_TLS_VERIFY
          value: 'false'
        - name: FMS_ORCHESTRATOR_URL
          value: 'http://localhost:1234'
      name: llama-stack
      port: 8321
    distribution:
      name: 'rh'
    storage:
      size: 20Gi
      mountPath: <custom-mount-path> ## Defaults to /opt/app-root/src/.llama/distributions/rh/
```

### 4. Verify LlamaStack Deployment

Check the status of the LlamaStackDistribution:
```bash
oc get llamastackdistribution -n llamastack
```

Check the running pods:
```bash
oc get pods -n llamastack | grep llamastack-custom-distribution
```

Check the logs of the LlamaStack pod:
```bash
oc logs -n llamastack -l app=llama-stack
```

Expected log output should include:
```
INFO: Started server process
INFO: Waiting for application startup.
INFO: Application startup complete.
INFO: Uvicorn running on http://['::', '0.0.0.0']:8321
```

### 5. Query the Model from Jupyter Notebook

1. Go to ODH dashboard -> Workbenches -> Workbench -> Minimal Python

2. Install required packages:
```bash
pip install llama_stack
```

3. Initialize the client:
```python
from llama_stack_client import Agent, AgentEventLogger, RAGDocument, LlamaStackClient
client = LlamaStackClient(base_url="http://llamastack-custom-distribution-service.llamastack.svc.cluster.local:8321")
```

4. Register the model
```python
client.models.register(provider_id="vllm-0", model_type="llm", model_id="llama32-3b")
```

5. Verify model registration:
```python
client.models.list()
```

Expected output should show both the LLamA model and the embedding model:
```
[Model(identifier='llama32-3b', metadata={}, api_model_type='llm', provider_id='vllm-0', provider_resource_id='llama32-3b', type='model', model_type='llm')]
```

6. Run chat completion
```python
response = client.inference.chat_completion(
    messages=[
        {"role": "system", "content": "You are a friendly assistant."},
        {"role": "user", "content": "Write a two-sentence poem about llama."}
    ],
    model_id='llama32-3b',
)

print(response.completion_message.content)
```

## Monitor Llama Stack with OpenTelemetry

### Prerequisites

Before configuring OpenTelemetry monitoring for LlamaStack, ensure you have the observability components deployed:

- [OpenShift Observability Operators](https://github.com/opendatahub-io/llama-stack-demos/tree/main/kubernetes/observability#openshift-observability-operators)


### Configure LlamaStack for OpenTelemetry

#### 1. Update LlamaStackDistribution with Telemetry Configuration

Add the required environment variables to enable OpenTelemetry telemetry collection:

```yaml
spec:
  server:
    containerSpec:
      env:
        - name: TELEMETRY_SINKS
          value: 'console, sqlite, otel_trace'
        - name: OTEL_TRACE_ENDPOINT
          value: http://otel-collector.observability-hub.local.svc.cluster:4318/v1/traces
      name: llama-stack    # match your deployment’s container name
      port: 8321           # default port
    distribution:
      image: <custom-image>
```
**Note**: The default `rh` distribution doesn't support enabling observability for `Dev Preview`. However, you can enable it by modifying `run.yaml` in your custom image as shown in the [observability configuration guide](https://github.com/opendatahub-io/llama-stack-demos/blob/main/kubernetes/observability/run-configuration.md).

#### 2. Add OpenTelemetry Sidecar Injection

To automatically inject the OpenTelemetry sidecar, add the following annotation to your llama stack server deployment:

```yaml
---
  template:
    metadata:
      labels:
        app: llama-stack
      annotations:
        sidecar.opentelemetry.io/inject: llamastack-otelsidecar # <- be sure to add this annotation to the **template.metadata**
    spec:
      containers:
```

For more advanced observability configurations refer this [configuration guide](https://github.com/opendatahub-io/llama-stack-demos/blob/main/kubernetes/observability).
