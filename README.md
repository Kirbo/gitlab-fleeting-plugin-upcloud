# fleeting-plugin-upcloud

A [GitLab fleeting](https://gitlab.com/gitlab-org/fleeting/fleeting) plugin that provisions ephemeral [UpCloud](https://upcloud.com/) servers as GitLab CI runner instances via the [`docker-autoscaler` executor](https://docs.gitlab.com/runner/executors/docker_autoscaler/).

GitLab Runner runs on a small, long-lived **runner manager** host. The plugin creates UpCloud servers from a template you build once (with Docker preinstalled), the runner connects to them over SSH to run jobs in Docker, and the plugin deletes them again when they're no longer needed.

## Prerequisites

- A **runner manager** host (Linux, macOS or Windows) with [GitLab Runner](https://docs.gitlab.com/runner/install/) 16.11 or later installed. The manager doesn't need Docker. The CI instances it creates are always Linux servers on UpCloud.
- An **UpCloud API credential** for the plugin. Either:
  - a Personal Access Token (recommended). Tokens only work from the IP ranges they allow, so allow the runner manager's address:

    ```sh
    upctl account token create --name gitlab-runner --expires-in 2160h --allow-ip-range "<runner manager IP>/32"
    ```

    Note the expiry, and create a new token before it runs out; or
  - an UpCloud username and password with API access enabled (a dedicated [subaccount](https://upcloud.com/docs/guides/managing-subaccounts/) is best).
- The runner manager must be able to reach the instances over SSH (port 22) — on their public IPv4 address, or on a private network with [`use_private_network`](#plugin-config-reference).
- To build the instance template: [`upctl`](https://github.com/UpCloudLtd/upcloud-cli) and `jq`. Both are installed by [`mise install`](#development) if you work from a clone of this repository.

## Installation and upgrading

The plugin runs wherever GitLab Runner runs. Follow the guide for your runner manager's OS. Each one covers installing the plugin, the SSH key, registering the runner, the OS-specific config lines, restarting, upgrading and rolling back:

| Runner manager OS | Guide | Installer |
|---|---|---|
| Linux (amd64, arm64) | [docs/install-linux.md](docs/install-linux.md) | [`install-plugin.sh`](scripts/install-plugin.sh) |
| macOS (Apple Silicon, Intel) | [docs/install-macos.md](docs/install-macos.md) | [`install-plugin.sh`](scripts/install-plugin.sh) |
| Windows (amd64, arm64) | [docs/install-windows.md](docs/install-windows.md) | [`install-plugin.ps1`](scripts/install-plugin.ps1) |

Quick reference:

```sh
# Linux / macOS
curl -fsSL https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/scripts/install-plugin.sh | bash
```

```powershell
# Windows (elevated PowerShell)
irm https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/scripts/install-plugin.ps1 | iex
```

Both installers verify the release checksum, install the latest release unless `VERSION` is set (e.g. `v1.4.0`), and can replace the binary while the runner is running. The runner only loads a new version when it restarts. Check the [release notes](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/releases) for config changes before upgrading.

GitLab Runner finds the plugin through the `plugin` setting. That's either the bare binary name `fleeting-plugin-upcloud`, looked up on the runner's `PATH`, or the full path to the binary. The guides say which to use on each OS.

## Creating a custom UpCloud template

Using a pre-baked template (a server image that already has Docker installed) means instances are ready in seconds rather than having to bootstrap from scratch on every boot. Per GitLab's docs, instances need Docker but **not** GitLab Runner, and must never register themselves as runners; the image script follows that.

Run these steps from any machine with `upctl` and `jq` (authenticated to your UpCloud account).

### 1. Create a builder server based on Debian 13 Trixie

```sh
upctl server create \
  --zone fi-hel1 \
  --os "Debian GNU/Linux 13 (Trixie)" \
  --os-storage-size 10 \
  --hostname "gitlab-runner-template" \
  --plan "4xCPU-8GB" \
  --network family=IPv4,type=public \
  --ssh-keys ~/.ssh/id_ed25519.pub \
  --wait
```

`--ssh-keys` is your own public key, used only to log in to the builder. `--wait` returns once the server has started. If the OS name has changed, list the available ones with `upctl storage list --public --template`.

### 2. Prepare the server

```sh
TEMPLATE_IP="$(upctl server show gitlab-runner-template -o json \
  | jq -r '.ip_addresses[] | select(.access == "public" and .family == "IPv4") | .address')"

ssh "root@${TEMPLATE_IP}" \
  "curl -fsSL 'https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/scripts/custom-image-debian13.sh' | bash -s -- --all"
```

This installs Docker and cloud-init and then cleans the server for imaging. It removes SSH host keys, `authorized_keys`, logs, machine-id and cloud-init state. If SSH isn't accepting connections yet, wait a few seconds and retry. Replace `main` in the URL with a release tag to pin the script version.

### 3. Stop the server

```sh
upctl server stop --wait gitlab-runner-template
```

Only continue if the previous step succeeded: a template made from a half-prepared server will produce broken instances.

### 4. Templatise the storage

```sh
upctl storage templatise "$(upctl server show gitlab-runner-template -o json | jq -r '.storage_devices[0].storage')" --title "GitLab Runner - Debian 13 ($(date +%F))" --wait
```

This creates a new private template in your account without touching the original server.

> The `$(date +%F)` suffix date-stamps the title (e.g. `GitLab Runner - Debian 13 (2026-06-08)`). Each `templatise` mints a **new** template UUID, so when you rebuild later to pick up package updates the date in the title lets you tell versions apart in `upctl storage list --template` and confirm which one your config points at.

### 5. Delete the builder server and its original storage

```sh
upctl server delete --delete-storages gitlab-runner-template
```

### 6. Retrieve the template UUID

```sh
upctl storage list --template
```

Copy the UUID — this is the value to use as `template` in `[runners.autoscaler.plugin_config]`. When you rebuild the template later, update `template` and restart the runner.

## Configuration

Edit the runner's `config.toml`: `/etc/gitlab-runner/config.toml` on Linux, `~/.gitlab-runner/config.toml` on macOS, `C:\GitLab-Runner\config.toml` on Windows. Keep the fields `gitlab-runner register` wrote (`id`, `token`, `token_obtained_at`, …) and add the autoscaler sections. The example uses Linux values for `plugin` and `key_path`; your [OS guide](#installation-and-upgrading) has the right ones for macOS and Windows.

```toml
# At most max_instances × capacity_per_instance jobs can run at once.
concurrent = 20
# Seconds the runner gives plugins to clean up on shutdown (0 = default of 30,
# which is tight for deleting instances).
shutdown_timeout = 120

[[runners]]
  name = "UpCloud GitLab Runner"
  url = "https://gitlab.com"
  token = "<filled in by gitlab-runner register>"
  executor = "docker-autoscaler"
  request_concurrency = 2          # default 1 delays jobs while GitLab long-polls

  [runners.docker]
    image = "alpine:latest"

  [runners.autoscaler]
    plugin = "fleeting-plugin-upcloud"
    capacity_per_instance = 4      # jobs running on one instance at the same time
    max_use_count = 60             # jobs per instance before it is replaced
    max_instances = 5
    instance_ready_command = "docker info"
    delete_instances_on_shutdown = true

    [runners.autoscaler.plugin_config]
      # Auth: use a Personal Access Token (recommended) or username + password
      token = "<your UpCloud API token>"
      # username = "<your UpCloud username>"
      # password = "<your UpCloud password>"
      template = "<your template UUID>"
      name = "my-runner-group"     # unique per runner manager, see below
      plan = "4xCPU-8GB"
      storage_size = 20
      storage_tier = "maxiops"
      zone = "fi-hel1"

    [runners.autoscaler.connector_config]
      username = "root"
      key_path = "/root/.ssh/fleeting-upcloud"
      use_external_addr = true

    [[runners.autoscaler.policy]]
      idle_count = 0               # jobs' worth of idle capacity to keep ready
      idle_time = "45m0s"          # how long an idle instance is kept
```

`config.toml` contains the UpCloud credential, so keep it readable only by the account the runner runs as. Restart the runner after editing it, as described in your OS guide.

Settings left out of the example (`update_interval`, `deletion_retry_interval`, `failure_threshold`, …) use GitLab Runner's defaults. See the [`[runners.autoscaler]` reference](https://docs.gitlab.com/runner/configuration/advanced-configuration/#the-runnersautoscaler-section).

> **Isolation trade-off:** with `capacity_per_instance` and `max_use_count` above 1, jobs share instances (and their Docker daemon) with other jobs, which is faster and cheaper. If jobs from different projects or users must not affect each other, set both to `1` so that each job gets a fresh instance, as GitLab's examples do.

## Plugin config reference

All fields go under `[runners.autoscaler.plugin_config]`.

| Field | Required | Default | Description |
|---|---|---|---|
| `token` | yes* | — | UpCloud Personal Access Token (`ucat_…`) |
| `username` | yes* | — | UpCloud API username (alternative to `token`) |
| `password` | yes* | — | UpCloud API password (required with `username`) |
| `zone` | yes | — | UpCloud zone, e.g. `fi-hel1` |
| `template` | yes | — | UpCloud template UUID to clone for each instance. Checked at startup. |
| `name` | yes | — | Group name, stored on every server as the `fleeting-group` label. It identifies which servers the plugin manages, so it must be unique per runner manager: two managers using the same name would delete each other's servers. GitLab's docs also warn against sharing autoscaling resources between runner managers. |
| `plan` | no | `1xCPU-2GB` | UpCloud server plan (`upctl server plans`) |
| `storage_tier` | no | (from template) | `maxiops`, `standard` or `hdd` |
| `storage_size` | no | (from template) | Storage size in GB |
| `name_prefix` | no | `fleeting` | Prefix for generated hostnames. Sanitized to a valid RFC 1123 label: lowercased, with invalid characters collapsed to hyphens (e.g. `CI` → `ci`). The server title keeps its original casing via `title_prefix`. |
| `title_prefix` | no | `fleeting-plugin-upcloud` | Prefix for generated server titles |
| `max_size` | no | `100` | Maximum number of instances the plugin allows. `max_instances` must not exceed it. |
| `network` | no | — | UUID of an UpCloud private network (`upctl network list`) to attach to every instance, in addition to the public interface. Required by `use_private_network`. |
| `use_private_network` | no | `false` | Connect to instances over their private IP on `network` instead of the public IP. The runner manager must be attached to the same network. |
| `user_data` | no | — | URL or inline script for cloud-init on first boot |

\* Either `token` or both `username`+`password` must be provided.

## How it works

On each autoscaler cycle the plugin:

1. **Update** — lists all UpCloud servers labelled with the group `name` and reports their state to the runner. Servers that are **stopped** or in the **error** state can't run jobs but are still billed for storage, so the plugin deletes them in the background. This covers servers it is removing itself, servers whose earlier delete failed, and servers stopped by anything else. If you stop a group server by hand, for example to debug it, it will be deleted.
2. **Increase** — clones the template to create new servers, injecting the SSH public key derived from `connector_config.key_path`.
3. **Decrease** — hard-stops instances that are no longer needed, without waiting for them to stop, so scaling isn't held up. Update deletes each one once it has stopped.
4. **ConnectInfo** — returns the public (or private) IPv4 address and SSH details so the runner can connect.
5. **Heartbeat** — reports instances that are gone, stopped or in the error state as unhealthy, so the runner replaces them.
6. **Shutdown** — before the runner exits, waits for removals still in progress, so stopped servers aren't left behind.

## Troubleshooting

- **Logs:** plugin messages appear in the runner's log: `sudo journalctl -u gitlab-runner -f` on Linux, the Event Viewer's Application log on Windows, and see the [macOS guide](docs/install-macos.md#5-start-and-verify) for macOS.
- **`fleeting list` doesn't show the plugin:** the runner can't find the binary. On Linux, make sure `sudo which fleeting-plugin-upcloud` finds it. On macOS and Windows, use the full path in `plugin`.
- **`authenticating with UpCloud API` at startup:** the credentials are wrong or expired, or the request comes from an IP the token (or the account's API access settings) doesn't allow.
- **`template … not found` at startup:** `template` doesn't match a storage UUID in your account. Check `upctl storage list --template`.
- **Instances are created but jobs never start:** the runner can't reach them over SSH. Check that port 22 is reachable from the runner manager, that `key_path` and `username` are correct, and that the template has cloud-init (the image script installs it).
- **`plugin process exited  error=exit status 2` when the runner stops**, followed by `decreasing instances` errors: systemd killed the plugins along with the runner. Add `KillMode=mixed` as described in the [Linux guide](docs/install-linux.md#5-make-restarts-graceful-one-time). Instances left behind this way are removed after the next start.
- **Leftover servers:** every server the plugin creates is titled `<title_prefix> - <hostname>` and labelled `fleeting-group=<name>` (shown by `upctl server show <uuid>`). List them by title with `upctl server list -o json | jq -r '.servers[] | select(.title | startswith("fleeting-plugin-upcloud")) | "\(.uuid) \(.state) \(.title)"'`.

## Development

The toolchain (Go, golangci-lint, shellcheck, gotestsum, plus `jq` and `upctl` for the template workflow) and the tasks are managed with [mise](https://mise.jdx.dev/) and defined in [`mise.toml`](mise.toml). Install [mise](https://mise.jdx.dev/getting-started.html), then from the repository root:

```sh
mise trust   # approve this repo's mise.toml (first time only)
mise install
```

```sh
mise run build        # build for the current machine into bin/ (macOS: also ad-hoc signs it)
mise run dist         # build every release binary with .sha256 files into dist/, as CI does
mise run check        # vet, golangci-lint, shellcheck, race-enabled tests, govulncheck
mise tasks            # list all tasks (build-all, install, deploy, clean, …)
```

`mise run test-integration` runs fleeting's conformance test against **real, billed** UpCloud servers (up to 3 servers, deleted afterwards). It needs `UPCLOUD_TOKEN`, `UPCLOUD_ZONE`, `UPCLOUD_TEMPLATE` and `UPCLOUD_SSH_KEY`; see [`integration_test.go`](integration_test.go).

### CI/CD

The [pipeline](.gitlab-ci.yml) runs these stages:

1. **Test** (merge requests and pushes): vet, golangci-lint, race-enabled tests (results and coverage appear in merge requests), govulncheck, shellcheck, PSScriptAnalyzer and GitLab Secret Detection.
2. **Build:** every release binary, with checksums, checking that the binary for the build host runs and reports the right version. Runs on merge requests too, so a cross-compile error shows up before it reaches `main`.
3. **Release** (pushes to `main`): uploads the binaries and creates the GitLab release. This only happens after every target has built, so a release is never published half-built.
4. **Verify:** installs the new release with `install-plugin.sh`, both pinned and as "latest", and checks the version it reports.

Releases are automated: pushes to `main` with releasable [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, …) produce a new version via semantic-release. Other types (`chore:`, `ci:`, `build:`, `docs:`, …) don't trigger a release, so dependency updates that end up in the binary should use `fix(deps):`. [Renovate](renovate.json) is configured to do that.

One-time project settings worth enabling:

- **A weekly [pipeline schedule](https://docs.gitlab.com/ci/pipelines/schedules/) on `main`.** It runs the checks, including govulncheck, so newly disclosed vulnerabilities show up without a push. It never builds or releases.
- **Settings > CI/CD > General pipelines > Auto-cancel redundant pipelines,** so a new push cancels the checks still running for the previous one. Release jobs are never cancelled.
- **Integration test:** to get a manual `integration` job, add `UPCLOUD_TOKEN` (masked), `UPCLOUD_ZONE` and `UPCLOUD_TEMPLATE` as CI/CD variables, and `UPCLOUD_SSH_KEY` as a *File* variable holding a private key. Use protected variables if only `main` should run it.

## Contributing

Issues and merge requests are welcome at [gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud).

## License

[MIT](LICENSE)
