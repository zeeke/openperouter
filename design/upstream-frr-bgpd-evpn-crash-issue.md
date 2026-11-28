# Draft: upstream FRR issue — bgpd crash in bgp_config_write_evpn_info after
# hidden-instance resurrection

This document contains everything needed to file the issue upstream at
https://github.com/FRRouting/frr/issues. It is **not** meant to be merged; it
is a scratch pad for the filing session.

**Status update**: the bug is already reported upstream in
FRRouting/frr#23835, with open fixes in FRRouting/frr#23851 and
FRRouting/frr#24569 (only one needs to merge). FRRouting/frr#24213 is a
related crash on the same path, already fixed on master and stable/10.8.
The material below is still useful as review input for those fix PRs: the
exact crash site, the core-dump evidence, and the repro.

## Suggested title

bgpd: NULL dereference in bgp_config_write_evpn_info when a hidden default
instance is resurrected by "router bgp" with the same ASN

## Summary

FRR 10.7.1 bgpd segfaults (SIGSEGV, si_addr 0x20) in
`bgp_config_write_evpn_info()` while servicing a vtysh `show running-config`,
when the written BGP instance was previously deleted and then recreated with
the same ASN.

The deleted instance is not freed when VPN routes are still present in the
default instance RIB: `bgp_delete()` marks it `BGP_FLAG_INSTANCE_HIDDEN`
instead. While doing so it also runs `bgp_evpn_cleanup()`, which frees
`bgp->evpn_info`. When `router bgp <same ASN>` is configured again,
`bgp_lookup_by_as_name_type()` resurrects the hidden instance through
`bgp_create(..., hidden=true)`, which skips `bgp_evpn_init()` and then clears
the hidden flag. The instance is back in `bm->bgp` with `evpn_info == NULL`,
and the next config write dereferences it.

## Environment where the crash was observed

