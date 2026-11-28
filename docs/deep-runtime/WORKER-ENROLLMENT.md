# Enroll a Worker with a certificate request

The existing pre-signed mTLS configuration also works without
`ca_signing_key_file`. For ticket-based enrollment, give each Worker a
dedicated self-signed signing CA. Configure the public CA as `ca_file` and
the private CA key as `ca_signing_key_file` in that Worker's entry in
platformd's configuration. Only the platform service account and operator
should be able to read the signing key. The other TLS fields still specify the
controller's client certificate and key. Serve the management API over HTTPS.

Generate the Worker leaf key and CSR on the Worker. Keep the private key there.

```bash
umask 077
openssl genpkey -algorithm ED25519 -out node-key.pem
openssl req -new -key node-key.pem -subj / -out node.csr
openssl pkey -in node-key.pem -pubout -outform DER |
  openssl dgst -sha256 -r
```

Copy the displayed digest to the configured Worker's `node_spki_pin` with
the `sha256:` prefix. Configure its HTTPS endpoint and generation in
platformd. Use generation 1 for the first enrollment and the next generation
for a rotation. Restart platformd with the new configuration before requesting
a ticket.

Use an administrative management token and the current control epoch to
request a ticket. Keep the response file private.

```bash
umask 077
printf 'Authorization: Bearer %s\n' "$platform_admin_token" |
  curl --fail --silent --show-error --cacert "$platform_ca" \
    -H @- -H "If-Control-Epoch: $control_epoch" -H 'Content-Type: application/json' \
    --data '{"mode":"initial"}' \
    "$platform_url/v1/workers/$worker_id/enrollment/ticket" > worker-ticket.json
```

Use `"rotate"` after a prior enrollment. Deliver the ticket over a trusted
channel to the Worker. The Worker retrieves its public certificate as follows.
The curl header is passed through a local stream so the ticket stays out of
curl's arguments.

```bash
umask 077
jq -n --rawfile csr_pem node.csr '{csr_pem: $csr_pem}' > worker-csr.json
jq -r '"Authorization: WorkerEnrollment " + .ticket' worker-ticket.json |
  curl --fail --silent --show-error --cacert "$platform_ca" \
    -H @- -H 'Content-Type: application/json' --data-binary @worker-csr.json \
    "$platform_url/v1/workers/$worker_id/enrollment/redeem" > worker-certificate.json
jq -er .certificate_pem worker-certificate.json > node.pem
```

Install `node.pem` and its matching key on the Node. Copy the Worker ID,
enrollment generation, controller ID and control epoch from the response into
`sandbox-node`'s authority configuration. The Node also needs the controller
CA and client SPKI pin. For rotation, perform the offline journal rebind shown
in the Node configuration example before starting the daemon. Platformd
activates the enrollment after authenticating the running Node over mTLS.

A ticket expires after five minutes. If redemption's response is lost, repeat
the exact CSR and ticket before expiry to obtain the same public certificate.
A different CSR cannot reuse the consumed ticket. An administrator can replace
an unused ticket, or issue a new ticket after expiry while the generation
remains eligible. Remove the ticket files after use. Issued leaf certificates
identify only the configured endpoint and Worker URI, with ServerAuth usage
and validity capped at 30 days and at the CA expiration.

Before activation, an administrator can withdraw the pending candidate with
the Worker enrollment revoke endpoint and the ticket's generation. A
withdrawn key can no longer redeem that ticket or activate that generation.
Configure a fresh key and its pin before starting enrollment again at that
generation.
