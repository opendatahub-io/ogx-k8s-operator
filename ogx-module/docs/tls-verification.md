# Metrics TLS verification

Deploy the repaired `odh-ogx-module-operator` image. These checks target the
module operator, not the root operator that it deploys. Use a test cluster for
cluster-wide TLS changes, and restore the original settings afterwards.

The metrics endpoint uses TLS without request authentication. These bundles do
not install a metrics NetworkPolicy; TLS alone does not restrict which pods can
read metrics. Collector-specific ingress isolation needs the deployment's
collector namespace/pod selectors and a CNI that enforces NetworkPolicy. Verify
that isolation separately before relying on restricted access.

The metrics address defaults to `:8443`. The mounted ConfigMap can override
`controller.metrics.bind-address`, including `"0"` to disable metrics. The
bundles do not set an environment override for this value.

1. Render the default, OpenShift, ODH, and RHOAI bundles. For the OpenShift
   variants, verify that the metrics Service requests a serving certificate,
   the Deployment mounts the same Secret, and `--metrics-cert-path` matches
   the mount. Confirm the Pod starts without unknown-flag or certificate errors.
2. Forward the module Pod's actual metrics port, for example:

   ```sh
   oc -n <namespace> port-forward pod/<module-pod> 18443:8443
   ```

3. Request `https://localhost:18443/metrics` with `curl -vk --max-time 10`.
   Verify a TLS handshake and metrics response. Request the same URL with
   `http://`; it must not return metrics over cleartext. Then verify certificate
   trust without `-k`, using the service CA and the metrics Service DNS name
   (for example with curl's `--cacert` and `--resolve` options).
4. Under `StrictAllComponents`, configure a TLS 1.2 minimum with usable suites.
   Confirm TLS 1.2 works. Change the profile to Modern (TLS 1.3 minimum), without
   manually restarting the operator. Observe graceful shutdown and container
   restart, reestablish port forwarding, and check:

   ```sh
   openssl s_client -connect localhost:18443 -tls1_2 -brief </dev/null
   openssl s_client -connect localhost:18443 -tls1_3 -brief </dev/null
   ```

   The first handshake must fail and the second must negotiate TLS 1.3. Do not
   mistake a certificate trust error or a local client limitation for a protocol
   rejection. Change adherence separately and verify it also triggers a reload.
5. With a custom TLS 1.2 profile, offer only an allowed runtime-supported cipher
   with `openssl s_client -tls1_2 -cipher <OpenSSL-name>` and verify success.
   Offer only an excluded cipher and verify failure. Use suites compatible with
   the serving certificate, and check that the client supports the offered suite.
6. Confirm health/readiness probes still pass and the module continues to deploy
   the root operator and report OGX status. Exercise serving certificate rotation
   and verify new connections see the replacement certificate.

The unit tests cover denied API reads, missing API/resource and transient
fallbacks, bounded bootstrap requests, invalid minimums, unusable/mixed cipher
requests, exact Old/custom minimums under strict or unknown adherence, and
profile/adherence watcher callbacks. These include inputs the cluster API may
reject before they can be exercised through a real APIServer object.
