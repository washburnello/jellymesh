# Runbook: recovering from a compromised node key

Use this when a node's private key may be in someone else's hands: a stolen
disk or backup, a compromised host, or an unexplained action attributed to
that node. It implements the decision in design-spec.md section 8, "Key
compromise and recovery": there is no key rotation, and a node with a new key
is a new node.

Commands are shown as `jellymesh ...`. In a container deployment, run them as
`docker compose exec jellymesh /jellymesh ...` on the node named.

## 1. Cut the compromised key off (any owner or administrator)

On the owner's node, or any administrator's node, eject the compromised node:

```bash
jellymesh status                 # find the node ID of the compromised node
jellymesh eject <node-id>
```

The ejection is a signed log event. Every member that applies it stops
trusting the key at once, so the key can no longer complete a handshake with
them (conformance C-OP-3). If the owner's node is offline, an administrator's
ejection queues and applies when the owner is back. If speed matters more
than waiting, every member can also block the node locally at once:

```bash
jellymesh block <node-id>        # on each member's node
```

A block takes effect without waiting for the log, and lasts until it is lifted
explicitly.

**If the compromised node is the owner's:** an administrator cannot eject the
owner. Block the owner's node on every other member, then let succession run:
once the owner has been unreachable for the full window, members attest, and
the eligible administrator claims ownership (conformance C-PO-24). The
former owner's key is fenced out of the new epoch. The new owner then ejects
it.

## 2. Confirm the cut

On a few members' nodes:

```bash
jellymesh sync
jellymesh status                 # the ejected node should be absent from members
```

The ejected node's catalog records are removed at each member's next catalog
pass (`jellymesh catalog-sync` runs one now).

## 3. Rebuild the node with a new identity (the affected household)

Do not restore the old key. A backup made before the compromise still holds
the compromised key, so restoring it would bring the stolen key back.

1. Stop the node and move its data directory aside. Keep it for
   investigation, but do not reuse it.
2. Start the node with an empty data directory. It generates a new key, a new
   node ID, and a new admin token.
3. Check the Jellyfin service user. If the host was compromised, change the
   service user's password in Jellyfin and in `JELLYMESH_JELLYFIN_PASSWORD`,
   and review its library access.
4. Publish the node's libraries again:

   ```bash
   jellymesh libraries
   jellymesh publish <library-id> -root /media/movies
   ```

## 4. Re-enroll (any member invites, an owner or administrator approves)

The rebuilt node joins like any new node, with a fresh invitation and fresh
approval (conformance C-PO-5). An old grant is never reinstated.

```bash
# On an existing member's node
jellymesh invite

# On the rebuilt node
jellymesh join -address <inviter-address> -code <short-code>

# On the owner's or an administrator's node
jellymesh requests               # confirm the fingerprint out of band with the household
jellymesh approve <inviter-id> <invitation-id>
```

Confirm the new node's fingerprint with its household over a channel the
attacker does not control, such as a phone call, before approving.

## 5. Clean up

- Lift any local blocks of the old node ID. It is no longer a member, so a
  block serves no purpose, and the new node has a different ID.
- Take a fresh backup of the rebuilt node (`jellymesh backup`), and destroy
  backups that contain the compromised key.
- Review the audit log on the affected nodes for actions attributed to the old
  node ID.

## What this does not undo

Ejection stops future access. It cannot erase metadata or media the
compromised node had already fetched while it was trusted.
