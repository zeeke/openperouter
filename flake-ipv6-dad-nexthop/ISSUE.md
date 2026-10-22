Flaky e2e: frr-k8s gets link-local-only IPv6 next hop on host session when BGP comes up during IPv6 DAD

## Summary

`Routes with RT between bgp and the fabric with vnis_rt and frr-k8s [It] translates EVPN incoming routes as BGP routes`
fails intermittently. The router advertises IPv6 prefixes to the host (frr-k8s) with a **link-local address as the
global next hop**, instead of the router's VNI host address (`2001:db8:2::1`).

This happens when the **IPv4** host session reaches Established while the router-side veth's global IPv6 address is
still **tentative** (Duplicate Address Detection in progress). FRR records the peer's IPv6 next hop when the session is
established and does not refresh it once the global address becomes usable, so the session keeps advertising the
link-local address until it is reset.

It is a real (if transient) data-plane issue, not only a test problem: the host receives IPv6 routes whose "global"
next hop is link-local.

## Where

- CI run: https://github.com/openperouter/openperouter/actions/runs/40372949861
- Job `e2etests (operator)`: https://github.com/openperouter/openperouter/actions/runs/40372949861/job/121754595408?pr=906
- PR: #906 (the change is grout/PCI only and not exercised by this job; the failure is unrelated to it)
- Test: `e2etests/tests/route_target.go:196`, failing assertion at `route_target.go:282`
  (`checkBGPPrefixesForHostSession` for VRF blue, IPv6)
- Node: `pe-kind-worker2`, router pod `router-fbv56`, frr-k8s pod `frr-k8s-daemon-tclk9`

## Observed error

(full snippet: `01-ginkgo-failure.log`)

```
[FAILED] Timed out after 240.001s.
IPv6 prefix 2001:db8:21::/64 with nexthop 2001:db8:2::1 not found in routes map[
  2001:db8:20::/64:[2001:db8:1::1 fe80::b456:acff:fe4e:3ac9]            <- red: global + link-local (OK)
  2001:db8:21::/64:[fe80::74fd:eeff:fef9:12d2 fe80::74fd:eeff:fef9:12d2] <- blue: link-local twice
  ...] for pod frr-k8s-daemon-tclk9
```

Every blue IPv6 prefix on that node has the same problem, including the `2001:db8:2::{2,3,4}/128` host addresses.
Red is fine.

## Timeline (router-fbv56, VRF blue, `pe-e-200`)

(from `02-router-frr-blue-timeline.log`)

| Time (UTC) | Event |
|---|---|
| 18:43:34.692 | Previous test cleanup deletes `pe-e-200` (ifindex 155) |
| 18:43:39.536 | Controller recreates `pe-e-200` (ifindex 161), enslaves it to VRF blue |
| 18:43:39.550 | zebra: `Tentative addr 2001:db8:2::1/64 on UP interface pe-e-200, skipping` |
| 18:43:40.555 | Incoming IPv4 session from `192.169.11.4`: `nexthop_set failed ... resetting connection` (no IPv6 address at all yet) |
| 18:43:40.651 | Link-local `fe80::74fd:eeff:fef9:12d2` becomes usable |
| **18:43:41.557** | **IPv4 session `192.169.11.4` Established**: global IPv6 still tentative, so FRR falls back to link-local as the global next hop |
| **18:43:41.610** | **DAD completes**: `ZEBRA_INTERFACE_ADDRESS_ADD 2001:db8:2::1/64` (53 ms later) |
| 18:43:41.624 | IPv6 session `2001:db8:2::4` Established with the correct global next hop |

The resulting per-peer next hops on the router (`03-router-neighbor-nexthops.txt`):

```
BGP neighbor is 192.169.11.4     Nexthop global: fe80::74fd:eeff:fef9:12d2   <- stuck link-local
BGP neighbor is 2001:db8:2::4    Nexthop global: 2001:db8:2::1               <- correct
```

The interface does have the global address at dump time (`05-router-ip-address-pe-e-200.txt`). FRR just never
reconsidered it for the already-established IPv4 session.

For comparison, in VRF red the same recreation happened, but DAD finished (18:43:38.986) **before** the sessions came
up (18:43:39.385 / 18:43:39.817), so red got the correct next hop (`06-router-frr-red-timeline.log`).

## Why the test never recovers

1. The router enables both IPv4 and IPv6 unicast on both the IPv4 (`192.169.11.4`) and IPv6 (`2001:db8:2::4`) host
   neighbors, so frr-k8s receives every IPv6 prefix twice.
2. The two paths tie on all attributes. For eBGP, FRR prefers the **oldest** path, which is the one from the IPv4 session
   (established ~67 ms earlier). See `04-frrk8s-tclk9-bgp-ipv6-table.txt`: the RIB installs blue IPv6 routes via
   `fe80::74fd…`.
3. `parseRoutes` (`e2etests/pkg/frr/bgp.go:179`) only collects next hops of the **best** path, so it sees only
   `[fe80…, fe80…]`.
4. Nothing resets the session afterwards, so the 4-minute `Eventually` cannot succeed.

## Why it's flaky

It only fails when frr-k8s reconnects the IPv4 host session inside the DAD window (~1–2 s) right after the controller
recreates the `pe-e-<vni>` veth, which happens on every test that tears down and re-creates the L3VNIs. Which VRF/node
loses the race is random.

## Root cause

The host-side veth IPv6 address is added with plain `netlink.AddrAdd` (`internal/hostnetwork/link_properties.go`,
`AssignIPToInterface`), so the kernel runs DAD and the address is tentative for a while after creation. No
`IFA_F_NODAD` / `accept_dad=0` is used anywhere under `internal/`. Combined with FRR's behaviour of fixing the
advertised IPv6 next hop at session establishment, a session that comes up during DAD advertises a link-local "global"
next hop for its whole lifetime.

## Proposed fix

Add the IPv6 address on the point-to-point host veth with `IFA_F_NODAD`:

```go
addr.Flags |= unix.IFA_F_NODAD // IPv6 only
```

The veth pair is created and owned by OpenPERouter at both ends, so DAD protects nothing there, and with this flag the
address is usable as soon as it is added. This removes the race.

## Alternatives considered

- **Relax the e2e check** to accept any valid path rather than only the best path. This hides the symptom: the host
  still receives a link-local-only next hop from the IPv4 session.
- **Don't cross-activate address families** (activate IPv6 unicast only on the IPv6 host neighbor and IPv4 only on the
  IPv4 one). This changes the generated FRR config and the API behaviour, so it's a larger change.
- **Wait for DAD before configuring FRR** (poll until the address loses `IFA_F_TENTATIVE`). This works but adds latency
  and complexity compared to `IFA_F_NODAD`.

## Evidence

All excerpts were taken from the `kind-logs-operator` artifact of the run above (attached folder):

- `01-ginkgo-failure.log`
- `02-router-frr-blue-timeline.log`
- `03-router-neighbor-nexthops.txt`
- `04-frrk8s-tclk9-bgp-ipv6-table.txt`
- `05-router-ip-address-pe-e-200.txt`
- `06-router-frr-red-timeline.log`
