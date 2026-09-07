# Grout + Systemd Mode Support Plan

The Go controller already handles both axes (`--datapath=grout` and `--mode=host`) independently. All work is in systemd deployment artifacts, FRR config, deploy script, and CI.

## 1. New grout container quadlet

Create `systemdmode/quadlets/grout.container` attached to `routerpod.pod`. It must:
- Run grout inside the router's network namespace (`nsenter --net=/var/run/netns/perouter grout --test-mode --socket-mode 0666`)
- Mount the grout socket directory as a shared volume
- Start before FRR (FRR's `dplane_grout` module connects to the grout socket)

Reference: K8s grout sidecar definition in `charts/openperouter/templates/router.yaml` and `config/grout/router_patch.yaml`.

## 2. Grout-specific FRR daemons file

Create `systemdmode/frrconfig/daemons.grout` (or a templating mechanism) with zebra options including `-M dplane_grout -K 60`:
```
zebra_options="  -A 127.0.0.1 -s 90000000 --limit-fds 100000 -K 60 -M dplane_grout"
```

Reference: `config/grout/frr_cm_patch.yaml:47`.

## 3. Controller quadlet grout variant

The deploy script (`systemdmode/deploy.sh`) copies every `*.container` file from `systemdmode/quadlets/` verbatim. Since quadlet files are plain INI with no templating, the cleanest approach is to have the deploy script apply `sed` patches to the copied files when `DATAPATH=grout` is set. This mirrors how the K8s kustomize overlay patches the base manifests (`config/grout/controller_patch.yaml`).

Concretely, `deploy.sh` should — after copying the quadlet files to the node — apply these changes when `DATAPATH=grout`:

**controller.container** — append grout flags to the `Exec=` line and add the grout socket volume:
```bash
# Append --datapath=grout and --grout-socket to the Exec line
sed -i 's|^Exec=.*|& --datapath=grout --grout-socket=/var/run/grout/grout.sock|' "$QUADLET_DIR/controller.container"

# Add grout socket volume mount (after the openvswitch volume line)
sed -i '/^Volume=.*openvswitch/a Volume=/var/run/openperouter/grout:/var/run/grout:rshared' "$QUADLET_DIR/controller.container"
```

**frr.container** — add `GROUT_SOCK_PATH` env var and grout socket volume:
```bash
# Add grout socket env var
sed -i '/^Environment=TINI_SUBREAPER/a Environment=GROUT_SOCK_PATH=/var/run/grout/grout.sock' "$QUADLET_DIR/frr.container"

# Add grout socket volume mount
sed -i '/^Volume=frr-sockets/a Volume=/var/run/openperouter/grout:/var/run/grout:rshared' "$QUADLET_DIR/frr.container"
```

**FRR daemons file** — swap in the grout variant:
```bash
# Replace the daemons file with the grout version (which adds -M dplane_grout -K 60 to zebra_options)
# The grout daemons file lives at systemdmode/frrconfig/daemons.grout
cp "$SCRIPT_DIR/frrconfig/daemons.grout" <destination for daemons file on node>
```

This avoids maintaining a full parallel set of quadlet files. The base files remain the source of truth, and grout mode is a small, auditable set of `sed` patches — analogous to kustomize patches but for INI files.

Reference: `config/grout/controller_patch.yaml:17-18`.

## 4. Grout socket volume mounts

Add volume mounts for the grout socket directory to:
- `controller.container` — controller runs `grcli` commands via the socket
- `frr.container` — FRR's `dplane_grout` module connects to grout via the socket
- `grout.container` — grout creates the socket here

All three containers share a host directory (e.g., `/var/run/openperouter/grout`).

## 5. `GROUT_SOCK_PATH` env var in FRR container

Add `Environment=GROUT_SOCK_PATH=/var/run/grout/grout.sock` to `frr.container` when running in grout mode.

Reference: `charts/openperouter/templates/router.yaml:165-167`.

## 6. Deploy script updates

Update `systemdmode/deploy.sh` to:
- Accept a flag or env var to select grout mode (e.g., `--datapath=grout` or `DATAPATH=grout`)
- Load the grout router image (`router:main-grout`) instead of the standard one
- Copy grout-specific quadlet files and FRR daemons file
- Deploy the `grout.container` quadlet

## 7. Makefile target

Add a `grout-deploy-hostmode` target (or similar) that invokes the deploy script with grout mode enabled.

## 8. CI lane

Add a `systemdmode-grout` entry to the CI matrix in `.github/workflows/ci.yaml:232` that exercises the grout + systemd combination end-to-end.

## Implementation approach

The simplest approach is to maintain separate grout-variant quadlet files (like kustomize does with patch overlays) rather than templating a single set. This keeps the quadlets straightforward and mirrors the existing pattern where `config/grout/` overlays patch the base manifests.

Files to create:
- `systemdmode/quadlets/grout.container`
- `systemdmode/frrconfig/daemons.grout`
- `systemdmode/quadlets/controller.container.grout` (or patch the existing one)

Files to modify:
- `systemdmode/quadlets/frr.container` (add grout socket volume mount + env var, or create grout variant)
- `systemdmode/deploy.sh`
- `Makefile`
- `.github/workflows/ci.yaml`
