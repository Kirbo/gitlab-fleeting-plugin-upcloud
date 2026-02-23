# fleeting-plugin-upcloud

A [GitLab fleeting](https://gitlab.com/gitlab-org/fleeting/fleeting) plugin that provisions ephemeral [UpCloud](https://upcloud.com/) servers as GitLab CI runner instances via the `docker-autoscaler` executor.

## Installation

### Quick install (recommended)

The install script detects your OS and architecture automatically and downloads the correct binary from the latest release:

```sh
curl -fsSL https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/install.sh | bash
```

By default the binary is installed to `~/.gitlab-runner/plugins/`. Override with `INSTALL_DIR`:

```sh
INSTALL_DIR=/usr/local/bin curl -fsSL https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/install.sh | bash
```

### Manual download

Download the binary for your platform from the [releases page](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/releases), place it in `~/.gitlab-runner/plugins/`, and make it executable:

```sh
chmod +x ~/.gitlab-runner/plugins/fleeting-plugin-upcloud
```

#### macOS: ad-hoc sign the binary

On macOS (especially Apple Silicon) the runner will refuse to execute an unsigned binary. After placing it, sign it with an ad-hoc identity:

```sh
codesign --sign - ~/.gitlab-runner/plugins/fleeting-plugin-upcloud
```

If you installed to a different path, adjust accordingly.

### Adding the install directory to PATH

The default install directory (`~/.gitlab-runner/plugins/`) is intentionally **not** on PATH — GitLab Runner finds the plugin by name without needing it there. However, if you want to invoke the binary directly from your shell (e.g. for debugging), add the directory to your shell's startup file:

**bash** (`~/.bashrc` or `~/.bash_profile`):
```sh
export PATH="$HOME/.gitlab-runner/plugins:$PATH"
```

**zsh** (`~/.zshrc`):
```sh
export PATH="$HOME/.gitlab-runner/plugins:$PATH"
```

Then reload the file or open a new terminal:
```sh
source ~/.zshrc   # or ~/.bashrc
```

If you installed to a custom `INSTALL_DIR`, substitute that path instead.

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
  echo -e "\rCurrent state: ${STATE}"
  if [[ "${STATE}" == "started" ]]; then
    echo -e "\nServer started successfully."
    break
  elif (( TRIES >= 60 )); then
    echo -e "\nAborted due maximum 60 tries."
    break
  fi

  sleep 5
  ((TRIES++))
done
```

### 3. Prepare the server and shut it down

```sh
ssh root@$(upctl server show gitlab-runner-template -o json | jq -r '.ip_addresses[0].address') \
  "curl -fsSL 'https://gist.githubusercontent.com/Kirbo/a037f984643a18ae037089b6c0305e79/raw' | /bin/bash -s --"
```

### 4. Wait until the server has stopped

```bash
while true; do
  STATE=$(upctl server show gitlab-runner-template -o json | jq -r '.state')
  echo -e "\rCurrent state: ${STATE}"
  if [[ "${STATE}" == "stopped" ]]; then
    echo -e "\nServer stopped successfully."
    break
  elif (( TRIES >= 60 )); then
    echo -e "\nAborted due maximum 60 tries."
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

### 5. Delete the builder server and its original storage

```sh
upctl server delete --delete-storages gitlab-runner-template
```

### 6. Retrieve the template UUID

```sh
upctl storage list --template
```

Copy the UUID — this is the value to use as `template` in `[runners.autoscaler.plugin_config]`.

## Configuration

Add the plugin to your `~/.gitlab-runner/config.toml`. The key sections are `executor = "docker-autoscaler"`, the `[runners.autoscaler]` block pointing to the plugin, and the `[runners.autoscaler.plugin_config]` block with your UpCloud credentials and server settings.

```toml
concurrent = 5
check_interval = 0
connection_max_age = "15m0s"
shutdown_timeout = 0

[[runners]]
  name = "my-runner"
  url = "https://gitlab.com"
  id = 0
  token = "<your-runner-token>"
  token_obtained_at = 2025-01-01T00:00:00Z
  token_expires_at = 0001-01-01T00:00:00Z
  tags = ["docker", "linux"]
  executor = "docker-autoscaler"
  request_concurrency = 5

  [runners.docker]
    image       = "alpine:latest"
    privileged  = true

  [runners.cache]
    [runners.cache.s3]
    [runners.cache.gcs]
    [runners.cache.azure]

  [runners.autoscaler]
    plugin                  = "fleeting-plugin-upcloud"
    instance_ready_command  = "docker info"
    capacity_per_instance   = 4
    max_use_count           = 60
    max_instances           = 5

  [[runners.autoscaler.policy]]
    idle_count = 0
    idle_time  = "45m0s"
    periods    = ["* * * * *"]

  [runners.autoscaler.plugin_config]
    # Auth: use a Personal Access Token (recommended) or username + password
    token    = "<your-upcloud-api-token>"
    # username = "<your-upcloud-username>"
    # password = "<your-upcloud-password>"

    zone     = "fi-hel1"         # UpCloud zone
    template = "<storage-uuid>"  # Custom Image UUID
    name     = "my-runner-group" # unique label for this runner group

    # Optional
    plan         = "4xCPU-8GB"   # default: "1xCPU-2GB"
    storage_tier = "maxiops"     # "maxiops" or "standard"; default: inherit from template
    storage_size = 40            # GB; default: inherit from template
    name_prefix  = "fleeting"    # hostname prefix; default: "fleeting"
    max_size     = 10            # hard cap on concurrent instances; default: 100

  [runners.autoscaler.connector_config]
    os                = "linux"
    arch              = "amd64"
    protocol          = "ssh"
    use_external_addr = true
    username          = "root"
    key_path          = "/home/<your-user>/.gitlab-runner/keys/<key-name>"
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
