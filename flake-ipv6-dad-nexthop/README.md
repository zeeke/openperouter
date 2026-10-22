# Evidence: IPv6 DAD / link-local next hop flake

Source: CI run 40372949861, job `e2etests (operator)` (121754595408), artifact `kind-logs-operator`.
All files are trimmed excerpts with ANSI codes stripped.

| File | What it proves |
|---|---|
| `ISSUE.md` | Ready-to-paste issue body (first line is the title). |
| `01-ginkgo-failure.log` | The failing spec and error: blue IPv6 prefixes on `frr-k8s-daemon-tclk9` have only link-local next hops; red is correct. |
| `02-router-frr-blue-timeline.log` | `router-fbv56` FRR log, 18:43:34–18:43:42, for `pe-e-200`, `2001:db8:2::1`, `192.169.11.4` and `2001:db8:2::4`. Shows the veth being recreated, the address skipped as tentative, the IPv4 session reaching Established at 18:43:41.557, and DAD completing 53 ms later at 18:43:41.610. |
| `03-router-neighbor-nexthops.txt` | Router per-peer next hops. The IPv4 blue peer `192.169.11.4` has `Nexthop global: fe80::74fd…`, while the IPv6 blue peer and both red peers have the correct global next hop. |
| `04-frrk8s-tclk9-bgp-ipv6-table.txt` | frr-k8s IPv6 BGP table and RIB. Blue IPv6 routes are installed via the link-local next hop. |
| `05-router-ip-address-pe-e-200.txt` | At dump time `pe-e-200` does have `2001:db8:2::1/64`, so the address arrived and FRR simply never refreshed the session's next hop. |
| `06-router-frr-red-timeline.log` | Control case: in VRF red, DAD finished (18:43:38.986) before the sessions came up (18:43:39.385), so red got the correct next hop. |
