# fleeting-plugin-upcloud

A [GitLab fleeting](https://gitlab.com/gitlab-org/fleeting/fleeting) plugin that provisions ephemeral [UpCloud](https://upcloud.com/) servers as GitLab CI runner instances via the `docker-autoscaler` executor.

## Installation

### Quick install (recommended)

The install script detects your OS and architecture automatically and downloads the correct binary from the latest release:

```sh
curl -fsSL https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/scripts/install-plugin.sh | bash
```

### Manual download

Download the binary for your platform from the [releases page](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/releases), rename it to `fleeting-plugin-upcloud`, and place it on your `$PATH`:

```sh
# example for linux/amd64
curl -fsSL -o /root/.config/fleeting/plugins/registry.gitlab.com/gitlab-org/fleeting/plugins/fleeting-plugin-upcloud "<download-url-for-fleeting-plugin-upcloud-linux-amd64>"
chmod +x /root/.config/fleeting/plugins/registry.gitlab.com/gitlab-org/fleeting/plugins/fleeting-plugin-upcloud
```

### Verify it's found

```sh
gitlab-runner fleeting list
```
..should output something like:
```sh
Runtime platform                                    arch=amd64 os=linux pid=12402 revision=07e534ba version=18.9.0
runner: TCfHVcDHi, plugin: fleeting-plugin-upcloud, path: /root/.config/fleeting/plugins/registry.gitlab.com/gitlab-org/fleeting/plugins/fleeting-plugin-upcloud
```

### Build from source

Requires Go 1.21+ and [just](https://just.systems) ([docs](https://just.systems/man/en/)).

Install `just`:

```sh
# macOS
brew install just

# Linux (Debian/Ubuntu 24.04+)
apt install just

# Any platform via cargo
cargo install just

# Any platform — pre-built binary (see https://github.com/casey/just/releases)
curl --proto '=https' --tlsv1.2 -sSf https://just.systems/install.sh | bash -s -- --to /usr/local/bin
```

```sh
# Build for the current machine (macOS: also ad-hoc signs the binary)
just build

# Build for all platforms (linux/darwin × amd64/arm64)
just build-all

# Or build per platform
just build-linux
just build-mac
```

## Creating a custom UpCloud template

Using a pre-baked template (a server image that already has Docker installed) means instances are ready in seconds rather than having to bootstrap from scratch on every boot. The following steps use the [upctl](https://github.com/UpCloudLtd/upcloud-cli) CLI.

### 1. Create a builder server based on Debian 13 Trixie

```sh
upctl server create \
  --zone fi-hel1 \
  --os "Debian GNU/Linux 13 (Trixie)" \
  --os-storage-size 10 \
  --hostname "gitlab-runner-template" \
  --plan "4xCPU-8GB" \
  --network family=IPv4,type=public \
  --ssh-keys ~/.ssh/id_rsa.pub
```

### 2. Wait until the server has started

```sh
while true; do
  STATE=$(upctl server show gitlab-runner-template -o json | jq -r '.state')
  echo "Current state: ${STATE}"
  if [[ "${STATE}" == "started" ]]; then
    echo "Server started successfully."
    break
  elif (( TRIES >= 60 )); then
    echo "Aborted due maximum 60 tries."
    break
  fi

  sleep 5
  ((TRIES++))
done
```

### 3. Prepare the server and shut it down

```sh
ssh root@$(upctl server show gitlab-runner-template -o json | jq -r '.ip_addresses[0].address') \
  "curl -fsSL 'https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/scripts/custom-image-debian13.sh' | bash"
```

### 4. Wait until the server has stopped

```bash
while true; do
  STATE=$(upctl server show gitlab-runner-template -o json | jq -r '.state')
  echo "Current state: ${STATE}"
  if [[ "${STATE}" == "stopped" ]]; then
    echo "Server stopped successfully."
    break
  elif (( TRIES >= 60 )); then
    echo "Aborted due maximum 60 tries."
    break
  fi

  sleep 5
  ((TRIES++))
done
```

### 5. Templatise the storage

```sh
upctl storage templatise "$(upctl server show gitlab-runner-template -o json | jq -r '.storage_devices[0].storage')" --title "GitLab Runner - Debian 13" --wait
```

This creates a new private template in your account without touching the original server.

### 6. Delete the builder server and its original storage

```sh
upctl server delete --delete-storages gitlab-runner-template
```

### 7. Retrieve the template UUID

```sh
upctl storage list --template
```

Copy the UUID — this is the value to use as `template` in `[runners.autoscaler.plugin_config]`.

### 8. Create SSH key

```sh
ssh-keygen -f ~/.ssh/gitlab-cicd
```

### 9. Register GitLab Runner

Follow the instructions [how to register a GitLab Runner](https://docs.gitlab.com/tutorials/create_register_first_runner/#create-and-register-a-project-runner).

```sh
gitlab-runner register  --url https://gitlab.com --name "UpCloud GitLab Runner" --executor docker-autoscaler --docker-image "alpine:latest"
```

Paste the Token you got from GitLab

## Configuration

Configure `/etc/gitlab-runner/config.toml`

```toml
concurrent = 5
check_interval = 0
connection_max_age = "15m0s"
shutdown_timeout = 0

[session_server]
  session_timeout = 1800

[[runners]]
  name = "UpCloud GitLab Runner"
  url = "https://gitlab.com"
  id = 0
  token = "<your GitLab Runner Token>"
  token_obtained_at = 2026-02-24T11:23:22Z
  token_expires_at = 0001-01-01T00:00:00Z
  executor = "docker-autoscaler"

  [runners.cache]
    MaxUploadedArchiveSize = 0
    [runners.cache.s3]
    [runners.cache.gcs]
    [runners.cache.azure]
  
  [runners.docker]
    tls_verify = false
    image = "alpine:latest"
    privileged = false
    disable_entrypoint_overwrite = false
    oom_kill_disable = false
    disable_cache = false
    volumes = ["/cache"]
    shm_size = 0
    network_mtu = 0
  
  [runners.autoscaler]
    capacity_per_instance = 4
    max_use_count = 60
    max_instances = 5
    plugin = "/root/.config/fleeting/plugins/registry.gitlab.com/gitlab-org/fleeting/plugins/fleeting-plugin-upcloud"
    instance_ready_command = "docker info"
    instance_acquire_timeout = "0s"
    update_interval = "0s"
    update_interval_when_expecting = "0s"
    deletion_retry_interval = "0s"
    shutdown_deletion_interval = "0s"
    shutdown_deletion_retries = 0
    failure_threshold = 0
    delete_instances_on_shutdown = true
    
    [runners.autoscaler.plugin_config]
      # Auth: use a Personal Access Token (recommended) or username + password
      token = "<your UpCloud API Token>"
      # username = "<your UpCloud Username>"
      # password = "<your UpCloud Password>"
      template = "<your UpCloud Custom Image UUID>"
      name = "my-runner-group"
      plan = "4xCPU-8GB"
      storage_size = 20
      storage_tier = "maxiops"
      zone = "fi-hel1"
    
    [runners.autoscaler.connector_config]
      os = "linux"
      arch = "amd64"
      protocol = "ssh"
      protocol_port = 0
      username = "root"
      key_path = "/root/.ssh/gitlab"
      keepalive = "0s"
      timeout = "0s"
      use_external_addr = true
    
    [[runners.autoscaler.policy]]
      periods = ["* * * * *"]
      idle_count = 0
      idle_time = "45m0s"
      scale_factor = 0.0
      scale_factor_limit = 0
      preemptive_mode = false
```

## Plugin config reference

All fields go under `[runners.autoscaler.plugin_config]`.

| Field | Required | Default | Description |
|---|---|---|---|
| `token` | yes* | — | UpCloud Personal Access Token (`ucat_…`) |
| `username` | yes* | — | UpCloud API username (alternative to `token`) |
| `password` | yes* | — | UpCloud API password (required with `username`) |
| `zone` | yes | — | UpCloud zone, e.g. `fi-hel1` |
| `template` | yes | — | UpCloud template UUID to clone for each instance |
| `name` | yes | — | Unique group name used as an UpCloud server label |
| `plan` | no | `1xCPU-2GB` | UpCloud server plan |
| `storage_tier` | no | (from template) | `maxiops` or `standard` |
| `storage_size` | no | (from template) | Storage size in GB |
| `name_prefix` | no | `fleeting` | Prefix for generated hostnames |
| `max_size` | no | `100` | Maximum number of concurrent instances |
| `use_private_network` | no | `false` | Connect via private IP instead of public |
| `user_data` | no | — | URL or inline script for cloud-init on first boot |

\* Either `token` or both `username`+`password` must be provided.

## How it works

On each autoscaler cycle the plugin:

1. **Update** — lists all UpCloud servers tagged with the group label and reports their state to the runner.
2. **Increase** — clones the configured template to spin up new servers, injecting the SSH public key from `connector_config.key_path`.
3. **Decrease** — hard-stops and deletes instances that are no longer needed (in parallel).
4. **ConnectInfo** — returns the public (or private) IPv4 address and SSH details so the runner can connect.

## Contributing

Issues and merge requests are welcome at [gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud).
