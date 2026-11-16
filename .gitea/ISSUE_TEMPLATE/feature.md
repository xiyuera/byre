---
name: Feature or driver proposal
about: A new backend, a new capability, or a change to lease semantics
labels: enhancement
---

**Problem**
<!-- what is impossible or painful today, from the operator's seat -->

**Proposal**
<!-- the shape of the change; if it touches lease semantics, say which ADR it extends -->

**Alternatives considered**
<!-- including "do nothing" -->

**Backend proposals only**
- What obtains an instance:
- What reaches it over SSH:
- What returns it:
- How a restart recognises its own objects:

**Scope check**
- [ ] does not require a third-party service
- [ ] does not hardcode one environment's names or CRD
- [ ] keeps the client contract (one identity, one instance per lease)
