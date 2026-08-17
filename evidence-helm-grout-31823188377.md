# Evidence: helm-grout lane failure matches frr-reload BGP stuck RCA

## CI Run

- **Run:** https://github.com/openperouter/openperouter/actions/runs/31823188377
- **Job:** `e2etests (helm-grout)` (job ID 94842530845)
- **Branch:** `us/grout-l3vni`
- **Commit:** `de988580`
- **PR:** #635
- **Date:** 2026-08-14

## Failed Test

- **Name:** `Underlay explicit address family configuration / peers with the tor and exchanges address families ipv4unicast and ipv6unicast`
- **Defined at:** `e2etests/tests/sessions.go:1070`
- **Failed at:** `e2etests/tests/validate_session.go:89`

## Pod mapping (pe-kind-control-plane)

- **Router pod:** `openperouter-router-987n8` (containers: frr, reloader, grout)
- **Controller pod:** `openperouter-controller-2q4wr`

## Artifact logs

Downloaded via:
```bash
gh api repos/openperouter/openperouter/actions/artifacts/9228646678/zip > helm-grout-logs.zip
```

The failing test's logs are under the directory:
```
Underlay_explicit_address_family_configuration_peers_with_the_tor_and_exchanges_address_families_ipv4unicast_and_ipv6unicast/
```

Router pod log file: `openperouter-system_openperouter-router-987n8_pods_logs.log`
Controller pod log file: `openperouter-system_openperouter-controller-2q4wr_pods_logs.log`

Line numbers below are from `cat -n <file>`.

## Evidence from CI console log

Console log source: `gh api repos/openperouter/openperouter/actions/jobs/94842530845/logs`

Console line numbers are from that command piped through `cat -n`.

---

### Step 1: Prior test removes the underlay (clean phase)

**Console** (line 3842):
```
STEP: waiting for all router pods to be ready after removing the underlay  @ 08/14/26 17:38:20.057
```

**Controller** (`openperouter-controller-2q4wr`, lines 1705-1714):
```
17:38:20.021  start reconcile  controller=RouterConfiguration  request=openperouter-system/underlay
17:38:20.022  reloading FRR config  event="cleaning the frr configuration"
17:38:20.022  frr generate config  event=start
17:38:20.024  updater requesting update  socket=/etc/frr/reload.sock
17:38:21.003  updater update requested
17:38:21.003  frr generate config  event=stop
```

**Reloader** (`openperouter-router-987n8`, lines 28316-28320):
```
17:38:20.027  reload handler  event="received request"
17:38:20.209  frr update succeeded  action=test
               Lines To Delete: no router bgp 64512
               Lines To Add: ip nht resolve-via-default, ipv6 nht resolve-via-default
17:38:21.002  frr update succeeded  action=reload
17:38:21.003  reload handler  event="reload successful"
```

**Controller — grout cleanup** (`openperouter-controller-2q4wr`, lines 1716-1764):
```
17:38:21.005  underlay removed, cleaning up VNIs and underlay interfaces
17:38:21.667  deleting grout address  addr=192.168.11.3/24  iface=u_toswitch1
17:38:21.700  deleting grout port  name=u_toswitch1
17:38:21.538  deleting grout address  addr=192.168.12.3/24  iface=u_toswitch2
17:38:21.584  deleting grout port  name=u_toswitch2
17:38:21.798  end reconcile  request=openperouter-system/underlay
```

---

### Step 2: Reconcile #2 — deploy new underlay with explicit address families

**Console** (lines 3845-3847):
```
Underlay explicit address family configuration  peers with the tor and exchanges
  address families ipv4unicast and ipv6unicast
STEP: deploying an underlay with both ToR neighbors with address families
  ipv4unicast and ipv6unicast  @ 08/14/26 17:38:20.171
```

**Controller — FRR config write + reload request** (`openperouter-controller-2q4wr`, lines 1765-1773):
```
17:38:21.798  start reconcile  controller=RouterConfiguration  request=openperouter-system/red
17:38:21.802  frr generate config  event=start
17:38:21.803  updater writing frr file  file=/etc/frr/frr.conf
17:38:21.803  updater requesting update  socket=/etc/frr/reload.sock
17:38:22.143  updater update requested
17:38:22.143  frr generate config  event=stop
```

**Reloader — frr-reload.py incremental apply** (`openperouter-router-987n8`, lines 28321-28325):

