# Hosted control plane

Use a modern operator-controlled host. Keep customer-facing Node ingress separate from the administrator API and management networks. The current binary has two listeners: `-listen` for Node HTTPS and `-admin-listen` for loopback-only administration. If terminating TLS at a proxy, expose only Node ingress. Do not publish the private administrator port through that proxy.

Choose the server's run account, configuration/state directories, TLS termination, DNS and firewall configuration for the actual hosting environment. Operator credentials and signing keys remain on this side. Run `tack` on the control host or through explicitly authorized administrator access.

Customer nodes are untrusted even after authentication. Each node credential authorizes only that node's synchronization. A captured node credential must not grant a shell on the hosted server, administrator API access, another customer's identity, arbitrary HTTP proxying or access to controller infrastructure through a Chisel tunnel. The hosted Chisel relay needs its own restricted credentials, destination ACLs and network boundary. The unified Node holds a separate node-specific Chisel credential; management credentials do not authorize tunnels. Revocation must cover both services independently.

The first stage uses static random node tokens and process-local route isolation. Production rollout additionally requires the certificate lifecycle, customer authorization, rate limits and service/network separation in the design. A compromised node can falsify its own results. Do not use self-reported hostnames, paths, labels or capabilities as privileged deployment instructions.

Never promise that software-only local encryption hides a credential from the Node's own account or root. Use protected files/ACLs against lower-privilege access. Hardware-backed non-exportable keys are an optional future identity provider; a compromised original device may still invoke them. Native CentOS 6, MIPS and minimal NAS targets cannot be assumed to have TPM support.

Record how to add and revoke nodes. In the current implementation `tack provision` updates controller configuration, and the Controller must restart to load changes. Remove a node entry and restart to revoke it. Preserve the task state file. Revocation prevents further synchronization; it cannot remotely stop an already disconnected local process.

For automation operators, use scoped credentials through the private listener; `operator-set` and `operator-revoke` take effect for new requests without a Controller restart. Preserve stable operator IDs across token rotation and back up the matching `.operators.json` store with task state. See [task operations](../../../docs/task-operations.md) for scope, retry and recovery semantics. Node credential provisioning/revocation still uses the restart workflow above.
