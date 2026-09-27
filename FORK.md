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

Then `go vet ./... && go test -race ./...`, `cd web && npx tsc --noEmit &&
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
| `fix(actions)`: allowlist explicit mountpoints, on clone and on set | `zfs clone -o mountpoint=` was unvalidated, and the properties endpoint's regex accepted `/etc`. Either could mount a dataset over `/etc` as root. A denylist kept missing places (`/var/run` → `/run`, `/opt` hiding `/opt/easyzfs`, `/var/lib/dpkg`), so explicit mountpoints are now limited to the pool's own tree (`/<pool>`, or wherever its root dataset is actually mounted, never `/`) and below `/mnt`, `/media`, `/srv`, `/home`, with no symlinks on the way and every directory passed through root-owned and not writable by others. **Known limit:** an *inherited* mountpoint never passes through this check. **Behaviour change:** a mountpoint outside those trees (say `/data`) has to be set from the CLI. |
| `fix(auth)`: bound `/api/login` | The limiter map grew by one uncollectable entry per request under fresh usernames, with the username (up to 1 MiB) embedded in each key, and entries were created before the argon2 semaphore, so an anonymous client could exhaust a 256 MB service. Usernames over 64 bytes are refused, admission is capped at 32, and the purge works. A client holding all 32 slots still starves other logins, as before; it can no longer take the process down. |
| `fix(encryption)`: verify the current passphrase before change-key | `current_key` was checked for being non-empty and then ignored, so any admin session could re-wrap a dataset to a key its owner does not know. Now verified with a dry-run `zfs load-key -n`. Datasets with `keyformat=raw` (the UI never creates them) can no longer change key through the API. The "incorrect key" detection relies on OpenZFS's English error wording and has not been checked against a real host; if it differs, the change is still refused, with a generic error. |
| `fix(install)`: make the installer's prompts answer what they show | `confirm()` returned its default as an exit status, where 0 means yes, so every text prompt was inverted: Enter at `[s/N]` answered yes. At the uninstall prompt that deleted `/var/lib/easyzfs` and `/etc/easyzfs` (never the pools). Text prompts are what the documented `curl … \| bash` install always uses, since stdin is the pipe. `--yes` now takes the displayed default, so the "ZFS not detected, continue?" prompts abort under `--yes` instead of continuing. **Upstream installs still have this.** |
| `fix(install)`: helper source, weekly updater, dry run, uninstall | Under `curl … \| bash` the script's own directory resolved to the **current directory**, so an `easyzfs-sysd` left there by someone else was installed as the root helper; otherwise it was fetched unverified from the moving `main` branch. It now comes from the checkout, `--source` or next to `--binary`, else from the exact release tag the binary was downloaded from. The weekly-updater step aborted the installer (undefined `NEW_VERSION`, relative `cp`, no `sudo`); it now installs only for a release binary, since for a local or source build it would replace that build with upstream's release every week. Also: `DRY_RUN=1` crashed, uninstall left the root update units and `/opt/easyzfs` behind, and `hostname -I` killed the installer on Arch after a good install. **Upstream installs still have all of these.** |
| `feat(update)`, `feat(install)`: install and update from this checkout only | See "Installing this fork". Also fixed on the way: `--source` was ignored in favour of downloading upstream's release (the default `--url` was checked first); a reinstall dropped hand-added env lines; the CSP still let the browser reach `api.github.com`, which nothing uses. |
| `feat(updater)`: `EASYZFS_NO_UPDATE_CHECK` | Upstream checks `api.github.com` at boot and every 24 h with no opt-out. Not a defect — it sends nothing — but a recurring outbound call nobody agreed to. Does not affect the separate weekly auto-update timer `install.sh` can install. Also stops the Settings icon claiming "up to date" when no check ever succeeded. |
| `CLAUDE.md`, this file | Working notes for this clone. Upstream keeps AI tooling out of its history. |

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
| The sudoers file grants `zpool` and `zfs` with unrestricted arguments | Inherent to what the app does, and root-equivalent: treat a compromise of the web app as a compromise of the machine. The root-helper fix above matters regardless; it is not the only path. |
| Channel secrets (SMTP, Telegram, ntfy, Gotify) are plaintext in SQLite, alongside argon2 hashes and TOTP seeds | A backup downloaded from Settings therefore contains every secret. Store backups accordingly. |
| CSRF origin checking is off unless `CSRF_CHECK=1` | Set it in `/etc/easyzfs/env`. Without it, protection rests on `SameSite=Lax`. |
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
