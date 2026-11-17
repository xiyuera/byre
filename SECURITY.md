# Security policy

## Reporting

Report vulnerabilities privately to the maintainers (address in the repository's contact field) rather
than in a public issue. Include a description, the affected version, and a reproduction if you have one.

## Threat model

byre is a gateway that terminates SSH, so the interesting surfaces are:

| surface | what it means here |
|---|---|
| client authentication | public key only; no passwords, keyboard-interactive or agent forwarding to clients |
| identity | `sha256` of the presented key; keys are never logged, only a 16-character prefix |
| gateway → instance | the gateway's own key, verified by host key policy (`strict` / `accept-new` / `insecure`) |
| instance scheduling | the gateway creates what its configuration says; it holds RBAC only in the namespaces it needs |
| secrets | pool keys live in a Secret mounted read-only; the config file holds paths, never key material |

## Out of scope

- A compromised instance can read anything mounted into it. byre does not sandbox the workload.
- A leaked client private key is a leaked identity. Rotating it is the operator's job.
- Multi-tenant isolation between clients is *not* a goal: two clients sharing one key are one identity
  by design (ADR-0001).

## What the project will never do

- Shell out to `kubectl` (credentials in argv), pipe keys through environment variables, or write them
  to a temp file with default permissions.
- Trust a DNS answer as proof that an instance is ready (fake-IP resolvers exist); readiness is a TCP
  connect.