The `--test` output (line 28323) shows the exact incremental command order — **neighbor creation BEFORE address-family activation**:
```
Lines To Add:
  router bgp 64514                                    ← BGP instance created
  neighbor 192.168.11.2 remote-as 64512               ← neighbor exists NOW
  neighbor 192.168.12.2 remote-as 64513               ← neighbor exists NOW
  ...
  address-family ipv4 unicast                         ← activation LATER
    neighbor 192.168.11.2 activate                    ← too late
    neighbor 192.168.12.2 activate
  address-family ipv6 unicast
    neighbor 192.168.11.2 activate
    neighbor 192.168.12.2 activate
```

```
17:38:21.803  reload handler  event="received request"
17:38:21.912  frr update succeeded  action=test   (shows incremental plan above)
17:38:22.143  frr update succeeded  action=reload
17:38:22.143  reload handler  event="reload successful"
```

**FRR (bgpd) — suppressed peer** (`openperouter-router-987n8`, lines 15331-15341):

During `frr-reload.py`'s incremental apply, a zebra event triggers `BGP_Start` on the neighbors **before address-family activation completes**:
```
17:38:22.056 BGP: 192.168.11.2 [FSM] BGP_Start (Idle->Connect), fd -1 for Outgoing
17:38:22.056 BGP: 192.168.11.2 [FSM] Trying to start suppressed peer
               - this is never supposed to happen!
17:38:22.056 BGP: 192.168.11.2 [FSM] Failure handling event BGP_Start in state Idle,
               prior events (null), (null), fd -1,
               last reset: No AFI/SAFI activated for peer

17:38:22.057 BGP: 192.168.12.2 [FSM] BGP_Start (Idle->Connect), fd -1 for Outgoing
17:38:22.057 BGP: 192.168.12.2 [FSM] Trying to start suppressed peer
               - this is never supposed to happen!
17:38:22.057 BGP: 192.168.12.2 [FSM] Failure handling event BGP_Start in state Idle,
               prior events (null), (null), fd -1,
               last reset: No AFI/SAFI activated for peer
```

---

### Step 3: Controller sets up grout datapath (after FRR reload completes)

**Controller — grout setup** (`openperouter-controller-2q4wr`, lines 1775-1811):
```
17:38:22.144  configure interface start  namespace=/var/run/netns/perouter
17:38:22.144  setting up underlay
17:38:22.177  creating grout port  name=u_toswitch1
17:38:22.241  assigning IP to grout port  iface=u_toswitch1  cidr=192.168.11.3/24
17:38:22.258  migrated underlay address to grout  cidr=192.168.11.3/24  iface=u_toswitch1
17:38:22.292  creating grout port  name=u_toswitch2
17:38:22.332  assigning IP to grout port  iface=u_toswitch2  cidr=192.168.12.3/24
17:38:22.365  migrated underlay address to grout  cidr=192.168.12.3/24  iface=u_toswitch2
17:38:22.392  setup underlay done
17:38:22.456  configure interface end
17:38:22.456  end reconcile  request=openperouter-system/red
```

---

### Step 4: BGP sessions permanently stuck — nexthop_set failures

**FRR (bgpd) — first nexthop_set failures** (`openperouter-router-987n8`, lines 15720-15728):
```
17:38:23.781 BGP: 192.168.12.2: nexthop_set failed,
               local: 192.168.12.3:50804 remote: 192.168.12.2:179
               update_if: (None) resetting connection - intf u_toswitch2
17:38:23.783 BGP: 192.168.11.2: nexthop_set failed,
               local: 192.168.11.3:43420 remote: 192.168.11.2:179
               update_if: (None) resetting connection - intf u_toswitch1
```

**FRR (bgpd) — last nexthop_set failure, 340 total** (`openperouter-router-987n8`, line 28129):
```
17:44:04.280 BGP: 192.168.11.2: nexthop_set failed,
               local: 192.168.11.3:179 remote: 192.168.11.2:45820
               update_if: (None) resetting connection - intf u_toswitch1
```

Total `nexthop_set failed` for 192.168.11.2: **340 consecutive failures** from 17:38:23 to 17:44:04 (the entire 5-minute test timeout).

---

### Step 5: Test times out

**Console** (lines 3848-3862):
```
STEP: validating TOR session from clab-kind-leafkind1 to pe-kind-control-plane
  (ID: 192.168.11.3) and network layer protocols [ipv4 unicast ipv6 unicast]
  @ 08/14/26 17:38:21.49

[FAILED] Timed out after 300.000s.
Unexpected error:
    neighbor clab-kind-leafkind1 to pe-kind-control-plane - 192.168.11.3
    is not established
```

### Step 6: All other tests pass

