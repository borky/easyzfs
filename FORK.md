# What this fork changes, and why it is not upstream

This branch (`develop`) is upstream's `main` plus the patches below. Keep this
file current: it is what tells you, months later, which divergence was
deliberate.

## Branches

| branch | meaning |
|---|---|
| `main` | a mirror of upstream (`gnacho/easyzfs`). Fast-forward only, never commit to it. |
| `develop` | the product: upstream plus the patches below. |
| `feature/<name>` | new work meant for upstream, cut from `main` so it can become a PR unchanged. |

## Syncing with upstream

```sh
git fetch upstream
git checkout main && git merge --ff-only upstream/main && git push origin main
git rebase upstream/main develop        # then: git push --force-with-lease origin develop
```

Before rebasing, read what upstream added. EasyZFS runs as a service that can
elevate to root, so every sync gets the same two checks the original audit did:

```sh
base=$(git merge-base origin/develop upstream/main)   # the develop you last pushed
git diff --stat "$base" upstream/main -- deploy/ internal/ main.go
pat='https?://[a-zA-Z0-9.%/_?=&:-]+'
git grep -hoE "$pat" "$base"       -- '*.go' '*.ts' '*.tsx' ':!*_test.go' | sort -u > /tmp/ez-before
git grep -hoE "$pat" upstream/main -- '*.go' '*.ts' '*.tsx' ':!*_test.go' | sort -u > /tmp/ez-after
diff /tmp/ez-before /tmp/ez-after
```

- Anything under `deploy/` (the sudoers file, the `easyzfs-sysd` root helper,
  the units) or `internal/executil` / `internal/actions` changes what can run
  as root: read it, don't skim it.
- A new URL is a new outbound destination. What it sends, whether it is on by
  default, whether it identifies the installation. A URL assembled at runtime
  (the alert channels build theirs from configuration) will not show up in
  this diff, so for anything touching `http.` read the code.
- `landing/` is the marketing site and is never embedded in the binary; its
  trackers do not reach the NAS.

Then `go vet ./... && go test -race ./...`, `cd web && npx tsc -b --noEmit &&
npm run build`, and only then push.

## Fork-only patches

All but the last came out of a security audit of upstream at `00b2e27`
(v2.9.23). None was found to be malicious; these are defects. They are kept
here rather than offered upstream by the owner's decision, which also means
**upstream installs still carry them** — the first one in particular.

