# fleeting-plugin-upcloud

A [GitLab fleeting](https://gitlab.com/gitlab-org/fleeting/fleeting) plugin that provisions ephemeral [UpCloud](https://upcloud.com/) servers as GitLab CI runner instances via the [`docker-autoscaler` executor](https://docs.gitlab.com/runner/executors/docker_autoscaler/).

GitLab Runner runs on a small, long-lived **runner manager** host. The plugin creates UpCloud servers from a template you build once (with Docker preinstalled), the runner connects to them over SSH to run jobs in Docker, and the plugin deletes them again when they're no longer needed.

## Prerequisites

- A **runner manager** host (Linux) with [GitLab Runner](https://docs.gitlab.com/runner/install/) 16.11 or later installed. The manager doesn't need Docker.
- An **UpCloud API credential** for the plugin. Either:
  - a Personal Access Token (recommended). Tokens only work from the IP ranges they allow, so allow the runner manager's address:

    ```sh
    upctl account token create --name gitlab-runner --expires-in 2160h --allow-ip-range "<runner manager IP>/32"
    ```

    Note the expiry, and create a new token before it runs out; or
  - an UpCloud username and password with API access enabled (a dedicated [subaccount](https://upcloud.com/docs/guides/managing-subaccounts/) is best).
- The runner manager must be able to reach the instances over SSH (port 22) — on their public IPv4 address, or on a private network with [`use_private_network`](#plugin-config-reference).
- To build the instance template: [`upctl`](https://github.com/UpCloudLtd/upcloud-cli) and `jq`. Both are installed by [`mise install`](#development) if you work from a clone of this repository.

## Installation

Run these steps on the runner manager.

### Quick install (recommended)

The install script detects your OS and architecture, downloads the binary from the latest release, verifies its SHA256 checksum against the published `.sha256` release asset, and installs it to `/usr/local/bin/fleeting-plugin-upcloud` (using `sudo` if needed):

```sh
curl -fsSL https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/scripts/install-plugin.sh | bash
```

Optional environment variables: `VERSION=v1.2.0` installs a specific release instead of the latest, and `INSTALL_PATH=/some/path/fleeting-plugin-upcloud` installs elsewhere.

GitLab Runner finds manually installed fleeting plugins on `$PATH`, by binary name, which must follow the `fleeting-plugin-<name>` convention ([docs](https://docs.gitlab.com/runner/fleet_scaling/fleeting/)). So the config refers to the plugin by its full binary name:

```toml
[runners.autoscaler]
  plugin = "fleeting-plugin-upcloud"
```

(`~/.config/fleeting/plugins/...` is only used by `gitlab-runner fleeting install` for plugins distributed through OCI registries; it doesn't apply here.)

### Manual download

Download the binary and its `.sha256` file for your platform from the [releases page](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/releases), verify it, and install it under the name `fleeting-plugin-upcloud` somewhere on `$PATH`:

```sh
# example for linux/amd64
sha256sum fleeting-plugin-upcloud-linux-amd64   # must match the .sha256 file
sudo install -m 0755 fleeting-plugin-upcloud-linux-amd64 /usr/local/bin/fleeting-plugin-upcloud
```

### Verify it's found

```sh
fleeting-plugin-upcloud version
sudo gitlab-runner fleeting list
```

Once the [configuration](#configuration) is in place, `fleeting list` should output something like:

```
Runtime platform                                    arch=amd64 os=linux pid=12402 revision=07e534ba version=18.9.0
runner: TCfHVcDHi, plugin: fleeting-plugin-upcloud, path: /usr/local/bin/fleeting-plugin-upcloud
```

## Upgrading

GitLab Runner starts the plugin once and keeps that process running, so installing a new binary has no effect until the runner restarts. Replacing the binary while the runner is running is safe.

1. **One-time setup: make restarts graceful.** By default, stopping the `gitlab-runner` service aborts running jobs. GitLab [recommends](https://docs.gitlab.com/runner/commands/#signals) a systemd drop-in that stops the runner with `SIGQUIT` instead, which waits for running jobs to finish:

   ```sh
   sudo mkdir -p /etc/systemd/system/gitlab-runner.service.d
   printf '[Service]\nKillSignal=SIGQUIT\nTimeoutStopSec=infinity\n' | sudo tee /etc/systemd/system/gitlab-runner.service.d/kill.conf
   sudo systemctl daemon-reload
   ```

2. **Install the new version** by re-running the [install script](#quick-install-recommended). It prints the old and new versions. Add `VERSION=vX.Y.Z` to pick a specific release.

3. **Restart the runner** to load it:

   ```sh
   sudo systemctl restart gitlab-runner
   ```

   With the drop-in from step 1, this waits for running jobs to finish. With `delete_instances_on_shutdown = true` (as in the example config), the old instances are deleted on shutdown and new ones are created after the restart.

4. **Check** that the new version is in use:

   ```sh
   fleeting-plugin-upcloud version
   sudo journalctl -u gitlab-runner --since "10 min ago" | grep -i -E "fleeting|plugin|error"
   ```

To roll back, re-run the install script with `VERSION=` set to the previous release and restart again. Check the [release notes](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/releases) for config changes before upgrading.

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

Copy the UUID — this is the value to use as `template` in `[runners.autoscaler.plugin_config]`. When you rebuild the template later, update `template` and [restart the runner](#upgrading).

## Setting up the runner manager

### 1. Create the SSH key the runner uses to reach instances

The runner service runs as root, so the key lives in root's home directory. The plugin derives the public key from it and injects it into every instance it creates:

```sh
sudo ssh-keygen -t ed25519 -N "" -f /root/.ssh/fleeting-upcloud
```

The path must match `key_path` in [the configuration](#configuration).

### 2. Register the runner

[Create a runner](https://docs.gitlab.com/tutorials/create_register_first_runner/#create-and-register-a-project-runner) in GitLab (project, group, or instance level). This gives you a runner authentication token (`glrt-…`). Then register it on the runner manager:

```sh
sudo gitlab-runner register --non-interactive \
  --url https://gitlab.com \
  --token "<glrt-... runner authentication token>" \
  --name "UpCloud GitLab Runner" \
  --executor docker-autoscaler \
  --docker-image alpine:latest
```

This writes the `[[runners]]` entry to `/etc/gitlab-runner/config.toml`. Complete it as described below.

## Configuration

Edit `/etc/gitlab-runner/config.toml`. Keep the fields `gitlab-runner register` wrote (`id`, `token`, `token_obtained_at`, …) and add the autoscaler sections:

```toml
# At most max_instances × capacity_per_instance jobs can run at once.
concurrent = 20

[[runners]]
  name = "UpCloud GitLab Runner"
  url = "https://gitlab.com"
  token = "<filled in by gitlab-runner register>"
  executor = "docker-autoscaler"

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

`config.toml` contains the UpCloud credential, so keep it readable by root only (`sudo chmod 600 /etc/gitlab-runner/config.toml`). Restart the runner after editing it: `sudo systemctl restart gitlab-runner`.

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

- **Logs:** `sudo journalctl -u gitlab-runner -f`. Plugin messages are included.
- **`fleeting list` doesn't show the plugin:** make sure `sudo which fleeting-plugin-upcloud` finds it. The runner service runs as root, so the binary must be on root's `PATH` (`/usr/local/bin` is).
- **`authenticating with UpCloud API` at startup:** the credentials are wrong or expired, or the request comes from an IP the token (or the account's API access settings) doesn't allow.
- **`template … not found` at startup:** `template` doesn't match a storage UUID in your account. Check `upctl storage list --template`.
- **Instances are created but jobs never start:** the runner can't reach them over SSH. Check that port 22 is reachable from the runner manager, that `key_path` and `username` are correct, and that the template has cloud-init (the image script installs it).
- **Leftover servers:** every server the plugin creates is titled `<title_prefix> - <hostname>` and labelled `fleeting-group=<name>` (shown by `upctl server show <uuid>`). List them by title with `upctl server list -o json | jq -r '.servers[] | select(.title | startswith("fleeting-plugin-upcloud")) | "\(.uuid) \(.state) \(.title)"'`.

## Development

The toolchain (Go, golangci-lint, plus `jq` and `upctl` for the template workflow) and the tasks are managed with [mise](https://mise.jdx.dev/) and defined in [`mise.toml`](mise.toml). Install [mise](https://mise.jdx.dev/getting-started.html), then from the repository root:

```sh
mise trust   # approve this repo's mise.toml (first time only)
mise install
```

```sh
mise run build        # build for the current machine into bin/ (macOS: also ad-hoc signs it)
mise run build-all    # build for linux/darwin × amd64/arm64
mise run check        # everything CI runs: vet, lint, race-enabled tests, govulncheck
mise tasks            # list all tasks (install, deploy, clean, …)
```

`mise run test-integration` runs fleeting's conformance test against **real, billed** UpCloud servers (up to 3 servers, deleted afterwards). It needs `UPCLOUD_TOKEN`, `UPCLOUD_ZONE`, `UPCLOUD_TEMPLATE` and `UPCLOUD_SSH_KEY`; see [`integration_test.go`](integration_test.go).

Releases are automated: pushes to `main` with releasable [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, …) produce a new version via semantic-release. Other types (`chore:`, `ci:`, `build:`, `docs:`, …) don't trigger a release, so dependency updates that end up in the binary should use `fix(deps):`. [Renovate](renovate.json) is configured to do that. To get notified about newly disclosed vulnerabilities, add a weekly [pipeline schedule](https://docs.gitlab.com/ci/pipelines/schedules/) on `main`. It runs `govulncheck` without releasing.

## Contributing

Issues and merge requests are welcome at [gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud).

## License

[MIT](LICENSE)
