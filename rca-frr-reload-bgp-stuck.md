# Root Cause Analysis: BGP session permanently stuck after frr-reload.py incremental apply

## Branch

`us/grout-l3vni`

## Symptom

E2E tests on the `us/grout-l3vni` branch fail flakily — every CI run fails, but a **different** test fails each time. All failures are at `e2etests/tests/validate_session.go:89`: a BGP session never reaches `Established` state within the 5-minute timeout.

The specific failure investigated was in run 31397945499 (commit 9b3622e3), test "Underlay explicit address family configuration / peers with the tor and exchanges address families ipv4unicast and ipv6unicast", on `pe-kind-control-plane`.

## Root Cause

The controller reconcile loop (`internal/controller/routerconfiguration/reconcile.go`) applies FRR config first (lines 120-128), then configures the grout datapath (line 130). These two steps are **serialized** — there is no race between them.

The bug is inside the FRR reload step itself. `frr-reload.py` applies configuration **incrementally**, command by command, via vtysh. When a reconcile cycle does a clean-then-redeploy (which happens during e2e tests when one RouterConfiguration is deleted and another is applied), the sequence is:

### Reconcile #1 — CleanAll (delete old RouterConfiguration)

1. FRR config is written with `no router bgp 64514` (empty config, removing all BGP).
2. `frr-reload.py` runs, removes the BGP instance from bgpd.
3. Grout cleanup runs: deletes `u_toswitch1` TAP port, restores kernel interface `toswitch1`.

### Reconcile #2 — deploy new underlay config

1. FRR config is written with full BGP config (router bgp + neighbors + address-family activations).
2. `frr-reload.py` starts applying commands incrementally:
   - First: `router bgp 64514`
   - Then: `neighbor 192.168.11.2 remote-as 64612` (neighbor now exists in bgpd)
   - **At this point** (~14:51:17.135): a zebra event (connected route, interface notification from the previous grout cleanup, or an internal timer) triggers `BGP_Start` on the neighbor. bgpd rejects it: **"No AFI/SAFI activated for peer"** → **"Trying to start suppressed peer"**. The peer FSM enters a broken Idle/suppressed state.
   - Later: `address-family ipv4 unicast` / `neighbor 192.168.11.2 activate` — these commands succeed in FRR's config model, but bgpd's FSM for this peer is already stuck.
3. `frr-reload.py` reports "reload successful" (~14:51:17.222).
4. Grout setup runs (serial, after reload): creates new `u_toswitch1` TAP port, interface comes UP, addresses are assigned.
5. bgpd tries to connect (BGP_Start, Idle→Connect), TCP connects succeed, but **`nexthop_set failed, update_if: (None)`** — the nexthop tracking for this peer was never properly initialized because the peer entered the suppressed state before the interface existed.

### Why it never recovers

After grout finishes and `u_toswitch1` is UP with the correct addresses, bgpd retries every few seconds but always hits the same `nexthop_set failed, update_if: (None)` error. 348 consecutive failures from 14:51:18 to 14:57:00 (the entire 5-minute test timeout). The peer is permanently stuck.

### Why it's flaky

The timing of whether a kernel/zebra event triggers `BGP_Start` during the tiny window between `neighbor ... remote-as` and `neighbor ... activate` inside frr-reload.py depends on CI load and scheduling. Some nodes hit it, some don't, explaining why different tests fail each run.

## Timeline (pe-kind-control-plane, controller-l276h / router-n4n2s)

```
14:51:14.362  controller   start reconcile (CleanAll), underlays=[]
14:51:14.365  controller   FRR config written (empty, no router bgp)
14:51:15.126  controller   frr-reload.py completes
14:51:15.127  controller   grout cleanup: delete u_toswitch1, restore toswitch1
14:51:15.416  controller   end reconcile #1

14:51:16.848  controller   start reconcile #2 (deploy underlay with ipv4+ipv6 unicast)
14:51:16.850  controller   FRR config written (full BGP config), reload requested
14:51:17.135  bgpd         BGP_Start -> "No AFI/SAFI activated for peer" -> "suppressed peer"
14:51:17.222  controller   frr-reload.py reports "reload successful"
14:51:17.223  controller   grout setup starts (serial, after FRR reload)
14:51:17.251  controller   creates new grout port u_toswitch1 (ifindex 112)
14:51:17.336  zebra        u_toswitch1(112) DOWN
14:51:17.353  zebra        192.168.11.3/24 added to u_toswitch1(112)
14:51:17.463  zebra        u_toswitch1(112) UP
14:51:17.593  controller   end reconcile #2

14:51:18.193  bgpd         first "nexthop_set failed, update_if: (None)" -- never recovers
14:51:18-14:57:00  bgpd    348 identical nexthop_set failures
```

## Relevant Code

- `internal/controller/routerconfiguration/reconcile.go` — serialized FRR reload (line 120-128) then datapath (line 130)
- `internal/frr/config.go` — FRR config generation, NeighborConfig, ActivateFor()
- `internal/frr/templates/frr.tmpl` — main FRR config template
- `internal/frr/templates/neighboripfamily.tmpl` — per-neighbor address-family activation template
- `internal/grout/underlay.go` — SetupUnderlay (line 33), configureUnderlayPort (line 163), RestoreUnderlay (line 105)
- `internal/grout/grout_client.go` — ensurePort, ensureVXLAN, ensureVRF

## Fix Direction

The core issue is that `frr-reload.py`'s incremental apply creates a transient window where a BGP neighbor exists without any activated address family, and zebra events during that window permanently break the peer FSM.

Possible approaches:

1. **Ensure the grout interface doesn't exist during FRR reload**: before running frr-reload.py, tear down the TAP interface so there's no connected route to trigger zebra events. Recreate it after reload. Downside: more churn, brief connectivity loss on every reconcile.

2. **Use `clear bgp *` after reload**: after frr-reload.py completes and grout setup finishes, issue `vtysh -c "clear bgp *"` to reset all BGP sessions. This forces bgpd to re-evaluate all peers from scratch with the correct config and interfaces in place. This is the simplest fix.

3. **Reorder frr-reload.py commands**: modify how the FRR config template generates the config so that address-family activation appears immediately after neighbor creation (or use `neighbor ... activate` in the same config block). This is fragile and depends on frr-reload.py internals.

4. **Use FRR's `bgp suppress-fib-pending`**: configure FRR to not start peers until FIB is ready. May not solve the root cause.

Approach 2 is the most pragmatic. The `clear bgp *` should be issued **after both** the FRR reload and the grout datapath setup are complete, so all interfaces are UP and all config is applied before sessions are re-initiated.

## Task

Implement a fix for the flaky BGP session establishment. After investigating the approaches above, apply the most appropriate one. Add the BGP session reset at the right point in the reconcile loop. Make sure to verify that the fix doesn't break other test scenarios (e.g., steady-state reconciles where sessions are already established should not be unnecessarily disrupted).
