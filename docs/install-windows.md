# Windows: install and upgrade

This guide sets up the plugin on a **Windows runner manager** (amd64 or arm64). There, GitLab Runner is [installed as a Windows service](https://docs.gitlab.com/runner/install/windows/), typically from `C:\GitLab-Runner`, and runs as the built-in System account. It reads `config.toml` from that folder.

Before you start, read the [prerequisites](../README.md#prerequisites) and build an [instance template](../README.md#creating-a-custom-upcloud-template). The instances themselves are Linux servers on UpCloud. Only the manager runs Windows.

> Windows support is new. The binaries are built and checksummed like the others, but they haven't been tested on a real Windows runner manager yet. Please [report](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/issues) anything that doesn't work.

Run every command below in an **elevated PowerShell** (Run as administrator).

## Install

### 1. Install the plugin

The install script detects your architecture, downloads the binary from the latest release, verifies its SHA256 checksum, and installs it to `C:\Program Files\fleeting-plugin-upcloud\fleeting-plugin-upcloud.exe`. It works in both Windows PowerShell 5.1 and PowerShell 7:

```powershell
irm https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/scripts/install-plugin.ps1 | iex
```

To install a specific release, or to install somewhere else, set these first:

```powershell
$env:VERSION = 'v1.4.0'
$env:INSTALL_PATH = 'D:\Tools\fleeting-plugin-upcloud.exe'
```

<details>
<summary>Manual download instead</summary>

Download `fleeting-plugin-upcloud-windows-amd64.exe` (or `-arm64.exe`) and its `.sha256` file from the [releases page](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/releases), then:

```powershell
(Get-FileHash .\fleeting-plugin-upcloud-windows-amd64.exe -Algorithm SHA256).Hash   # must match the .sha256 file
New-Item -ItemType Directory -Force "$env:ProgramFiles\fleeting-plugin-upcloud" | Out-Null
Copy-Item .\fleeting-plugin-upcloud-windows-amd64.exe "$env:ProgramFiles\fleeting-plugin-upcloud\fleeting-plugin-upcloud.exe"
```

</details>

### 2. Create the SSH key for reaching instances

Keep the key next to the runner, where the service account can read it. Press Enter twice when asked for a passphrase:

```powershell
New-Item -ItemType Directory -Force C:\GitLab-Runner\.ssh | Out-Null
ssh-keygen -t ed25519 -f C:\GitLab-Runner\.ssh\fleeting-upcloud
```

`ssh-keygen` comes with the OpenSSH client that ships with Windows 10, Windows 11 and Windows Server 2019 or later.

### 3. Register the runner

[Create a runner](https://docs.gitlab.com/tutorials/create_register_first_runner/#create-and-register-a-project-runner) in GitLab to get a runner authentication token (`glrt-…`), then:

```powershell
cd C:\GitLab-Runner
.\gitlab-runner.exe register --non-interactive `
  --url https://gitlab.com `
  --token "<glrt-... runner authentication token>" `
  --name "UpCloud GitLab Runner" `
  --executor docker-autoscaler `
  --docker-image alpine:latest
```

### 4. Configure

Add the autoscaler sections from the [example configuration](../README.md#configuration) to `C:\GitLab-Runner\config.toml`. These lines are Windows-specific:

- **Plugin path:** use the full path. The service doesn't see changes to the system `PATH` until Windows restarts. GitLab Runner accepts a path to an existing file as well as a bare plugin name.
- **Quoting:** use single quotes, which TOML treats as literal strings, so the backslashes don't need escaping.

```toml
[runners.autoscaler]
  plugin = 'C:\Program Files\fleeting-plugin-upcloud\fleeting-plugin-upcloud.exe'

  [runners.autoscaler.connector_config]
    os = "linux"                       # describes the instances, not the manager
    username = "root"
    key_path = 'C:\GitLab-Runner\.ssh\fleeting-upcloud'
```

### 5. Start and verify

```powershell
cd C:\GitLab-Runner
.\gitlab-runner.exe stop; .\gitlab-runner.exe start
& "$env:ProgramFiles\fleeting-plugin-upcloud\fleeting-plugin-upcloud.exe" version
.\gitlab-runner.exe fleeting list
```

`fleeting list` should show the plugin's full path for your runner. The service writes its log to the Windows Event Log (Event Viewer, under **Windows Logs > Application**).

## Upgrade

The runner keeps using the old plugin until it restarts. Windows can't overwrite a running `.exe`, so the installer renames the old one to `fleeting-plugin-upcloud.exe.old-<timestamp>` and puts the new one in its place. Old copies are cleaned up by a later upgrade once nothing uses them. Check the [release notes](https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/releases) for config changes first.

On Windows, stopping the service is graceful: the runner stops taking new jobs and waits for running ones to finish, so `stop` can take a while.

```powershell
# 1. Install the new version (prints "Upgraded: ... (old -> new)")
irm https://gitlab.com/kirbo/gitlab-fleeting-plugin-upcloud/-/raw/main/scripts/install-plugin.ps1 | iex

# 2. Restart
cd C:\GitLab-Runner
.\gitlab-runner.exe stop; .\gitlab-runner.exe start

# 3. Verify
& "$env:ProgramFiles\fleeting-plugin-upcloud\fleeting-plugin-upcloud.exe" version
.\gitlab-runner.exe fleeting list
```

**Roll back:** set `$env:VERSION = '<previous tag>'`, re-run step 1, then restart. Run `Remove-Item Env:VERSION` afterwards so later installs get the latest release again.

## Uninstall

Remove `[runners.autoscaler]` from the config, or unregister the runner, and restart the runner first. Then:

```powershell
Remove-Item -Recurse "$env:ProgramFiles\fleeting-plugin-upcloud"
```