| patch | what it fixes |
|---|---|
| `fix(sysd)`: confine the root helper to regular files directly in `/etc/cron.d` | `valid_cron_file()` accepted `/etc/cron.d/../../anywhere` (a bash `case` glob `*` matches `/`) and checked only the basename. Chained with `cron-to-timer`, which writes the file's line into `ExecStart=` and starts the unit, the unprivileged service account could run any command as root. Sudoers pins the helper's path but not its arguments, so its own validation is the whole boundary. The first version of this fix compared resolved paths and was still beatable with a symlink swapped between check and read; the parent is now compared literally and symlinks are refused. |
| `fix(2fa)`: stop regenerating recovery codes on a GET, and on every panel open | `GET /api/me/2fa/recovery` destroyed and reissued the codes, outside the CSRF, rate and demo guards; and the Settings panel called it on every mount, so opening Settings silently invalidated the codes the user had saved. |
| `fix(api)`: redact the webhook URL for non-admins | `GET /api/settings` is not admin-only and returned the webhook URL, usually a capability URL with a secret, to plain users and to read-only API keys. |
| `fix(actions)`: allowlist explicit mountpoints, on clone and on set | `zfs clone -o mountpoint=` was unvalidated, and the properties endpoint's regex accepted `/etc`. Either could mount a dataset over `/etc` as root. A denylist kept missing places (`/var/run` → `/run`, `/opt` hiding `/opt/easyzfs`, `/var/lib/dpkg`), so explicit mountpoints are now limited to the pool's own tree (`/<pool>`, or wherever its root dataset is actually mounted, never `/`) and below `/mnt`, `/media`, `/srv`, `/home`, with no symlinks on the way and every directory passed through root-owned and not writable by others. The *effective* mountpoint, the one nobody asks for, is the row below. **Behaviour change:** a mountpoint outside those trees (say `/data`) has to be set from the CLI. |
| `fix(actions)`: check the mountpoint a dataset actually gets | §1, and the reason a Proxmox host that matters had to run read-only until now. A dataset also gets a mountpoint without anybody setting one: inherited from its parent, received in a stream, or derived from the pool's name — and that is what ZFS mounts on, at creation and at every import after it. Nothing looked at it. On the test VM (root on `rpool/ROOT/pve-1`, mounted at `/`) creating a dataset whose inherited mountpoint resolved into `/etc` left the host with no network after its next reboot. Now every path that can end in a mount resolves the real one first (`zfs get mountpoint`, unprivileged) and refuses a system path or one that cannot be reached safely, wherever it came from; a path the app is *deriving* must also land inside the pool's own tree, `/mnt`, `/media`, `/srv`, `/home`, or the mountpoint of the nearest ancestor that has one recorded, with an ancestor at `/` granting nothing. A path recorded on the dataset itself (`local`, `received`) skips that last rule — it is already what ZFS will mount at, EasyZFS or not — so Proxmox's own `rpool/var-lib-vz` at `/var/lib/vz` keeps working. Covered: create, clone without a mountpoint, rename, mount, promote (only the always-on rules; promote mounts nothing), inheriting `mountpoint`, pool create (`/<name>`, and `rePool` allows `etc`), pool import, restoring from the recycle bin, and a local replication destination. The effective path is walked for symlinks but never for owners, unlike a mountpoint somebody asks for: a share directory `chown`-ed to its users, or left group-writable, is the ordinary setup, and demanding root ownership all the way would refuse every dataset created, imported or restored below one — paths that had no check at all before. **Residual limits:** for a recorded path the system-path list is the whole defence, and a denylist always misses something; and without the ownership rule the symlink swap between check and mount is still open to anyone who can write to a directory on the way. **Behaviour changes:** `zpool import` is now `-N` plus a checked `zfs mount` per dataset (and `zfs share` for the datasets that ask for it, which `zfs mount` alone does not do), and it returns the ones it left unmounted; `/mnt`, `/media`, `/srv` and `/home` are refused as a mountpoint the app *derives*, so a pool cannot be called `home` (a dataset recorded at `/home`, as root-on-ZFS installs have, still mounts). The mounting `zpool import <pool>` is gone from the sudoers file. |
| `fix(auth)`: bound `/api/login` | The limiter map grew by one uncollectable entry per request under fresh usernames, with the username (up to 1 MiB) embedded in each key, and entries were created before the argon2 semaphore, so an anonymous client could exhaust a 256 MB service. Usernames over 64 bytes are refused, admission is capped at 32, and the purge works. A client holding all 32 slots still starves other logins, as before; it can no longer take the process down. |
| `fix(encryption)`: verify the current passphrase before change-key | `current_key` was checked for being non-empty and then ignored, so any admin session could re-wrap a dataset to a key its owner does not know. Now verified with a dry-run `zfs load-key -n`. Datasets with `keyformat=raw` (the UI never creates them) can no longer change key through the API. The "incorrect key" detection relies on OpenZFS's English error wording and has not been checked against a real host; if it differs, the change is still refused, with a generic error. |
| `fix(install)`: make the installer's prompts answer what they show | `confirm()` returned its default as an exit status, where 0 means yes, so every text prompt was inverted: Enter at `[s/N]` answered yes. At the uninstall prompt that deleted `/var/lib/easyzfs` and `/etc/easyzfs` (never the pools). Text prompts are what the documented `curl … \| bash` install always uses, since stdin is the pipe. `--yes` now takes the displayed default, so the "ZFS not detected, continue?" prompts abort under `--yes` instead of continuing. **Upstream installs still have this.** |
| `fix(install)`: helper source, weekly updater, dry run, uninstall | Under `curl … \| bash` the script's own directory resolved to the **current directory**, so an `easyzfs-sysd` left there by someone else was installed as the root helper; otherwise it was fetched unverified from the moving `main` branch. It now comes from the checkout, `--source` or next to `--binary`, else from the exact release tag the binary was downloaded from. The weekly-updater step aborted the installer (undefined `NEW_VERSION`, relative `cp`, no `sudo`); it now installs only for a release binary, since for a local or source build it would replace that build with upstream's release every week. Also: `DRY_RUN=1` crashed, uninstall left the root update units and `/opt/easyzfs` behind, and `hostname -I` killed the installer on Arch after a good install. **Upstream installs still have all of these.** |
| `feat(update)`, `feat(install)`: install and update from this checkout only | See "Installing this fork". Also fixed on the way: `--source` was ignored in favour of downloading upstream's release (the default `--url` was checked first); a reinstall dropped hand-added env lines; the CSP still let the browser reach `api.github.com`, which nothing uses. |
| `feat(updater)`: `EASYZFS_NO_UPDATE_CHECK` | Upstream checks `api.github.com` at boot and every 24 h with no opt-out. Not a defect — it sends nothing — but a recurring outbound call nobody agreed to. Does not affect the separate weekly auto-update timer `install.sh` can install. Also stops the Settings icon claiming "up to date" when no check ever succeeded. |
| `CLAUDE.md`, this file | Working notes for this clone. Upstream keeps AI tooling out of its history. |

### From the Proxmox VE analysis

A second review (`easyzfs_security_proxmox_analysis.md`, kept out of the repo)
asked what EasyZFS can do to a Proxmox host whose root is on ZFS. Its findings
were reproduced on a Proxmox VE 8.4 test VM (ZFS 2.2.7, root on
`rpool/ROOT/pve-1`, UEFI + Secure Boot) before and after each fix. The §
numbers are that document's.

