# Cluster runner bootstrap

The matrix-tier workflows (`matrix-{pr,nightly,weekend}-tier.yml`)
provision **ephemeral** GitHub Actions self-hosted runners on the
three cluster hosts at the start of every run and tear them down
when the run finishes — even on cancel/failure. The runner itself does
not persist between tier runs, and the pristine rule (no Go install, no
runner daemon, no `~/actions-runner` dir) is preserved. One thing does
persist by design: the version-keyed tool cache described under
[Persistent tool cache](#persistent-tool-cache).

The dev-side machinery is in:

  - `.github/actions/cluster-runner-up/`     — bootstrap composite
  - `.github/actions/cluster-runner-down/`   — teardown composite
  - `ansible/runner-setup.yml`               — per-host provisioning
  - `ansible/runner-teardown.yml`            — per-host cleanup

A run from start to finish:

```
ubuntu-latest                    cluster                       github.com
─────────────                    ───────                       ──────────
setup job:
  tailscale up (ephemeral) ───┐
                              └──► reachable from CI
  mint registration-token ────────────────────────────────────► POST /actions/runners/registration-token
  ansible runner-setup ───────► /tmp/actions-runner-<host>/
                                  config.sh --ephemeral …
                                  ./run.sh & (per host)
                                  ────────────────────────────► registers with GH

matrix job:
  GH dispatches to one of
  the new self-hosted
  runners ──────────────────► runs mage Deploy / Validate /
                              ValidateDiff / Cleanup

teardown job (if: always()):
  tailscale up (ephemeral)
  mint removal-token  ────────────────────────────────────────► POST /actions/runners/remove-token
  ansible runner-teardown ──► kill Runner.Listener
                              config.sh remove
                              rm -rf /tmp/actions-runner-<host>/
  sweep orphan registrations ─────────────────────────────────► DELETE /actions/runners/<id>
```

## Required secrets

Configure these in **Settings → Secrets and variables → Actions**
on the `goceleris/probatorium` repo:

| Secret | Purpose | How to create |
|---|---|---|
| `TS_OAUTH_CLIENT_ID` | Tailscale OAuth client ID (read+write devices, scoped to `tag:ci`) | Tailscale admin → Settings → OAuth clients → New client. Tags: `tag:ci`. Scopes: `auth_keys`. |
| `TS_OAUTH_SECRET` | OAuth secret paired with the client ID | Same dialog as above; copy the secret once (only shown at creation). |
| `RUNNER_PAT` | GitHub PAT (fine-grained or classic) with **Administration: read+write** on this repo. Used to mint registration + removal tokens. | https://github.com/settings/tokens — fine-grained, repo: `goceleris/probatorium`, permission: Administration (read/write). |
| `CLUSTER_SSH_KEY` | Private SSH key whose public half is in `~mini/.ssh/authorized_keys` on every cluster host | Generate `ssh-keygen -t ed25519 -f cluster-runner-key`, append the `.pub` to authorized_keys on msa2-server / msa2-client / msr1, paste the private half as this secret. |

A GitHub App installation token would be a cleaner long-term swap
for `RUNNER_PAT` (no human-tied lifetime, scoped per-install) — easy
follow-up once the bootstrap is proven working.

## Required Tailscale ACL

Add (or confirm) that the `tag:ci` ephemeral nodes can SSH the
cluster:

```hujson
{
  "acls": [
    // … your existing rules …
    {
      "action": "accept",
      "src":    ["tag:ci"],
      "dst":    ["msa2-server:22", "msa2-client:22", "msr1:22"],
    },
  ],
  "tagOwners": {
    "tag:ci": ["albert.bausili@github"],
  },
}
```

(Substitute your tailnet's owner identifier.)

## Concurrency / waiting

Every matrix-tier workflow declares:

```yaml
concurrency:
  group: matrix-tier-cluster
  cancel-in-progress: false
```

so GitHub serializes them — a queued run waits for any in-flight
run in the same group to finish before starting. The setup
composite additionally has a `wait-for-manifest-clear: true` input
that polls `/tmp/celeris-bench-manifest.json` on each cluster host
before provisioning, so a **manually-launched** `mage Bench` /
`mage Validate` (bypassing the workflow) is also given a chance to
finish.

## Runner lifecycle

- `--ephemeral` flag on `config.sh`: the runner exits after picking
  up and completing exactly one job. GitHub auto-removes the
  registration on exit.
- `runner-teardown.yml` is the safety net for the case where the
  matrix job is cancelled before the runner picked up a job, or
  crashed before exit.
- The composite's final step sweeps **any** offline
  `celeris-cluster` runner registrations from the API, so an orphan
  from a previously-crashed workflow doesn't block the next run.

## Testing the bootstrap end-to-end

1. Set the four secrets in repo Settings.
2. Update the Tailscale ACL.
3. From the GitHub UI: **Actions** → **Nightly Validation** →
   **Run workflow** → main → duration=10m.
4. Watch the three jobs progress: `setup` (~3 min) → `matrix`
   (10 min) → `teardown` (~2 min).
5. Confirm no `/tmp/actions-runner-*` dirs remain on any cluster
   host afterward. `/tmp/celeris-runner-tarballs/` is expected to
   remain: it is the tool cache, not a leftover.

## Operator overrides

If you want to keep runners alive across multiple workflow runs
(useful when iterating on a tier locally), set the input
`wait-for-manifest-clear: "false"` and skip the `teardown` job. Not
intended for production — the pristine rule means we don't leave
state lying around between scheduled runs. The tool cache below is the
one deliberate exception.

## Persistent tool cache

Every tier run needs the actions-runner tarball, uv, a python build, an
ansible-core venv and the `ansible.posix` collection. Fetching them fresh
on every run made each tier depend on four internet services at once
(probatorium#387, #392), so they live in a cache that survives teardown:

```
/tmp/celeris-runner-tarballs/
  actions-runner-linux-<arch>-<version>.tar.gz
  tools/
    uv-<version>/                         the uv binary
    uv-python/  uv-cache/                 uv's python builds and wheel cache
    ansible-venv-py<py>-core<core>-deps<cutoff>/
    ansible-collections-posix<version>/
    ansible-home/                         ansible-galaxy temp dirs and API cache
```

- **Keyed by pin.** Every directory name carries the version it holds, so a
  bump in `runner-setup.yml` installs fresh instead of reusing a stale copy.
  Old versions stay until a reboot; they are small (tens of MB each).
- **Trusted only when verified, never on presence.** uv must report its
  pinned version. The venv needs a completion stamp *and* a working
  `ansible-playbook --version` naming the pinned core. `ansible.posix` needs
  a completion stamp *and* `ansible-galaxy collection verify --offline`. Any
  miss discards that tool and reinstalls it.
- **Reached through symlinks.** Six cluster workflows hardcode
  `/tmp/actions-runner-<host>/ansible-venv` and `.../ansible-collections`;
  the bootstrap recreates those as links into the cache on every run.
  Teardown removes the links and leaves the cache.
- **Cleared by a reboot**, because `/tmp` is RAM-backed on these hosts. To
  purge it by hand, run `rm -rf /tmp/celeris-runner-tarballs` on each host
  while no cluster run is in progress; the next bootstrap repopulates it.
