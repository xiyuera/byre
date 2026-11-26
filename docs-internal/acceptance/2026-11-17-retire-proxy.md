# Retirement record — 2026-11-17: the incumbent proxy is gone

After every client identity had been verified against the deployed gateway, the proxy it replaces was
removed. This is the last step of "build new → verify → migrate → retire" and the point at which the
gateway is no longer a parallel path but *the* path.

## Order of operations

1. Each profile's SSH backend moved to the gateway (own key, gateway port); each was verified with a real
   session, not just a handshake.
2. The proxy's objects were dumped to files **before** deletion: Deployment, Service, ConfigMap,
   ServiceAccount, Role, RoleBinding. A rollback means re-applying those six objects — the gateway shares
   nothing with them, so both can coexist again if needed.
3. Only the proxy's own objects were deleted. **The instance template and the warm pool were kept** —
   the gateway creates its instances from the same template/pool, and deleting them would have stranded
   both systems.
4. The proxy's leftover instances (objects without an identity annotation) were deleted; the gateway
   would have destroyed them on its next adoption pass anyway, so this makes the state explicit.

## Verification after retirement

```
# no proxy objects, no proxy pods, no unmanaged instances
deployments/services/configmaps/serviceaccounts/roles/rolebindings -> none
claims with a proxy origin                                            -> none

# the gateway serves every identity, each on its own instance
identity-1  instance-1
identity-2  instance-2
identity-3  instance-3
identity-4  instance-4
identity-5  instance-5
identity-6  instance-6
connected: 6/6   distinct: 6/6

# acceptance against the Service
PASS  first connection
PASS  reconnect returns the same instance
PASS  tunnel carries the instance sshd banner
INFO  leases adopted at startup: 5
INFO  warm instances: 2
passed=3 failed=0
```

`adopted: 5` is the migration itself: five identities were already holding instances when the last
gateway pod started, and it took them over instead of creating new ones.

## What this proves about the design

- **The proxy and the gateway were never entangled.** Retirement was deletion, not surgery: no shared
  Service, no shared key, no shared configuration. They ran side by side on different ports for the
  whole migration.
- **Identity, not infrastructure, is the unit of migration.** Each profile moved independently, and the
  gateway never needed to know which one was talking to it — only which key.
- **Instances outlive the process.** The five leases survived a gateway restart in the middle of the
  migration (see `2026-11-17-deploy.md`), so migration order did not have to be coordinated with the
  gateway's lifecycle.

## Rollback

Re-apply the six backed-up objects, point the profiles back at the old Service and port, and let the
gateway's leases expire. Nothing in the instances needs undoing: they are the same instances either
system would have used.