| patch | § | what it fixes |
|---|---|---|
| `fix(install)`: leave a Proxmox host's packages alone | 14 | `apt-get update` exits 100 on the enterprise repos without a subscription, and the old dependency step partially upgraded ZFS (2.2.7 → 2.2.10 userland under a 2.2.7 kernel module). The installer now installs only missing packages with `--no-upgrade`, never touches ZFS packages on PVE, and prints what it found (PVE version, root pool, Secure Boot) before changing anything. On the test VM the only package it added was `sudo`, which PVE lacks. |
| `fix(install)`: ask where the web UI listens | 3 | It listened on every interface. The installer now asks (localhost / one address / all) and keeps the answer on reinstall; `--listen` sets it non-interactively. |
| `fix(api)`: check the Origin of every mutation by default | 4 | `CSRF_CHECK` was off unless set; it is now on unless set to `0`. |
| `feat`: read-only mode | 5, 20 | `install.sh --read-only` (`EASYZFS_READONLY=1`): the sudoers file grants only four reads (`smartctl -j -a`, `zpool events -f`, `zpool history -i`, `zfs diff -FHt`, plus `crontab -l`), no root helper is installed, and the API refuses every storage mutation with 403 `read_only`. Even a compromised service cannot change a pool. The scheduler and replication do not start. **This is the recommended mode on a Proxmox host that holds anything important.** |
| `fix(longops)`: read output to the end before waiting | — | Found while testing: `cmd.Wait` ran before the output reader finished, a race that lost the tail of a long operation's log (and failed `-race`). |
| `fix(disks)`: check a disk live before ZFS takes it over | 7, 8, 9 | The disk view called an LVM physical volume and a disk with an EFI system partition "free", and pool create / vdev add / replace / RAID-Z expand / power-off trusted the cache. Each now re-reads the disk with `lsblk` at the moment of the request and refuses (409 `dev_in_use`, with the reason) anything mounted, swap, LVM, LUKS, mdadm, Ceph, an ESP or BIOS-boot partition, a ZFS label, a filesystem, or a device-mapper holder. The view shows the same reason. Fixtures are real `lsblk` output from the VM. |
| `fix(props)`: refuse `checksum=off`, and make high-impact changes ask first | 6 | `checksum=off` and `fletcher2` are gone from the allowlist. `sync=disabled`, `copies`, `readonly=on`, `canmount`, `mountpoint`, `exec/setuid/devices=off`, a shrinking `volsize` and inheriting any of those return 409 `risk_ack_required` with an explanation until the request carries `acknowledge_risk`; the UI shows the explanation and asks. |
| `fix(ci)`: make the TypeScript check check something | — | `web/tsconfig.json` has `"files": []`, so `tsc --noEmit` checked nothing. CI and the docs now run `tsc -b --noEmit`. |
| `feat(auth)`: ask for the password again before irreversible operations | 5, 20 | Pool create/export, vdev add/offline/detach, replace, RAID-Z expand, checkpoint, dataset and snapshot destroy, rollback, change-key, user creation and deletion (a new admin account would answer every later prompt), API key creation, a `volsize` change (shrinking destroys data), password and 2FA reset, backup import (which replaces every account), and a replication job with `force_full` (20 routes; the `force_full` check decodes the body exactly as the handler does, and asks when it cannot) now need `reauth_password` (for the backup upload, in `X-Reauth-*` headers), and the TOTP code when 2FA is on (recovery codes are not accepted: each confirmation would burn one). Answers are 403, never 401, so a wrong password does not log the user out. Wrong answers count toward the login limiter; right ones do not (`fix(auth)`: do not spend login attempts…, which fixed a 429 on the sixth deletion in a row). |
| `fix(replication)`: run send\|recv as separate processes | 12 | `bash -c "zfs send … \| zfs recv …"`: safe only because of the input whitelists, and broken without root, since sudoers grants `zfs`, not `bash`. Now two processes joined by a pipe (`longops.StartPipeline`), each under its own `sudo`; cancelling kills both process groups. Verified on the VM as the `easyzfs` user. |
| `fix(sysd)`: migrate a cron job to a timer that runs it the way cron did | 13 | `cron-to-timer` wrote the command straight into `ExecStart=`, which systemd does not run through a shell: pipes, redirects, `;`, `$VAR` and cron's `%` all changed meaning. The unit now runs `SHELL -c "<command>"` with cron's `PATH`, `User=` always set, `%`, `$` and `\` escaped for systemd, and an unescaped `%` (cron's stdin marker) refused. Output checked identical to cron's under real systemd on the VM. |
| `fix(updater)`: verify a staged update as root before installing it | 10 | `easyzfs-update.service` ran `install` as root on whatever the service account had left in `$DATA_DIR/update`. The updater now records the staged release tag beside the binary (and keeps the pair through rollback). The root helper `deploy/easyzfs-apply-update` copies the binary without following symlinks, fetches that tag's `checksums.txt` from GitHub itself, and installs only a match. It is not in upstream's release tags, so without a checkout next to the installer the `.path` unit is not installed. Only matters for a `github`-channel build; the default `local` build has no updater. |
| `fix(sudoers)`: pin every zfs/zpool argument | 2 | See below. |

A review of the commits above found, and these fixed:

| patch | what it fixes |
|---|---|
| `fix(auth)`: close two ways past re-authentication | Creating a user did not re-authenticate, so a hijacked session could add an admin and answer every later prompt itself. The `force_full` predicate read the body case-sensitively while the handler did not: `{"FORCE_FULL":true}` skipped the prompt. |
| `fix(install)`: no chown through a symlink, no silent unpinned sudo | The service account could replace `$DATA_DIR/update` with a symlink and have the installer hand the target (e.g. `/etc/systemd/system`) to it. On sudo < 1.9.10 the root-equivalent fallback grant now needs `--allow-unpinned-sudo`. |
| `fix(longops)`: SIGTERM, and report how it ended | SIGKILL to a process group killed `sudo` but left its root child running: a cancelled replication kept sending, and a timed-out `zpool history` stayed orphaned (reproduced on the VM; after the fix, cancelling a live `zfs send \| zfs recv` leaves nothing). An operation that finishes despite a cancel is reported done, so replication moves its bookmark. |
| `fix(disks)`: power-off of exported pools, same-bay replace | Power-off refused an exported pool's disks, which is what one powers off to pull; it now refuses only active use (imported pool members by live LABEL, mounts, swap, LVM/LUKS/RAID/Ceph, boot partitions). Replace skipped the live check when both names resolved to one device; it now runs and accepts only this pool's own label. `lsblk` before util-linux 2.37 is handled. |
| `fix(disks)`: a collector for disk use | `GET /api/disks` ran `lsblk` in the handler. |
| `fix(auth)`: current-password guesses; volsize; prompts | `/api/me/password` now uses the login limiter and answers 403, not 401 (a typo logged the user out). Concurrent prompts no longer strand a request. |
| `fix(sysd)`, `fix(sudoers)` | Migrated cron jobs run in the user's home, as under cron. `lsblk` pinned too. |

**§2, root helpers.** The analysis proposed one root helper per operation. The
same boundary is drawn with sudoers instead: `pinned_sudoers` in
`deploy/install.sh` writes one whole-argument regex per command shape the
service runs (42 rules), and `deploy/easyzfs.sudoers` is generated from it.
`zfs program`, `zfs allow`, `zpool import -d`, file vdevs, `altroot`,
`zpool status -c`, `-f` on create/destroy and every flag the code never passes
are refused; a 112-case allow/deny matrix passed with real sudo on the VM, and
every operation was then run through the API under it. What remains is what
the app is for: a compromised service can still destroy pools and datasets,
and `zfs recv` into a dataset whose mountpoint it can set is root-equivalent.
Read-only mode is the answer to that. `TestSudoersPropsMatchValidators` fails
when a property is added to the allowlist but not to sudoers.

**§11, signed releases — now enforced (remediation spec P3).** A checksum
fetched next to the binary protects against tampering on the NAS, not against
a compromised release pipeline, so the root helper now also requires
`checksums.txt.minisig` and verifies it with `minisign -V` against the public
key embedded in `deploy/easyzfs-apply-update` (`TRUSTED_MINISIGN_KEY`). It
fails closed: no key embedded, no minisign installed, no signature published,
a bad one, or a checksum that is not the one signed — nothing is installed and
the binary in use is left as it was. The key is empty in this fork and
upstream publishes no signatures, so a `github`-channel build never
self-updates; this fork's builds are on the `local` channel anyway and update
with `make update`. To publish signed releases, sign `checksums.txt` with
`minisign -S` and put the public key in the helper.

The helper also leaves the reason for a refusal in
`$DATA_DIR/update/apply-refused`, and `GET /api/update/status` returns it as
`applyRefused`: without it Settings and the update wizard kept showing a
refused update as one waiting for its restart. The service account owns that
directory, so the file is created with `dd conv=excl` (O_EXCL fails on any
existing path, symlinks included — bash's noclobber would write through a
symlink to a FIFO or a disk), and the staged files are read with
`nofollow,nonblock` so a planted FIFO cannot hang root. A refusal older than
the running daemon is dropped as stale (a plain restart or reboot after a
refusal also drops it; the update then shows as available again, and the
next attempt reports the refusal again). The replaced binary is kept as
`/usr/local/bin/easyzfs.prev`. The weekly timer's script used to install a release
as root on its checksum alone — a second, weaker path around the helper. It now
only downloads, and hands the binary to the helper; the installer no longer
fetches upstream's copy of that script (which still does the old thing),
installs the timer only when the helper has a trusted key, and otherwise
removes any weekly timer and script already there. `minisign` is
installed with the update units. Tests (`internal/updater/applyhelper_test.go`,
with a stub minisign): no key, no minisign, unsigned, wrong key, garbage
signature, checksums swapped after signing, wrong arch, symlinked binary, a
failed install keeping the old binary, a FIFO as the staged binary refused
without blocking, a planted `apply-refused` symlink not written through, and
a stale refusal dropped.

**§1, §19.1-2 — done**, see the effective-mountpoint row above. A Proxmox host
that matters no longer needs read-only mode for this reason.

Verified on the Proxmox VE 8.4 test VM (OpenZFS 2.2.7) with the installed
binary, not only in tests:
- Refused with a readable reason, and `rpool` left unchanged: a dataset under
  `rpool/ROOT/pve-1` at `/etc` (the incident) or anywhere else under `/`;
  mounting `rpool/ROOT/pve-1`; renaming a dataset under it; and pools named
  `etc` or `home`.
- Still working: datasets and children under `rpool/data`, a child of
  `rpool/var-lib-vz`, a child of a `chown`-ed share, unmount and mount
  (including `/var/lib/vz` itself), and recycle-bin trash and restore.
- A data pool round-trip (create, datasets including one recorded at `/srv`,
  export, import) comes back fully mounted.
- Real sudo allows `zpool import -N <pool>` and `zfs share <ds>`, and refuses
  the mounting `zpool import <pool>`, `import -N -d` and `zfs share -a`.

One thing the import cannot report: on a host with no NFS server tooling, OpenZFS
2.2 marks a dataset "already shared" after a share attempt fails, so
`zfs share` answers that and the import shows no warning although no export
exists. A plain `zpool import` could not have exported it either.

**§19.6, §21 — done** (`feat(host)`): the app recognises the host's own
storage and keeps its hands off it.

- **Read from the live host, every time, never from what EasyZFS created or
  recorded.** On Proxmox the pools exist before EasyZFS is installed.
  - The installer made `rpool`, with the OS on `rpool/ROOT/pve-1`.
  - The admin made the data pools, registered as Proxmox storage.
- **OS pools** are whichever pools have a dataset mounted at `/` or `/boot`,
  from mountinfo, so no pool name is assumed. It stays view-only apart from
  maintenance (scrub, trim, clear, SMART, snapshots). Replacing one of its
  disks needs Proxmox's own procedure (partitions copied, `proxmox-boot-tool`),
  which `zpool replace` does not do.
- **System datasets** are that pool's root, its boot-environment container, and
  anything mounted outside the pool's tree (`/var/lib/vz`). They cannot be
  destroyed, renamed, unmounted, changed or rolled back.
- **Guest disks** are recognised by Proxmox's names (`vm-N-disk-N`, `subvol-…`,
  …) on any pool, and are managed from Proxmox.
- **Proxmox storage** is the `zfspool` datasets in `/etc/pve/storage.cfg` (read
  through one pinned `sudo cat`) and any dataset holding guest disks. It
  cannot be destroyed, renamed, unmounted or moved, and a pool holding it
  cannot be exported or destroyed, and only properties its guests would not
  feel can change. Its disks can still be replaced: for a data pool that is
  the repair that matters.
- **No snapshot of EasyZFS's lands on a guest disk**: `qm rollback` refuses
  while one it does not know sits there. A manual recursive snapshot over
  guest disks is refused. A scheduled job instead snapshots the rest of the
  tree in one atomic command, and still prunes, which cleans up automatic
  snapshots left on guest disks by earlier versions.
- **A review found, and these fixed:**
  - Asking for a checkpoint when adding a vdev took it *before* the guard
    refused the add, leaving one on the OS pool, where it blocks
    `zpool replace`.
  - A storage root such as `rpool/data` still accepted `exec`, `quota`,
    `sync` and key changes, all of which its guests inherit. Only harmless
    properties are allowed there now.
  - Guest names now follow Proxmox's own volume pattern: templates
    (`basevol-…`) and hand-named disks (`vm-100-mydisk`).
  - Disk work on a data pool no longer depends on listing every dataset,
    only on mountinfo.
  - Also covered now:
    - `dir:` storage, boot pools at `/boot`, and ancestors of outside mounts;
    - create or clone targets with Proxmox names or inside `rpool/ROOT`;
    - discarding an OS pool's checkpoint;
    - an unreadable `storage.cfg`, which marks every pool and top-level
      dataset as storage, is logged, and shows a banner in the UI saying what
      it blocks and how to fix it (`make update`);
    - a failed collector read, which keeps the last view instead of showing
      the host as plain data.
- Each rule is enforced in the actions, which read the host again and refuse
  when they cannot. The UI marks each item and leaves out what would be
  refused.
- Verified on the Proxmox VE 8.4 VM, with a VM disk and a container subvolume
  named as Proxmox names them, and the VM left as it was.

**Still open.**

- **Replication source.** A replication job's snapshot on a guest disk is not
  refused (replicating VM disks off-host is a real use). The runner keeps its
  two latest `ezrepl-*` snapshots on the source, so Proxmox cannot roll back a
  VM whose disk is being replicated.

### The privileged gateway (remediation spec P0, P1, P7)

`easyzfs_proxmox_remediation_requirements.md` (untracked) asked for the checks
to hold at the privileged boundary too, not only in the service. The sudoers
file had pinned argument *shapes*, but still let the service account run
`zfs set mountpoint=/etc …`, `zfs mount`, `zfs destroy` and the rest itself,
so a compromised service process could skip every Go-side check.

- **`easyzfs priv <tool> …`** (`internal/priv`) is now the only way the
  service reaches `zfs`, `zpool`, `smartctl`, `dd`, `hdparm` or `udisksctl`
  with root rights; sudoers grants it and none of the tools.
- **As root**, it matches the argv against a closed grammar
  (`internal/actions/privgate.go`), then re-runs the action's own checks right
  before the command: name whitelists, host/guest storage, effective
  mountpoints (inherited, received, `none`/`legacy`, symlinks), and live disk
  and pool state. Only then does it `exec` the tool, so exit codes, output and
  SIGTERM relay are the tool's own.
- **Service side:** `executil` routes those tools through it automatically.
  In root mode the same checks run in-process. The service keeps its own
  copies for quick, well-worded 4xx answers.
- **Verified on the Proxmox VE 8.4 VM with real sudo:**
  - the service account's direct `zfs`/`zpool`/`smartctl` calls are refused;
  - through the gateway, `mountpoint=/etc`, mounting the root filesystem,
    `zfs program`, `zpool import -d`, `status -c` and destroying `rpool` or
    `rpool/data` are refused;
  - the dataset, recycle-bin, §1 and replication-cancel scenarios all pass
    through it.
- **A review of the gateway** found, and these fixed:
  - `zfs recv` mounted whatever the stream held, setuid-root files and device
    nodes included, which was a path to root. Two hardened forms now exist
    (`actions.RecvFSArgs`/`RecvVolArgs`, which replication uses):
    - a filesystem is received unmounted, with `setuid`, `devices` and `exec`
      off and the stream's mountpoint, canmount and share settings ignored;
    - a volume is received with `volmode=dev`.

    The gateway reads the stream's own header to check which form applies,
    since `volmode` is silently ignored for a filesystem stream. It only
    receives into an existing dataset carrying `easyzfs:replica=on`, a mark
    only a receive sets; otherwise an incremental stream could plant files in
    any dataset. A replica made by an earlier version lacks the mark and needs
    `zfs set easyzfs:replica=on <dataset>` from the console, or a full resend.
  - The hardening could be undone by turning `setuid` or `devices` back on,
    by inheriting them, or by cloning the replica under a parent where they
    are on. EasyZFS now never turns either on or inherits them, and clones are
    created with both off.
  - `zfs send` streamed any snapshot to the service account. Sending from the
    running OS (`/etc/shadow`, host keys, the cluster database) is refused.
  - sudo's `priv *` glob also matches a single argument such as `"priv x"`,
    which would have started the whole daemon as root. Under sudo the binary
    now refuses anything but `priv` and the installer's flag-only queries
    (`-update-channel`, `-generate-vapid`).
  - `destroy -r` of a snapshot reached guest disks' same-named snapshots; it
    is refused (nothing uses it).
  - Clone-then-promote could carry a guest disk's snapshots away. Promote now
    checks the origin, and cloning a guest disk's snapshot is refused.
  - zvols were accepted as disks for `zpool create`/`add`/`replace`/`attach`.
    They are refused.
  - `rewrite` requires the dataset to actually be mounted at the path.
  - The recycle-bin exception only applies to a bin EasyZFS made.
  - Every command the actions build during the test suite is replayed through
    the grammar afterwards (`internal/actions/main_test.go`), so a builder
    that drifts from the grammar fails CI instead of production.
- **Still not covered:**
  - A custom `DB_PATH` data directory. The gateway runs with sudo's cleaned
    environment and protects the default `/var/lib/easyzfs` only.
  - A guest disk's contents can still be streamed with `zfs send`, since
    replicating VM disks off-host is a use this app supports.
  - The binary the gateway runs is replaced by `make update` (root) or by the
    root apply helper. The helper verifies the release checksum; requiring
    a signature too is spec item P3, not done yet.

### Proxmox guest configs (spec P2, P6)

Guest disks were recognised by Proxmox's volume names alone. Now every
node's VM and container config (`/etc/pve/nodes/*/{qemu-server,lxc}/*.conf`,
snapshot sections and `unused` disks included) is read, and each volume it
references is mapped through `storage.cfg` to its dataset. That disk is a
guest disk whatever its name, and the reason names the guest ("lo usa VM 900").

- Guests that use a dataset without a storage volume count too. A raw
  device (`scsi1: /dev/zvol/tank/vol`, `dev0:`) marks the zvol as a guest
  disk, and a linked clone (`base-100-disk-0/vm-101-disk-0`) marks the clone.
  A bind mount (`mp0: /tank/media,mp=/media`, `lxc.mount.entry`) marks the
  dataset holding that path (never the OS root) as Proxmox *storage*, not a
  guest disk: Proxmox keeps no snapshots of a bind mount, so snapshot jobs,
  rollback and property changes keep working, while destroying, renaming or
  unmounting it (or its pool) is refused. Bind paths through a symlink are
  resolved as root in the gateway. Storage ids map to datasets by id, so two
  storages on one dataset both resolve.
- Known limits: a bind path inside a legacy-mounted dataset, and a raw
  device written as `/dev/zdN`, map to no dataset.
- The files are root-only: the service asks the gateway (`priv pvecfg`),
  which reads exactly those paths and returns only `storage.cfg` and the
  volume references — not the configs, which hold cloud-init password
  hashes and SSH keys. The old `cat storage.cfg` sudo rule is gone outside
  read-only mode.
- If `/etc/pve` or any guest config cannot be read (pmxcfs down, a
  permission problem), the host is treated as unknown. Every pool and
  top-level dataset then counts as Proxmox storage, and the UI banner says so.
- Storage definitions count cluster-wide: one restricted to another node
  (`nodes`) still protects its dataset here.
- Verified on the Proxmox VE 8.4 VM with a real `qm create`'d VM: all three
  of its disks were marked with the VM and refused for deletion.
- A two-node cluster could not be tested with one VM; that behaviour comes
  from Proxmox's shared `/etc/pve`.

### Root mode is acknowledged, never silent (spec P4)

The default stays the unprivileged `easyzfs` account behind the gateway.
`--root-mode` now prints what it gives up (the gateway's checks run inside
the web-facing root process, so a bug or compromise of the service is root
on the host) and asks, defaulting to no; declining falls back to the service
account. Unattended, `--root-mode --yes` is refused before anything is done
unless `--i-understand-root-mode` is also given. Tests:
`internal/actions/rootmode_test.go` runs the real installer for the refusal
and the gate function for each decision.

### systemd hardening that keeps sudo and host mounts (spec P5)

Each directive the spec lists was tried alone on the Proxmox VE 8.4 VM
(systemd 252), in a transient unit running as `easyzfs`, checking three
things: the unit shares PID 1's mount namespace, `sudo easyzfs priv zfs
create` works, and the new dataset's mount appears in `/proc/1/mountinfo`.

| Directive | Result |
|---|---|
| `LockPersonality`, `RestrictRealtime`, `SystemCallArchitectures=native`, `RestrictSUIDSGID`, `RestrictNamespaces`, `ProtectHostname`, `ProtectClock`, `MemoryDenyWriteExecute`, `RestrictAddressFamilies` | rejected: for a non-root unit systemd implies `NoNewPrivileges` (`NoNewPrivs: 1`), and sudo refuses to run |
| `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectKernelLogs`, `ProtectControlGroups` | rejected: private mount namespace (and NNP for the first three) |
| `LimitCORE=0`, `RemoveIPC=yes`, `KeyringMode=private` | kept: same namespace, sudo works, mounts reach the host |
| `UMask=0027` | passed, not kept: the sudo'd `zfs` inherits it, and the directories it creates for mountpoints would lose world access |

`install.sh --update` adds the three kept lines to an existing unit. After
the update the live service was checked: same mount namespace as PID 1,
`NoNewPrivs: 0`, core limit 0, and the end-to-end API script (create,
mount, unmount, clone, rename, delete to the recycle bin, rollback, scrub)
ran through it. `internal/actions/unit_test.go` fails if the static unit or
the installer's template loses the three lines or gains a rejected one. The
real protection stays where it was: the gateway's checks, which run as
root outside the service.

### Secure cookie, mode sweep, proxy docs (spec P8)

- The session cookie is Secure whenever the browser reached the app over
  HTTPS: TLS on the connection, or, with `TRUST_PROXY=1`, a proxy's
  `X-Forwarded-Proto` (or, without it, `Forwarded`) saying so; configure
  the proxy to set `X-Forwarded-Proto`, which takes precedence.
  `COOKIE_SECURE=1` still forces it. The last value is the one read, the
  one the nearest (trusted) proxy wrote; earlier ones can come from the
  client.
- `DEMO=1` no longer starts the scheduler or the replication runner: the
  API refused to create jobs, but jobs already in the database would have
  run real commands. `backgroundJobs()` in `main.go` decides for both
  modes, with a test.
- `internal/httpapi/modesweep_test.go` finds every mutating route in the
  source and checks it against read-only (storage routes → 403
  `read_only`; anything that is neither storage nor an allowlisted
  app-settings route fails the test) and demo (everything but logout and
  alert ack → 403 `demo_mode`).
- README: a modes table, and reverse-proxy and firewall examples.

### Destructive actions only after the safety steps

Fork-only, built on the fixes above:

| patch | what it does |
|---|---|
| `fix(unit)`: drop the private mount namespace | `ProtectSystem`/`ProtectHome`/`PrivateTmp` gave the service its own mount namespace: datasets it mounted were invisible on the host, and ones it unmounted, renamed or destroyed stayed mounted there (destroy failed "busy"). It was kept for a while as a guard against bad mounts, but it never was one — ZFS applies a mountpoint at the next import regardless. `make update` removes the lines from an installed unit. |
| `feat(datasets)`: a recycle bin | Deleting a dataset renames it into `<pool>/easyzfs-trash` (unmounted, readonly) and destroys it after 7 days; restore puts it back with its mountpoints. The delete dialog shows space, children, snapshots and mountpoint first, and offers an explicit permanent delete. The bin dataset disappears from the pool when empty. A dataset that inherits encryption from its parent cannot leave its encryption root and must be deleted permanently. Anything still in use (a zvol open by a running VM, a filesystem a container uses, checked with `fuser`) is refused, as `destroy` refused it with "busy"; nothing trashed is made read-only. |
| `feat(pools)`: optional checkpoint before adding a vdev | A wrong `zpool add` is permanent on a RAID-Z pool; a checkpoint lets it be rewound. Opt-in, never automatic: while one exists ZFS refuses `zpool replace` and hot spares, so a forgotten one would block replacing a failed disk. |
| `feat(pools)`: redundancy warnings; rollback impact | Offline/detach on a degraded or resilvering pool, or one that would leave its own mirror or RAID-Z vdev without redundancy (judged per vdev), need an explicit acknowledgement. The rollback dialog lists the snapshots it will destroy and suggests cloning. |

A "safety snapshot before rollback" was considered and dropped: `zfs rollback -r` destroys every snapshot newer than its target, the safety one included.

If any of these lands upstream in a different form, drop the local commit on
the next sync and check the upstream version closes the same case — the tests
added with each patch are the quickest way to find out.

## Installing this fork

Everything comes from this checkout; nothing is fetched from upstream.

```sh
make install                       # first time: builds as you, then runs the installer as root
git pull && make update            # later: pull (from your fork), rebuild, swap binary + helper
sudo bash deploy/install.sh --uninstall
```

Needs Go 1.25+, Node/npm and make on the machine. `make install` is
interactive; `make install INSTALL_ARGS="--yes"` takes every default, and
`INSTALL_ARGS="--port 9090"` passes flags through.

On a Proxmox host, build elsewhere and install with `--read-only`, choosing
localhost or a management address at the listen prompt:

```sh
make build                         # on a workstation
# copy ./easyzfs and deploy/ to the host, then on the host:
bash deploy/install.sh --binary ./easyzfs --read-only --listen 127.0.0.1
```

The installer adds only `sudo` if missing, upgrades nothing, and prints what
it found before it changes anything. The mode sticks: `--update` and a reinstall both keep it. To leave
it, delete the `EASYZFS_READONLY=1` line from `/etc/easyzfs/env` and run
`make install` again (that also installs the root helper and the full
sudoers).

Why it cannot drift back to upstream:

- `make build` links the binary with `updateChannel=local`. Such a binary has
  **no in-app updater**: it never contacts GitHub, has no `/api/update/*`
  routes, and the UI shows how to update instead of an Update button. A plain
  `go build` (upstream's release pipeline) keeps upstream's updater.
- The installer asks the binary for its channel (`easyzfs -update-channel`).
  For `local` it installs **none** of the root update units
  (`easyzfs-update.path`, the weekly timer) and **removes** any left by an
  earlier install of an upstream release, with `/opt/easyzfs`.
- The root helper comes from `deploy/` in this checkout, never from the network.
- Run from the checkout, the installer **never downloads a binary**. Without
  `--binary`/`--source` it takes the checkout's own build (`./easyzfs`, as
  `make build` leaves it), or stops and points at `make install`. Only an
  explicit `--url` downloads. (Upstream's default download is kept solely for
  its `curl | bash` one-liner, which has no checkout.)
- `make update` (`install.sh --update`) replaces only the binary (keeping the
  previous one as `/usr/local/bin/easyzfs.prev`) and the helper, refreshes
  sudoers, and restarts. It never downloads, and leaves `/etc/easyzfs/env`,
  the data, the service account and the unit alone. A full reinstall now keeps
  lines you added to the env file (`CSRF_CHECK`, `SMTP_*`…); it used to drop
  them.
- Build as your user, install as root: `make install`/`make update` run only
  the installer under sudo, so no root-owned files end up in the checkout.
  `sudo make install` and `sudo bash deploy/install.sh --source …` are refused
  for the same reason.
- `make update` checks the service answers HTTP on the address in
  `LISTEN_ADDR`, and stops with the rollback command if it does not.
- `make update` does not rewrite the systemd unit, on purpose. If a pull
  changes `write_unit` in `deploy/install.sh`, run `make install` instead: it
  keeps your secrets and your own env lines.

Rollback after a bad update:
`sudo install -m 0755 /usr/local/bin/easyzfs.prev /usr/local/bin/easyzfs && sudo systemctl restart easyzfs`.

**Do not** use the `curl … | bash` one-liner for this install: it installs
upstream's release, which lacks every fix above. `UPDATE_CHANNEL=github`
builds this fork with the in-app updater enabled, which can then replace it
with upstream's release.

After migrating from an earlier upstream install, the first `.prev` is
upstream's binary; rolling back to it brings back its updater (inert without
the units, but it contacts GitHub daily). Run `make update` again once fixed.

## Upstream behaviour this fork accepts as-is

| behaviour | decision |
|---|---|
| Even pinned, the sudoers grant lets the service destroy storage, and `zfs recv` is root-equivalent | Inherent to what the app does in full mode: treat a compromise of the web app as a compromise of the machine, or install with `--read-only`. |
| Channel secrets (SMTP, Telegram, ntfy, Gotify) are plaintext in SQLite, alongside argon2 hashes and TOTP seeds | A backup downloaded from Settings therefore contains every secret. Store backups accordingly. |
| The bootstrap admin password is written to the journal on first boot | Change it after first login. |
| Any logged-in user, not only an admin, can acknowledge alerts, and acknowledgement is shared | A design choice; read-only API keys cannot. |

## The commit hooks

`.git/hooks/pre-commit` and `.git/hooks/commit-msg` refuse commits carrying
identifying data (patterns from a file kept outside every repository) or AI
attribution trailers. Upstream keeps AI tooling out of its recorded history and
has force-pushed to strip such trailers before.

Hooks are not versioned, so **a fresh clone has no protection until they are
copied back** into `.git/hooks/` and marked executable. `git rebase` does not
run `commit-msg`, so replayed commits are not re-checked.

## Building

`main.go` embeds `dist/`, which is gitignored. A fresh checkout does not
compile until the frontend is built (`make build`) or a placeholder is dropped
in:

```sh
mkdir -p dist && echo '<!doctype html><html><body></body></html>' > dist/index.html
```
