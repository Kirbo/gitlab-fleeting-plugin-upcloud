# macOS: install and upgrade

This guide sets up the plugin on a **macOS runner manager** (Apple Silicon or Intel). On macOS, GitLab Runner [runs as a LaunchAgent](https://docs.gitlab.com/runner/install/osx/) under your user account, not as root. It reads `~/.gitlab-runner/config.toml`, starts when you log in and stops when you log out. For a manager that should survive reboots, turn on automatic login.

Before you start, read the [prerequisites](../README.md#prerequisites) and build an [instance template](../README.md#creating-a-custom-upcloud-template). The instances themselves are Linux servers on UpCloud. Only the manager runs macOS.

## Install

### 1. Install the plugin

The same install script as on Linux detects macOS and your architecture, verifies the checksum and installs to `/usr/local/bin/fleeting-plugin-upcloud`. It asks for your password if `/usr/local/bin` isn't writable:

```sh
curl -fsSL https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/scripts/install-plugin.sh | bash
```

Optional environment variables, set before `bash`:
- `VERSION=v1.4.0` installs a specific release instead of the latest.
- `INSTALL_PATH=…` installs somewhere else.

<details>
<summary>Manual download instead</summary>

Download `fleeting-plugin-upcloud-darwin-arm64` (Apple Silicon) or `-darwin-amd64` (Intel) and its `.sha256` file from the [releases page](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/releases), then:

```sh
shasum -a 256 fleeting-plugin-upcloud-darwin-arm64   # must match the .sha256 file
xattr -d com.apple.quarantine fleeting-plugin-upcloud-darwin-arm64 2>/dev/null || true   # browser downloads are quarantined
sudo install -m 0755 fleeting-plugin-upcloud-darwin-arm64 /usr/local/bin/fleeting-plugin-upcloud
```

</details>

### 2. Create the SSH key for reaching instances

The runner runs as you, so the key lives in your home directory:

```sh
ssh-keygen -t ed25519 -N "" -f ~/.ssh/fleeting-upcloud
```

### 3. Register the runner

[Create a runner](https://docs.gitlab.com/tutorials/create_register_first_runner/#create-and-register-a-project-runner) in GitLab to get a runner authentication token (`glrt-…`), then, as your user (no `sudo`):

```sh
gitlab-runner register --non-interactive \
  --url https://gitlab.com \
  --token "<glrt-... runner authentication token>" \
  --name "UpCloud GitLab Runner" \
  --executor docker-autoscaler \
  --docker-image alpine:latest
```

### 4. Configure

Add the autoscaler sections from the [example configuration](../README.md#configuration) to `~/.gitlab-runner/config.toml`. These lines are macOS-specific. Use full paths: launchd services get a minimal `PATH` that usually doesn't include `/usr/local/bin`, and `~` isn't expanded in the config:

```toml
[runners.autoscaler]
  plugin = "/usr/local/bin/fleeting-plugin-upcloud"

  [runners.autoscaler.connector_config]
    username = "root"                                   # the user on the Linux instances
    key_path = "/Users/<you>/.ssh/fleeting-upcloud"
```

Keep the file private, since it holds your UpCloud credential: `chmod 600 ~/.gitlab-runner/config.toml`.

### 5. Start and verify

```sh
gitlab-runner stop; gitlab-runner start
fleeting-plugin-upcloud version
gitlab-runner fleeting list
```

`fleeting list` should show `path: /usr/local/bin/fleeting-plugin-upcloud` for your runner. Where the runner's log goes depends on its LaunchAgent: look for `StandardOutPath`/`StandardErrorPath` in `~/Library/LaunchAgents/gitlab-runner.plist`. To watch it live, stop the service and run `gitlab-runner run` in a terminal instead.

## Upgrade

The runner keeps using the old plugin until it restarts. Check the [release notes](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/releases) for config changes first.

On macOS, `gitlab-runner stop` doesn't wait for running jobs. Before restarting, **pause the runner** in GitLab (the runner's page under **Settings > CI/CD > Runners**) and wait until it has no running jobs:

```sh
# 1. Install the new version (prints "Upgraded: ... (old -> new)")
curl -fsSL https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/scripts/install-plugin.sh | bash

# 2. Restart (after pausing the runner and letting jobs finish)
gitlab-runner stop; gitlab-runner start

# 3. Verify
fleeting-plugin-upcloud version | head -2
gitlab-runner fleeting list
```

Then resume the runner in GitLab. If instances are left behind by an abrupt stop, the plugin removes them after the next start.

**Roll back:** re-run step 1 with `VERSION=<previous tag>` before `bash`, then restart.

## Uninstall

Remove `[runners.autoscaler]` from the config, or unregister the runner, and restart the runner first. Then:

```sh
sudo rm /usr/local/bin/fleeting-plugin-upcloud
```
