# firecracker snapshot/template path — known blockers

Status as of 2026-04-19. This branch (`feat/firecracker-snapshot-template`) got
the plumbing working end-to-end — templates are built on first run, the cache
is keyed and flocked, fd lifecycle on the tap is correct, and a second pod
landing on the same node successfully restores from a template. But the
memory/startup wins the design promises are **not realized** in a k8s
environment, due to two architectural issues discovered during smoke testing
in GKE.

**Do not roll this out to a production pool in its current shape.**

## Blocker 1 — cache key is per-pod, not per-workload

`snapshot.ComputeKey` hashes `GuestCmdLine` verbatim. In k8s, the cmdline we
hand to firecracker always contains pod-specific fields:

```
Unikraft  env.vars=[ PATH=... HOSTNAME=<pod-name>
                     KUBERNETES_PORT_443_TCP=tcp://<kube-svc-ip>:443
                     KUBERNETES_SERVICE_HOST=<kube-svc-ip> ... ]
netdev.ip=<pod-IP>/24:<gw>:<dns>
vfs.fstab=[ "initrd0:/:extract:::" ]
-- /usr/bin/workerd serve ...
```

`HOSTNAME` and `netdev.ip` change for every pod. The `KUBERNETES_*` service
env vars are stable per cluster but depend on the cluster's service CIDR. Net
effect: every pod computes a unique cache key, every pod takes the BUILD path,
no pod ever hits the cache.

Observed in smoke test (`js-sandbox-urunc-snapshot` deployment, 9 replicas
across 3 nodes): 9 pods → 9 distinct `/var/lib/urunc/snapshots/<key>/`
directories. Zero cache hits, zero memory sharing, zero startup win.

### Fix sketch (not implemented)

Normalize the cmdline before hashing: strip everything a pod can vary, keep
the stable program + fstab. A reasonable stripping pass:

- Drop `env.vars=[ ... ]` entirely, or keep only an allowlist of keys that
  should participate in cache identity (PATH).
- Drop `netdev.ip=...`.
- Keep `vfs.fstab=[ ... ]`, the boot command (`-- /usr/bin/workerd ...`), and
  the unikernel binary sha (already hashed separately).

The original `ExecArgs.Command` carries the unstripped cmdline — fine for
actually booting firecracker; the cache key should take a sibling
`StableCmdLine` field that urunc computes once it has the ukernel spec.

## Blocker 2 — guest IP is baked into the snapshot, breaks restore-path routing

This is the deeper issue. A firecracker snapshot captures guest memory, which
includes lwip's `netif` state: the configured IP address, netmask, gateway,
ARP table. On restore, firecracker's `network_overrides` lets you change the
host-side tap name, but there is no API to change the guest-side IP — the
guest resumes believing it has whatever IP was in the snapshot.

If we solve blocker 1 so multiple pods share one template, they all resume
with the template's IP. Their CNI-assigned pod IPs differ from the template
IP. Consequences:

- Inbound packets (dst = pod CNI IP) arrive at the pod's netns tap. Guest
  lwip rejects them because dst ≠ its `netif` IP.
- Outbound packets leave with src = template IP. CNI expects src = pod IP;
  kube-proxy / iptables will drop or mis-SNAT them.

The standard production answers for this are:

1. **NAT-in-netns** — urunc sets up iptables/tc rules in each pod's netns
   that rewrite src/dst IPs at the tap boundary (CNI IP ↔ template IP).
   Guest sees the template IP forever; kernel/lwip never learn the truth.
   Transparent to workerd (listens on 0.0.0.0). ~50 lines of netlink +
   netfilter setup per pod. Production-realistic, scoped.
2. **Guest-cooperative re-init** — Unikraft gains a post-resume hook that
   reads new network config from vsock and calls
   `netif_set_ipaddr()` / re-binds listeners. Template snapshots *before*
   the TCP listener binds. Cleaner design, but requires Unikraft + workerd
   patches.
3. **Per-pod templates** (current state) — no sharing. Not viable.

Neither (1) nor (2) is implemented on this branch.

## Blocker 3 — page-cache sharing vs KSM

Even if blockers 1 & 2 are fixed, the memory win this design targets
(`MAP_PRIVATE` on the shared mem file → clean pages shared via host page
cache) is competing with KSM, which dedups anon pages across cold-booted VMs
without requiring any of the snapshot/template machinery. For workerd
unikernels the likely-shared pages (kernel text, zero pages, static data) are
identical content across cold-booted pods too, so KSM gets most of the same
win with zero code and zero IP-routing problems.

Recommendation: validate KSM savings on the existing cold-boot path before
investing further here.

## What works on this branch

- End-to-end firecracker API-driven boot on a unix domain socket, replacing
  `syscall.Exec --no-api --config-file` on the snapshot path.
- Cache layout, flock-based serialization of concurrent builders, atomic
  tmp+rename of vmstate/mem/manifest.
- `com.urunc.unikernel.snapshot.enable` annotation end-to-end from pod yaml
  through containerd (`container_annotations` + `pod_annotations` forwarding)
  through urunc spec-annotation overlay onto the image's urunc.json.
- Firecracker supervise process model (urunc reexec stays resident as the
  parent of fc, SIGKILL on reexec kills fc via `Pdeathsig`).
- Tap fd close in `createTapDevice` so fc can reopen the persistent tap on
  the supervised path (cold boot was covered by `syscall.Exec` closing fds
  automatically).
- Sun-path-length-safe API socket under `/run/urunc-fc-<cid-prefix>.sock`.
- Smoke-tested in GKE: first pod builds a 512 MiB template + 24 KB vmstate
  in ~2.3s, resume succeeds, `/healthz` would serve if routing worked.

## Resuming this work

If you pick this up: fix blocker 1 first (it's small and unblocks
measurement), then pick (1) or (2) for blocker 2 based on how much Unikraft
patching you're willing to do. Don't enable `snapshot.enable=true` on the
base `js-sandbox-urunc` pool until one of those is in place — pods will fail
to serve traffic.
