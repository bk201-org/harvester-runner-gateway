# Working agreements

## Legacy implementations

When a task may require preserving, migrating, removing, or otherwise handling
a legacy implementation or compatibility behavior, ask the user to confirm the
intended legacy scope before making changes. Do not assume that backward
compatibility is required or that legacy behavior may be removed.

## Kubernetes updates

Wrap every Kubernetes read-modify-update operation in a bounded conflict retry.
Fetch the latest object and repeat ownership and integrity checks inside each
retry attempt; never retry an update using the stale object. Add a regression
test that injects a conflict when introducing a new update path.

Do not apply this rule blindly to action subresources such as start, stop,
restart, attach, or detach. Retrying those calls may duplicate side effects and
requires operation-specific idempotency analysis.