**Console** (lines 4069-4076):
```
Summarizing 1 Failure:
  [FAIL] Underlay explicit address family configuration
         [It] peers with the tor and exchanges address families
         ipv4unicast and ipv6unicast
  /home/runner/work/openperouter/openperouter/e2etests/tests/validate_session.go:89

Ran 35 of 187 Specs in 811.289 seconds
FAIL! -- 34 Passed | 1 Failed | 0 Pending | 152 Skipped
```

The BFD test that runs immediately after (line 3867) passes, confirming the issue is specific to the clean-then-redeploy timing, not general infrastructure.

---

## Match with RCA

| Criterion | RCA (run 31397945499) | This run (31823188377) |
|---|---|---|
| Failed test | `Underlay explicit address family configuration / peers with the tor and exchanges address families ipv4unicast and ipv6unicast` | **Same** |
| Failure location | `validate_session.go:89` | **Same** |
| Node | `pe-kind-control-plane` | **Same** |
| Neighbor IP | `192.168.11.3` | **Same** |
| Symptom | BGP session never reaches Established within timeout | **Same** |
| Trigger | Clean-then-redeploy: prior test deletes underlay, this test creates new one | **Same** |
| bgpd: "No AFI/SAFI activated for peer" | Yes | **Yes** (router-987n8:15333) |
| bgpd: "Trying to start suppressed peer" | Yes | **Yes** (router-987n8:15332) |
| bgpd: "nexthop_set failed, update_if: (None)" | Yes (348 failures) | **Yes** (340 failures) |
| frr-reload.py: neighbor before activate | Yes | **Yes** (router-987n8:28323) |
| Other tests | Pass | **Same** (34 passed, 1 failed) |

## Why `clear bgp *` would fix this

The `clear bgp *` fix (in `cmd/reloader/main.go:164`) runs immediately after `frr-reload.py` completes, resetting all peer FSMs. This clears the suppressed state caused by the incremental-apply race. When the grout datapath subsequently creates `u_toswitch1` (17:38:22.177) and it comes UP, bgpd re-evaluates the peer with the full config (neighbor + address-family) already in place and establishes normally.

## How to verify

```bash
# Fetch the job logs
gh api repos/openperouter/openperouter/actions/jobs/94842530845/logs > /tmp/helm-grout.log

# Console log lines (line numbers from `cat -n /tmp/helm-grout.log`)
cat -n /tmp/helm-grout.log | sed -n '3842p'         # prior cleanup
cat -n /tmp/helm-grout.log | sed -n '3845,3847p'    # failing test starts
cat -n /tmp/helm-grout.log | sed -n '3848p'         # session validation begins
cat -n /tmp/helm-grout.log | sed -n '3849p'         # FAILED at validate_session.go:89
cat -n /tmp/helm-grout.log | sed -n '3855,3862p'    # timeout + error message
cat -n /tmp/helm-grout.log | sed -n '4069,4076p'    # final summary

# Download artifact logs
gh api repos/openperouter/openperouter/actions/artifacts/9228646678/zip > /tmp/helm-grout-logs.zip
unzip /tmp/helm-grout-logs.zip -d /tmp/helm-grout-logs
TESTDIR="Underlay_explicit_address_family_configuration_peers_with_the_tor_and_exchanges_address_families_ipv4unicast_and_ipv6unicast"
ROUTER="/tmp/helm-grout-logs/$TESTDIR/openperouter-system_openperouter-router-987n8_pods_logs.log"
CONTROLLER="/tmp/helm-grout-logs/$TESTDIR/openperouter-system_openperouter-controller-2q4wr_pods_logs.log"

# Controller: reconcile #1 (clean) and #2 (deploy)
cat -n "$CONTROLLER" | sed -n '1705,1714p'   # reconcile #1: FRR clean
cat -n "$CONTROLLER" | sed -n '1716,1764p'   # reconcile #1: grout cleanup
cat -n "$CONTROLLER" | sed -n '1765,1773p'   # reconcile #2: FRR deploy
cat -n "$CONTROLLER" | sed -n '1775,1821p'   # reconcile #2: grout setup

# Reloader: frr-reload.py incremental apply showing neighbor-before-activate
cat -n "$ROUTER" | sed -n '28321,28325p'

# FRR bgpd: suppressed peer
cat -n "$ROUTER" | sed -n '15331,15341p'

# FRR bgpd: first and last nexthop_set failures
cat -n "$ROUTER" | sed -n '15720p'           # first for 192.168.12.2
cat -n "$ROUTER" | sed -n '15728p'           # first for 192.168.11.2
grep -c "192.168.11.2.*nexthop_set failed" "$ROUTER"  # total count (340)
```