- FRR 10.7.1, x86_64, CentOS Stream 10, running under watchfrr with the
  integrated config file (daemons: zebra, bgpd, isisd, staticd, bfdd, mgmtd).
  The binary is the `frr` RPM from the grout v0.20.1 release
  (https://github.com/DPDK/grout/releases/tag/v0.20.1), which is vanilla FRR
  10.7.1 plus the grout zebra module; the crash is in unmodified bgpd code.
- The configuration is managed live by `frr-reload.py`, which runs
  `show running-config` on every invocation (both `--test` and `--reload`).
  Any other periodic `show running-config` (monitoring, config management)
  triggers the crash the same way.
- The BGP default instance peers with fabric leaves that advertise ipv4/ip6
  VPN routes, so `bgp->rib[AFI_IP][SAFI_MPLS_VPN]` is non-empty, and it has
  the l2vpn evpn address family configured (`address-family l2vpn evpn`,
  `advertise-all-vni`).

## Crash evidence

FRR's own signal handler logged (timestamps are UTC):

```
2026/11/26 10:58:34 WATCHFRR: [VTVCM-Y2NW3] Configuration Read in Took: 00:00:00
BGP: Received signal 11 at 1795690714 (si_addr 0x20, PC 0x635bc47ea601); aborting...
BGP: /usr/lib64/frr/libfrr.so.0(zlog_backtrace_sigsafe+0x73) [0x703810e85553]
BGP: /usr/lib64/frr/libfrr.so.0(zlog_signal+0xf9) [0x703810e85759]
BGP: /usr/lib64/frr/libfrr.so.0(+0xbb214) [0x703810ec6214]
BGP: /lib64/libc.so.6(+0x41140) [0x703810b79140]
BGP: /usr/libexec/frr/bgpd(bgp_config_write_evpn_info+0x41) [0x635bc47ea601]
BGP: /usr/libexec/frr/bgpd(+0x14ba03) [0x635bc48bea03]
BGP: /usr/libexec/frr/bgpd(bgp_config_write+0x16cc) [0x635bc48c07fc]
BGP: /usr/lib64/frr/libfrr.so.0(+0x47daa) [0x703810e52daa]
BGP: /usr/lib64/frr/libfrr.so.0(+0x48355) [0x703810e53355]
BGP: /usr/lib64/frr/libfrr.so.0(+0x49ca0) [0x703810e54ca0]
BGP: /usr/lib64/frr/libfrr.so.0(cmd_execute_command+0xd5) [0x703810e54e15]
BGP: /usr/lib64/frr/libfrr.so.0(cmd_execute+0xd8) [0x703810e55028]
BGP: /usr/lib64/frr/libfrr.so.0(+0xd6ff7) [0x703810ee1ff7]
BGP: /usr/lib64/frr/libfrr.so.0(+0xd7621) [0x703810ee2621]
BGP: /usr/lib64/frr/libfrr.so.0(+0xd8a32) [0x703810ee3a32]
BGP: /usr/lib64/frr/libfrr.so.0(event_call+0x96) [0x703810edb956]
BGP: /usr/lib64/frr/libfrr.so.0(frr_run+0xc8) [0x703810e7fdc8]
BGP: /usr/libexec/frr/bgpd(main+0x406) [0x635bc47a6146]
BGP: /lib64/libc.so.6(+0x2a58e) [0x703810b6258e]
```

i.e. a vtysh command (`show running-config`, arriving through `cmd_execute`)
walking `bgp_config_write()` into `bgp_config_write_evpn_info()`.

Crash PCs from core dumps of three different bgpd processes (all crashing
within seconds of each other after the same config change) resolve to the
same instruction. Disassembly of the function start in the shipped bgpd
(`bgp_config_write_evpn_info` is at ELF vaddr 0x775c0):

```
775db:  mov    0x2464(%rsi),%r9d     ; rsi = bgp (2nd arg)
775f1:  test   %r9d,%r9d
775f4:  jne    ...                   ; bgp->advertise_all_vni
775fa:  mov    0x2450(%rbx),%rdi     ; rbx = bgp; rdi = bgp->evpn_info
77601:  cmpq   $0x0,0x20(%rdi)       ; <-- crash: bgp->evpn_info->soo
```

`si_addr 0x20` matches `0x20(%rdi)` with `rdi == NULL`: the load of
`bgp->evpn_info` returned NULL and the very first dereference faulted. The
offset corresponds to the `bgp->evpn_info->soo` check near the top of the
function (source below). Registers in the core are clobbered by libfrr's
`core_handler` re-raise (tgkill), but the handler-stored faulting PC and
`si_addr` are conclusive, and the logged backtrace above confirms the symbol.

Raw artifacts (kind-logs with core dumps, pod logs, reloader logs) are kept
in this GitHub run, downloadable as `kind-logs-helm-grout` /
`kind-logs-operator-grout`:
https://github.com/zeeke/openperouter/actions/runs/45802100754

## Root cause

All references below are to the `frr-10.7.1` tag; line numbers for current
`master` @ a5410f6145e3 (2026-11-26) are given in brackets. The code is
unchanged between the two.

1. `router bgp 65000` on the default instance makes it the EVPN owner:
   `bgp_create()` sets `bm->bgp_evpn = bgp` (bgpd/bgpd.c:3791 [master:4045]).

2. `no router bgp 65000` while VPN routes exist in the default instance RIB:
   `bgp_delete()` runs `bgp_evpn_cleanup(bgp)` because `bm->bgp_evpn == bgp`
   (bgpd/bgpd.c:4663-4680 [master:4948-4964]), which frees `bgp->evpn_info`
   (bgpd/bgp_evpn.c:7884-7887), and then, because
   `bgp_table_top(bgp->rib[AFI_IP][SAFI_MPLS_VPN])` is non-empty, marks the
   instance hidden instead of freeing it (bgpd/bgpd.c:4518-4525
   [master:4799-4806]):

   ```c
   if (bgp->inst_type == BGP_INSTANCE_TYPE_DEFAULT &&
       (bgp_table_top(bgp->rib[AFI_IP][SAFI_MPLS_VPN]) ||
        bgp_table_top(bgp->rib[AFI_IP6][SAFI_MPLS_VPN]))) {
       ...
       SET_FLAG(bgp->flags, BGP_FLAG_INSTANCE_HIDDEN);
   }
   ```

   The hidden instance stays in `bm->bgp` with `evpn_info == NULL`.

3. `router bgp 65000` again: `bgp_lookup_by_as_name_type()` finds the hidden
   instance and resurrects it (bgpd/bgpd.c:4134-4135, 4166-4168
   [master:4415-4416, 4446-4448]):

   ```c
   if (IS_BGP_INSTANCE_HIDDEN(bgp) && *as != AS_UNSPECIFIED)
       hidden = true;
   ...
   if (hidden)
       bgp_create(as, name, inst_type, as_pretty, asnotation,
                  bgp, hidden);
   ...
   UNSET_FLAG(bgp->flags, BGP_FLAG_INSTANCE_HIDDEN);
   ```

   The hidden path in `bgp_create()` reuses the old struct and jumps past the
   initialization block (bgpd/bgpd.c:3773-3774 [master:4027-4028]):

   ```c
   if (hidden)
       goto peer_init;
   ```

   skipping `bgp_evpn_init(bgp)` (bgpd/bgpd.c:3960 [master:4221]), the only
   place `bgp->evpn_info` is allocated (bgpd/bgp_evpn.c:7923). So the
   resurrected instance is live and un-hidden, with `evpn_info == NULL`.

4. Re-applying `address-family l2vpn evpn` re-enables the EVPN address
   family on the resurrected instance. The next `show running-config` walks
   `bm->bgp`; the hidden-instance filter does not apply because the flag was
   cleared (bgpd/bgp_vty.c:22015-22017 [master:22109-22111]):

   ```c
   /* skip hidden default vrf bgp instance */
   if (IS_BGP_INSTANCE_HIDDEN(bgp))
       continue;
   ```

   and reaches (bgpd/bgp_vty.c:21872-21873 [master:22537]):

   ```c
   if (safi == SAFI_EVPN)
       bgp_config_write_evpn_info(vty, bgp, afi, safi);
   ```

   whose first uses of `evpn_info` are unchecked
   (bgpd/bgp_evpn_vty.c:7640-7646 [master:8254-8260]):

   ```c
   if (bgp->evpn_info->advertise_svi_macip)
           vty_out(vty, "  advertise-svi-ip\n");

   if (bgp->evpn_info->soo) {
           char *ecom_str;

           ecom_str = ecommunity_ecom2str(bgp->evpn_info->soo,
   ```

   -> SIGSEGV at `bgp->evpn_info + 0x20`.

Note the hidden path in `bgp_create()` also skips `bgp_pbr_init()`,
`bgp_srv6_init()` and `bgp_ls_init()`; whichever of those were torn down by
`bgp_delete()` are equally stale after a resurrection. The EVPN one is just
the one that crashes on a plain `show running-config`.

## Reproduction

Observed sequence (kubernetes e2e environment, config pushed with
frr-reload.py, so each step is a live config change, and `show running-config`
runs between steps):

1. Default instance with EVPN AF and vpnv4-capable neighbors established;
   fabric neighbors advertise ipv4 vpn routes, so the default instance RIB
   has VPN entries.
2. Live-apply `no router bgp 65000` (whole instance removal).
3. Live-apply `router bgp 65000` (+ `address-family l2vpn evpn`) again.
4. Run `vtysh -c 'show running-config'` -> bgpd SIGSEGV.

A minimal two-node repro should be: node A peers with node B over eBGP with
`address-family ipv4 vpn` activated and advertises VPN routes; on node B
configure `router bgp` with `address-family l2vpn evpn`, then `no router bgp`,
then `router bgp` + `address-family l2vpn evpn`, then `show running-config`.
[Not minimized yet — the filing session should verify this in a container
lab before submitting, e.g. two frr containers back to back. Single-node with
locally leaked VPN routes may also work if rib[AFI_IP][SAFI_MPLS_VPN] can be
populated without a peer; unverified.]

Impact in the environment where this was found: bgpd is restarted by watchfrr
in the middle of config churn on every underlay remove/re-add cycle, and any
config-management tool that reads the running config (frr-reload, monitoring)
crashes it again right after. With `debug bgp zebra` on, the resurrection
sequence is visible in the log: neighbors report "connection rejected ...
not configured" (instance gone/hidden) and, after the next daemon restart,
"Creating Default VRF, AS ..." (fresh instance, non-hidden path).

## Suggested fix directions

- Primary: when `bgp_lookup_by_as_name_type()` resurrects a hidden instance,
  re-run the initialization that `bgp_create(hidden=true)` skips — at least
  `bgp_evpn_init()` (allocating `evpn_info`), and audit `bgp_pbr_init()`,
  `bgp_srv6_init()`, `bgp_ls_init()` against what `bgp_delete()` cleaned for
  the hidden instance. The flag is cleared as part of the resurrection, so
  the instance must be brought back to a state equivalent to a fresh
  `bgp_create()`.
- Defense in depth: NULL-guard `bgp->evpn_info` in
  `bgp_config_write_evpn_info()` (skip the EVPN extras when NULL). This stops
  the crash on `show running-config` but leaves the instance without EVPN
  data, so it is not a complete fix on its own.

## Related but distinct FRR issues

- #22851: NULL deref in `bgp_evpn_local_vni_add()` when zebra reports a VNI
  and no EVPN instance exists. Same pointer family, different path.
- #23835: 10.7.1 ships the `bgp_dump_attr` crash fixed in #23087 (debug bgp
  updates). Unrelated crash, but the same "10.7.1 ships known bgpd crashes"
  theme.

## Filing checklist for the session

- [ ] Verify/minimize the repro in a two-container lab (see above).
- [ ] Re-check that master still carries the bug (it did at
      a5410f6145e3, 2026-11-26).
- [ ] Attach: the zlog_signal backtrace, the disassembly snippet, and the
      core dump if practical (73 MB, available from the linked run).
- [ ] Mention versions: verified on 10.7.1 (release RPM); source inspection
      shows the same code on master.
