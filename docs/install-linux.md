# Linux: install and upgrade

This guide sets up the plugin on a **Linux runner manager**, the host where GitLab Runner runs as the `gitlab-runner` systemd service and reads `/etc/gitlab-runner/config.toml`. It covers Debian, Ubuntu, RHEL and other systemd-based distributions on amd64 and arm64.

Before you start, read the [prerequisites](../README.md#prerequisites) and build an [instance template](../README.md#creating-a-custom-upcloud-template).

## Install

### 1. Install the plugin

The install script detects your architecture, downloads the binary from the latest release, verifies its SHA256 checksum against the published `.sha256` file, and installs it to `/usr/local/bin/fleeting-plugin-upcloud`, using `sudo` if needed:

```sh
curl -fsSL https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/scripts/install-plugin.sh | bash
```

Optional environment variables, set before `bash`:
- `VERSION=v1.4.0` installs a specific release instead of the latest.
- `INSTALL_PATH=/some/path/fleeting-plugin-upcloud` installs somewhere else.

<details>
<summary>Manual download instead</summary>

Download `fleeting-plugin-upcloud-linux-amd64` (or `-arm64`) and its `.sha256` file from the [releases page](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/releases), then:

```sh
sha256sum fleeting-plugin-upcloud-linux-amd64   # must match the .sha256 file
sudo install -m 0755 fleeting-plugin-upcloud-linux-amd64 /usr/local/bin/fleeting-plugin-upcloud
```

</details>

### 2. Create the SSH key for reaching instances

The runner service runs as root, so the key goes in root's home directory. The plugin derives the public key from it and injects it into every instance it creates:

```sh
sudo ssh-keygen -t ed25519 -N "" -f /root/.ssh/fleeting-upcloud
```

### 3. Register the runner

[Create a runner](https://docs.gitlab.com/tutorials/create_register_first_runner/#create-and-register-a-project-runner) in GitLab to get a runner authentication token (`glrt-…`), then:

```sh
sudo gitlab-runner register --non-interactive \
  --url https://gitlab.com \
  --token "<glrt-... runner authentication token>" \
  --name "UpCloud GitLab Runner" \
  --executor docker-autoscaler \
  --docker-image alpine:latest
```

### 4. Configure

Add the autoscaler sections from the [example configuration](../README.md#configuration) to `/etc/gitlab-runner/config.toml`. These lines are Linux-specific:

```toml
[runners.autoscaler]
  plugin = "fleeting-plugin-upcloud"   # found on root's PATH (/usr/local/bin)

  [runners.autoscaler.connector_config]
    username = "root"
    key_path = "/root/.ssh/fleeting-upcloud"
```

The config file holds your UpCloud credential, so keep it private: `sudo chmod 600 /etc/gitlab-runner/config.toml`.

### 5. Make restarts graceful (one-time)

By default, stopping the service aborts running jobs. It also sends the stop signal to every process in the service, including the plugins, which then exit before they can delete instances. This drop-in makes systemd send `SIGQUIT` to the runner only, which waits for running jobs and then shuts the plugins down cleanly:

```sh
sudo mkdir -p /etc/systemd/system/gitlab-runner.service.d
printf '[Service]\nKillSignal=SIGQUIT\nKillMode=mixed\nTimeoutStopSec=infinity\n' | sudo tee /etc/systemd/system/gitlab-runner.service.d/kill.conf
sudo systemctl daemon-reload
systemctl show gitlab-runner -p KillSignal -p KillMode   # expect KillSignal=3, KillMode=mixed
```

See GitLab's [notes on signals](https://docs.gitlab.com/runner/commands/#signals).

### 6. Start and verify

```sh
sudo systemctl restart gitlab-runner
fleeting-plugin-upcloud version
sudo gitlab-runner fleeting list
sudo journalctl -u gitlab-runner --since "5 min ago" --no-pager | grep -i -E "plugin initialized|ERROR" | grep -v "cache adapter"
```

`fleeting list` should show `plugin: fleeting-plugin-upcloud, path: /usr/local/bin/fleeting-plugin-upcloud` for your runner. There should be one `plugin initialized … version=…` line per runner.

## Upgrade

GitLab Runner starts the plugin once and keeps that process, so a new binary takes effect only after the runner restarts. Replacing the binary while the runner is running is safe: the installer swaps it atomically. Check the [release notes](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/releases) for config changes first.

```sh
# 1. Install the new version (prints "Upgraded: ... (old -> new)")
curl -fsSL https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/scripts/install-plugin.sh | bash

# 2. Restart; with the drop-in from step 5 this waits for running jobs
sudo systemctl restart gitlab-runner

# 3. Verify
fleeting-plugin-upcloud version | head -2
sudo journalctl -u gitlab-runner --since "10 min ago" --no-pager \
  | grep -i -E "plugin initialized|exit status|removed instance|ERROR" | grep -v "cache adapter"
```

The restart blocks until running jobs finish, so use `tmux` or `screen` if your SSH session might drop. With `delete_instances_on_shutdown = true`, you should see `removed instance` lines from the shutdown and no `exit status 2`.

**Roll back:** re-run step 1 with `VERSION=<previous tag>` before `bash`, then restart.

## Uninstall

Remove `[runners.autoscaler]` from the config, or unregister the runner, and restart the runner first. Then:

```sh
sudo rm /usr/local/bin/fleeting-plugin-upcloud
```
