---
name: Bug report
about: Something behaves differently than the docs say
labels: bug
---

**What happened**
<!-- one paragraph, no logs yet -->

**What you expected**
<!-- and why: link the ADR or the doc line that promises it -->

**Which invariant is affected**
- [ ] one identity's session moved between instances
- [ ] two identities shared an instance
- [ ] a lease expired while the client was still using it
- [ ] an instance was orphaned / duplicated across a restart
- [ ] neither / not sure

**Environment**
- byre version (`byre -version`):
- backend type (`static` / `kubernetes` + `provider.kind`):
- pool size, ttl, grace:
- client (and whether it uses `ControlMaster`):

**Reproduction**
```sh
# the smallest command sequence that shows it
```

**Logs**
<!-- JSON lines are fine. Identity prefixes are fine. Never paste a key. -->
