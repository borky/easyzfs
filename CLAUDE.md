# CLAUDE.md

Guidance for Claude Code when working in this repository.

## What this is

EasyZFS: a ZFS management app that runs **on the NAS itself**. One static Go
binary (`CGO_ENABLED=0`) that wraps system commands (`zpool`, `zfs`,
`smartctl`, hwmon sensors), exposes a REST + SSE API under `/api`, and serves
an embedded React PWA. Deployed as a Debian/LXC + systemd service. No Docker,
no appliance OS. Target footprint: tens of MB of RAM (`MemoryMax=256M` in the
unit).

Go module is `easyzfs` (Go 1.25). Frontend is `web/` (React 19 + Vite +
TypeScript, hand-written CSS). `landing/` is the separate marketing site for
easyzfs.cloudless.club and is unrelated to the app.

## Build & test

**`dist/` must exist or nothing compiles.** [main.go:53](main.go#L53) has
`//go:embed dist`, and `dist/` is gitignored (built by Vite). A fresh clone
fails `go build`/`go vet`/`go test` with `pattern dist: no matching files
found`. CI works around it the same way:

```bash
mkdir -p dist && echo '<!doctype html><html><body></body></html>' > dist/index.html
```

Backend tests never touch the SPA, so the placeholder is enough.

```bash
go vet ./...
go test -race ./...            # what CI runs; full suite ~30s, all green
staticcheck ./...              # CI uses honnef.co/go/tools @2026.1
make build                     # web (npm ci + vite) → dist/ → static binary
make web / make go             # the two halves separately
make install / make update     # fork: install or update this machine from the checkout (see FORK.md)
cd web && npx tsc -b --noEmit  # typecheck; plain 'tsc --noEmit' checks NOTHING (tsconfig.json has "files": [])
cd web && npm run dev          # Vite dev server, proxies /api → localhost:8080
```

For frontend work without a ZFS host, run the backend with `MOCK=1` (mock
collectors, real mutations attempted) or `DEMO=1` (mock + all mutations return
403 `demo_mode`).

## Architecture invariants

These are the rules the codebase is built around. Breaking one is a design
regression, not a style nit.

1. **HTTP handlers read the collectors' in-memory cache. They never run system
   commands.** Collectors poll on tickers and publish to the cache + SSE;
   `internal/httpapi` only reads `collectors.PoolProvider` / `DiskProvider` /
   `PerfProvider` / `SysTimerProvider` / `CapProvider`. The UI stays instant no
   matter how slow the disks are. The one documented exception is dataset
   properties (on-demand read with a 30s TTL).
2. **All system commands go through `internal/executil`, and storage tools
   through the privileged gateway.** `zfs`, `zpool`, `smartctl`, `dd`, `hdparm`
   and `udisksctl` run as `sudo easyzfs priv <tool> …` (`internal/priv`,
   policy in `internal/actions/privgate.go`): a closed argv grammar plus the
   host/guest, mountpoint and disk checks, re-run as root right before the
   command. Sudoers grants none of those tools directly. A new command shape
   needs a grammar entry in `privgate.go` (and a test), or the gateway
   refuses it at runtime while service-side tests still pass. `CommandContext`
   with a timeout, args passed separately — **never** a shell string. `executil`
   auto-prepends `sudo -n` when euid != 0 (override `EASYZFS_SUDO=0|1`). Use
   `RunStdin` for secrets (ZFS passphrases go over stdin, never argv — argv is
   visible in `ps`); `RunTolerant` for `smartctl`, whose nonzero exit is a
   warning bitfield with valid JSON on stdout; `NewCommand` for long-lived
   processes (`zpool events -f`, `zfs rewrite`). Plain reads (`zpool
   list/status/get/iostat`, `zfs list/get`, `lsblk`) use `RunRead`: no sudo,
   falling back to it only on "permission denied" — they work unprivileged on
   Debian/Proxmox, and read-only mode grants no sudo for them.
3. **A failing command degrades one metric. It never takes down the process.**
4. **Mutations live in `internal/actions`**, which validates names against
   regex whitelists (`rePool`, `reDataset`, `reSnapName`, `reDev`), checks
   `{"confirm":"<exact target>"}` on destructive ops, and writes to
   `audit_log`. Never let user input reach `zpool`/`zfs` without passing a
   whitelist. Secrets never enter `audit_log`.
5. **Long operations are launched, then observed.** Actions start a scrub /
   resilver / SMART test / rewrite; the corresponding collector or
   `internal/longops` observes progress and publishes SSE. `longops` state is
   in-memory only — a daemon restart orphans running ops by design.
6. **SSE hub never blocks the publisher.** `hub.Publish` drops events for slow
   subscribers rather than blocking a collector. 25s heartbeat and
   `X-Accel-Buffering: no` are required (proxies cut idle streams at ~60s). No
   global `WriteTimeout` on the HTTP server — it would kill SSE.
7. **DB migrations are append-only.** Add a new SQL string to the end of the
   `migrations` slice in [internal/db/db.go](internal/db/db.go) with a `// vN:`
   comment explaining why. Never `DROP`, never edit an existing entry,
   never reorder. `schema.sql` is v1 only.
8. **Dependencies are deliberately few.** Adding a Go dependency needs a
   reason. `modernc.org/sqlite` (pure Go) exists specifically to keep
   `CGO_ENABLED=0`; do not swap in a cgo driver.

## Package map

```
main.go              wiring: config → db → settings → users → hub → alerter →
                     collectors → actions → scheduler → HTTP; graceful shutdown
internal/
  config/            env → validated struct; rejects .env.example placeholders
  db/                SQLite open + embedded append-only migrations + purges
  model/             shared domain types = the JSON API contract (neutral pkg,
                     imported by everything to avoid dependency cycles)
  executil/          defensive exec (see invariant 2)
  collectors/        zpool, smart, sensors, schedsys, perf, caps, events,
                     mantenimiento, mock — each an in-memory cache + SSE
  actions/           real ZFS/SMART mutations: whitelists, confirm, audit_log;
                     mountpoint.go resolves the mountpoint ZFS will really use
                     (inherited/received/default) before anything mounts
  scheduler/         snapshot/scrub/smart jobs; custom schedule format
  longops/           generic long-process runner (rewrite, replication)
  replication/       zfs send/recv local + SSH, own SSH keypair under DATA_DIR
  alerts/            threshold evaluation → alerts table + SSE + fan-out
  channels/          ntfy, gotify, telegram, syslog, email — config in DB
  push/              Web Push (VAPID), per-user prefs, quiet-hours queue
  webhook/           outbound HMAC-signed webhook, bounded queue + DLQ
  notifier/          SMTP mailer + es/en alert templates
  hub/               SSE broker
  httpapi/           REST handlers; reads cache, NEVER runs CLI
  auth/              HttpOnly cookie sessions (token|HMAC-SHA256) + role mw
  users/ totp/ apikeys/   argon2id passwords, optional TOTP 2FA, read-only keys
  recs/              pure disk-replacement recommendation rules (no system I/O)
  series/ backup/ updater/ settings/ security/
web/src/
  data/              provider.ts (interface), http.ts (real), mock.ts (demo)
  ui/                store.tsx (global state, hash router), i18n.ts, theme.ts
  views/ components/
deploy/              systemd unit, sudoers, easyzfs-sysd helper, install.sh
docs/api-contract.md the front↔back contract — source of truth for the API
```

## Conventions

- **Write new comments in English**, even though this repo is Spanish-commented
  throughout. It is the maintainer's stated convention ("English comments;
  user-facing strings go through i18n") and the user's explicit instruction.
  **Do not translate existing Spanish comments** — the rule governs what gets
  added. The pull toward Spanish is strong and silent here: it shows up in no
  build, lint or test, so re-read the comments you just wrote before
  committing.
- Spanish that is *not* a comment stays Spanish: API error `message` strings,
  `log.Printf` text, and the `es` half of `web/src/ui/i18n.ts` are existing
  product behaviour and part of the contract.
- Comments explain *why*, often citing the issue number (`#136`), a dated
  lesson (`bug 3-Ago-2026`), or the constraint that forced the design. Keep
  that density when editing; drop the rationale and the next reader loses it.
- Commits follow Conventional Commits with the issue in parens:
  `feat(alerts): alert when a known pool is not imported (#136) (#137)`.
- **No AI attribution in commit messages** — no `Co-Authored-By: Claude`, no
  "Generated with Claude Code" footer, no robot emoji. The maintainer keeps AI
  tooling out of recorded history and has force-pushed to strip such trailers
  before. The history here is clean of them; keep it that way. A local
  `commit-msg` hook enforces it, but hooks are never cloned — don't rely on it.
- Never commit identifiable data: disk serials, `/dev/disk/by-id` names, real
  pool/dataset names or paths, `zpool history` excerpts. Fixtures use invented
  values — the repo's own convention is `tank`, `ssd`, `bigtank`.
- API errors are always `{"error":"code","message":"human text"}` with a 4xx/5xx
  status. Use the `writeErr`/`writeJSON`/`requireConfirm` helpers in
  [internal/httpapi/httpapi.go](internal/httpapi/httpapi.go).
- JSON numbers: bytes as integers (the frontend formats). Timestamps: RFC3339
  UTC.
- Admin-only routes are wrapped in `s.auth.RequireAdmin(...)` at registration
  in `Handler()`. Mutations are potentially destructive — default to admin.

## Adding an API endpoint

1. Document it in [docs/api-contract.md](docs/api-contract.md) — that file is
   the contract, not an afterthought.
2. Add/extend the type in `internal/model` if it's new domain data.
3. Handler in `internal/httpapi/<area>.go`, reading the collector cache.
   Register the route in `Handler()`, wrapping with `RequireAdmin` if it
   mutates, and also with `s.requireReauth` (password again, plus TOTP) if it
   is irreversible or can lock users out. A storage mutation must also be
   listed in `storageRoute()`, which read-only mode refuses.
4. Mutation logic (if any) in `internal/actions`: whitelist the input, require
   `confirm` when destructive, write `audit_log`. A disk about to be handed to
   ZFS or powered off goes through `requireFreeDisk` (live `lsblk`, never the
   cache); a property with real blast radius gets a `PropRisk` entry.
   Every action that changes a pool or dataset calls `guardHost`
   (`internal/actions/hoststorage.go`) before it runs anything: the running
   OS, Proxmox storage and guest disks are recognised from the live host, and
   on Proxmox those pools predate the install, so never infer them from what
   EasyZFS created.
   Deleting a dataset from the UI goes through the recycle bin
   (`DatasetTrash`, `internal/actions/trash.go`), and anything that takes
   redundancy away checks the pool first (`vdevActionRisk`). Anything that can
   end in a dataset being mounted goes through `internal/actions/mountpoint.go`
   on the path ZFS will really use, not the one that was asked for
   (`checkMountpoint` in `props.go` only covers a value somebody sets). Pick the
   entry point: `checkEffectiveMountpoint` for one dataset, `…Tree` for a
   `create -p` that makes ancestors too, `checkMountDanger` where the operation
   does not choose the place (promote, `canmount`), `checkInheritedMountpoint`
   for `zfs inherit mountpoint`, `checkRenameMount` for a rename, and
   `mountTree` to mount a whole tree as an import would.
   Any new `zpool`/`zfs` argument shape needs a line in `pinned_sudoers`
   (see below), or sudo refuses it at runtime while every test passes.
5. Frontend: add the method to `web/src/data/provider.ts` (the interface),
   then implement it in **both** `http.ts` **and** `mock.ts`. The mock is what
   the public demo runs on — leaving it out breaks the demo, and TypeScript
   will fail the build anyway.
6. Add types to `web/src/data/types.ts`.
7. Any user-facing string goes in `web/src/ui/i18n.ts` in **both** `es` and
   `en`. `en` is typed `Record<I18nKey, string>`, so a missing key is a compile
   error.

## Before shipping a change

- **Report observed state, not intended state.** This app is nothing but a
  state reporter for storage, and the cache between the system and the API
  makes the trap structural. For anything the UI or API reports, separate "what
  did we ask for" from "what is actually happening" and pick the one the caller
  needs; where both exist they must be the same predicate or deliberately
  different with a comment. Staleness is a reported state too — the
  `pool_missing` alert (#136) exists because "no data for this pool" had been
  rendering as "fine". When a fix doesn't work, add a log line that
  distinguishes the cases before re-explaining the theory.
- **Run a review subagent on high-impact or multi-file changes**, then a second
  pass to verify the findings are actually closed (the fixes introduce their
  own defects). High impact here means anything that can lose data or lock the
  user out: `internal/actions` mutations, replication `force_full`, backup
  import, the migration list, `executil` argument construction, auth/2FA/API
  keys, the updater's swap path. Multi-file is most features — a new endpoint
  touches nine files across backend and frontend.
- Fixtures must be real artefacts in the system's own format, not what the code
  expects. `internal/collectors/testdata/` holds actual `smartctl` JSON
  including the awkward cases (`exit192`, `nosmart`) for this reason.
- Don't hardcode device identity. `sdX` letters are unstable across reboots and
  bay moves (issue #65); `reDevByIDPath` and `resolveVdevPaths` exist to resolve
  by-id paths and UUIDs to real devices. Resolve, don't assume.

## Frontend notes

- No component library and no CSS framework beyond Tailwind's preflight. All
  styling is hand-written CSS with per-theme custom properties in
  `web/src/index.css`. [DESIGN.md](DESIGN.md) is the design system: Space
  Grotesk for UI voice, **JetBrains Mono for every technical value** (pool,
  vdev, device, path, size, temperature), border XOR shadow (never both),
  numeric columns right-aligned with `tabular-nums`.
- Four theme families (`classic`, `modern`, `phosphor`, `brutalist`) × light/
  dark/system, plus four accents and a density toggle — see
  `web/src/ui/theme.ts`. localStorage keys are `easyzfs-*`; legacy `zfc-*` keys
  migrate once on read.
- Routing is a hash router in `web/src/ui/store.tsx`. Views are code-split with
  `lazyRetry`, which reloads once if a chunk vanished after a deploy.
- Demo mode is a **client-side** mock session (localStorage `zfc-demo`); real
  credentials always show real data, there is no fallback to mock. `DEMO=1` on
  the server is a separate thing: a mock deployment with mutations blocked.

## Deployment & privileges

The service runs as the unprivileged `easyzfs` user. Its sudoers file is
generated by `pinned_sudoers` in [deploy/install.sh](deploy/install.sh) and
grants five things: the privileged gateway (`/usr/local/bin/easyzfs priv *`,
see invariant 2; it also answers `priv pvecfg`, Proxmox's root-only storage
and guest configs), `lsblk` and `fuser` with pinned arguments, `crontab -l`,
and `/usr/local/libexec/easyzfs-sysd`, a confined root helper accepting
three validated operations on whitelisted files. No
storage tool is granted directly. [deploy/easyzfs.sudoers](deploy/easyzfs.sudoers)
is generated from the same function for manual installs;
`TestStaticSudoersMatchesInstaller` and `TestSudoersGrantNoStorageToolDirectly`
fail when they drift. The unit deliberately does **not** set
`NoNewPrivileges=yes` — sudo needs the setuid bit. It also sets no
`ProtectSystem`/`ProtectHome`/`PrivateTmp`: each creates a private mount
namespace, which the sudo'd `zfs` inherits, so mounts and unmounts stopped
reaching the host (see FORK.md). Don't add them back.

If you add a command that needs root, add its shape to the gateway's grammar
(`internal/actions/privgate.go`) with the checks its action makes, and a case
in `privgate_test.go`; check it with real sudo on a test host
(`sudo -u easyzfs sudo -n /usr/local/bin/easyzfs priv <tool> …`). Read-only
mode (`--read-only`, `EASYZFS_READONLY=1`) writes a far smaller file: a few
pinned reads and `crontab -l`, no gateway and no helper.

Self-update: the daemon only detects and downloads (checksum-validated) to
`$DATA_DIR/update/`, with the release tag in `easyzfs.new.tag`, then touches a
flag; the root `easyzfs-update.path` unit runs `deploy/easyzfs-apply-update`,
which verifies that tag's `checksums.txt.minisig` against the embedded
minisign key (none embedded = nothing installed), then re-checks the binary
against `checksums.txt` itself before swapping and restarting; a refusal is
left in `$DATA_DIR/update/apply-refused` for the UI. The weekly timer goes
through the same helper. The daemon never writes to `/usr/local/bin`.

In this fork, `make build` links `-X main.updateChannel=local`: that binary
has no updater at all and is updated with `make update` from the checkout.
The self-update path above applies only to a plain `go build`.

Config is entirely environment variables read at startup (`/etc/easyzfs/env`) —
see the table in [README.md](README.md#configuration). Alert channel settings
are the exception: they live in the DB and are editable from Settings without a
restart; env only seeds them on first boot.
