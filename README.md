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

### 1. Find the Debian 13 base template UUID

```sh
upctl storage list --public | grep -i debian
```

Note the UUID of the Debian 13 entry — you will use it in the next step.

### 2. Create a builder server

```sh
upctl server create \
  --zone fi-hel1 \
  --title "docker-template-builder" \
  --hostname "docker-template-builder" \
  --plan 2xCPU-4GB \
  --storage action=clone,storage=<debian-13-template-uuid>,size=30,tier=standard \
  --network family=IPv4,type=public \
  --ssh-keys ~/.ssh/id_rsa.pub
```

Or try to use this oneliner:

```sh
upctl server create \
  --zone fi-hel1 \
  --title "docker-template-builder" \
  --hostname "docker-template-builder" \
  --plan 2xCPU-4GB \
  --storage action=clone,storage=$(upctl storage list --public | grep -i trixie | grep -i template | awk '{print $1}'),size=30,tier=standard \
  --network family=IPv4,type=public \
  --ssh-keys ~/.ssh/id_rsa.pub
```

Wait until the server is in the `started` state and note its UUID and public IP:

```sh
upctl server show docker-template-builder
```

### 3. Install Docker

```sh
ssh root@<server-ip> "curl -fsSL https://get.docker.com | sh && systemctl enable docker"
```

Add any other packages your CI jobs require (e.g. `git`, `make`, language runtimes). Once you are done, exit the SSH session.

### 4. Stop the server cleanly

```sh
upctl server stop --type soft --wait docker-template-builder
```

### 5. Find the server's storage device UUID

```sh
upctl server show docker-template-builder
```

Under the **Storage devices** section, note the UUID of the boot disk.

### 6. Templatize the storage

```sh
upctl storage templatise <storage-uuid> --title "docker-runner-template"
```

This creates a new private template in your account without touching the original server.

### 7. Delete the builder server and its original storage

```sh
upctl server delete --delete-storages docker-template-builder
```

### 8. Retrieve the template UUID

```sh
upctl storage list --template | grep docker-runner-template
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
    image = "alpine:latest"
    privileged = true

  [runners.cache]
    [runners.cache.s3]
    [runners.cache.gcs]
    [runners.cache.azure]

  [runners.autoscaler]
    plugin = "fleeting-plugin-upcloud"

    # Wait until Docker is ready before accepting jobs.
    # Use one of the two options below:

    # If you use the Custom Image
    #   instance_ready_command = "docker info"

    # If you use one of the Official Images
    #   instance_ready_command = "timeout 300 bash -c 'until [ -f /var/run/docker-ready ]; do sleep 5; done'"

    capacity_per_instance = 1
    max_use_count         = 10
    max_instances         = 5

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
    template = "<storage-uuid>"  # OS template UUID
    name     = "my-runner-group" # unique label for this runner group

    # Optional
    plan         = "1xCPU-2GB"   # default: "1xCPU-2GB"
    storage_tier = "standard"    # "maxiops" or "standard"; default: inherit from template
    storage_size = 30            # GB; default: inherit from template
    name_prefix  = "fleeting"    # hostname prefix; default: "fleeting"
    max_size     = 100           # hard cap on concurrent instances; default: 100

    # If you want to use Initialization Script (e.g. if you use Official Images)
    # Cloud-init: URL to a user-data script run on first boot, or an inline script body
    # Example can be found in https://gist.github.com/Kirbo/ce516834616930a8c7b35082b8ff5627
    # user_data = "https://example.com/your-init-script.sh"

  [runners.autoscaler.connector_config]
    username          = "root"
    key_path          = "/home/<your-user>/.gitlab-runner/keys/<key-name>"
    protocol          = "ssh"
    os                = "linux"
    arch              = "amd64"
    use_external_addr = true
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
