# Secrets

Only the [telemetry](../README.md#telemetry) badge backend needs secrets: the deploy jobs reach its server over SSH and write its runtime `.env`. Those secrets live in **`fnox.toml`** as [age](https://age-encryption.org) ciphertext and are read with [fnox](https://fnox.jdx.dev). Nothing else stores them: no plaintext in git, and GitLab holds only one age identity. fnox and age are pinned in [`mise.toml`](../mise.toml).

Contributors need none of this. Building, testing and releasing the plugin use no secrets.

## Who can read what

A fnox **provider** encrypts to a fixed set of recipients (public keys), so the provider a secret uses decides who can read it. A **profile** groups the secrets of one job group.

| Profile | Secrets | Provider | Readers |
|---|---|---|---|
| `deploy` | `SSH_KEY` (base64 private key), `SSH_KNOWN_HOSTS`, `TELEMETRY_HOST`, `TELEMETRY_USER` | `age_deploy` | maintainer, deploy |
| `telemetry` | `CLICKHOUSE_URL`, `CLICKHOUSE_USER`, `CLICKHOUSE_PASSWORD`, `APTABASE_APP_ID` (the backend's runtime `.env`) | `age_deploy` | maintainer, deploy |

All keys are post-quantum (`age-keygen -pq`). fnox reaches them through `age-plugin-pq`, which ships with mise's age, so fnox needs each identity in its plugin form (`AGE-PLUGIN-PQ-1…`).

## Identities

| Identity | Where it lives |
|---|---|
| maintainer | 1Password item "fleeting-plugin-upcloud fnox keys" (field `maintainer identity`). On each maintainer machine it also sits in the OS keychain (service `gitlab-fleeting-plugin-upcloud.fnox`), put there by `scripts/secrets.sh setup`. |
| deploy | The same 1Password item (field `deploy identity`), and GitLab: File variable `FNOX_AGE_KEY_FILE`, environment scope `release/deploy`, protected and masked. |

`fnox.toml` names no identity, so it works unchanged everywhere. The gitignored `fnox.local.toml` is **generated** by `scripts/secrets.sh` and points every age provider at the keychain. fnox merges a provider whole, so that file repeats each provider's recipients: after pulling a recipient change, run `scripts/secrets.sh refresh-local`.

## Setting it up (once)

Needs an activated mise, the 1Password CLI (`op`) and an authenticated `glab`.

1. `scripts/secrets.sh init-keys` generates both identities, stores them in 1Password, writes their public keys into `fnox.toml`, puts the maintainer identity in the keychain and the deploy identity in GitLab.
2. Add the values, each read from stdin:

   ```sh
   base64 < ~/.ssh/deploy_key | tr -d '\n' | scripts/secrets.sh set deploy SSH_KEY
   ssh-keyscan -t ed25519 <host>            # check the key by hand, then:
   printf '%s' '<host key line>' | scripts/secrets.sh set deploy SSH_KNOWN_HOSTS
   printf '%s' '<host>'          | scripts/secrets.sh set deploy TELEMETRY_HOST
   printf '%s' '<user>'          | scripts/secrets.sh set deploy TELEMETRY_USER
   printf '%s' '<url>'           | scripts/secrets.sh set telemetry CLICKHOUSE_URL
   # … CLICKHOUSE_USER, CLICKHOUSE_PASSWORD, APTABASE_APP_ID (the app's GUID in Aptabase)
   ```

   The server, its SSH key and ClickHouse are shared with electrolux-to-mqtt, so you can copy those values from there without them touching the screen or disk, e.g.:

   ```sh
   for n in SSH_KEY SSH_KNOWN_HOSTS TELEMETRY_HOST TELEMETRY_USER; do
     (cd ../electrolux-to-mqtt && fnox get --no-defaults -P deploy "$n") | scripts/secrets.sh set deploy "$n"
   done
   for n in CLICKHOUSE_URL CLICKHOUSE_USER CLICKHOUSE_PASSWORD; do
     (cd ../electrolux-to-mqtt && fnox get --no-defaults -P telemetry "$n") | scripts/secrets.sh set telemetry "$n"
   done
   ```

3. `scripts/secrets.sh check`, then commit `fnox.toml`. The push runs `deploy-telemetry`.

On another maintainer machine, only `scripts/secrets.sh setup` is needed.

## Commands

| Command | What |
|---|---|
| `scripts/secrets.sh setup` | Once per machine: maintainer identity 1Password → keychain, generate `fnox.local.toml`, check |
| `scripts/secrets.sh check` | Every profile decrypts |
| `scripts/secrets.sh set <profile> <NAME>` | Encrypt one value (stdin) into that profile |
| `scripts/secrets.sh env` | Write `telemetry/.env` (0600, gitignored) to run the backend locally |
| `scripts/secrets.sh sync-ci` | Push the deploy identity 1Password → GitLab again |
| `scripts/secrets.sh rotate maintainer\|deploy` | New key: 1Password first, then recipients and re-encryption, then keychain / GitLab. Commit `fnox.toml` afterwards. |
| `fnox exec --no-defaults -P <profile> -- <cmd>` | Anything else |

## CI

`deploy-telemetry` and `refresh-telemetry-badges` run in environment `release/deploy` (`action: prepare`), so GitLab gives them the deploy identity as `FNOX_AGE_KEY_FILE`. Because the variable is protected, only `main` pipelines get it. The jobs install fnox and age with mise and source [`ci/secrets.sh`](../ci/secrets.sh), which provides `secrets_load <profile>…` (export into the job shell), `secrets_export <profile>` (a dotenv on stdout) and `deploy_ssh_setup`.

- The SSH key goes into `ssh-agent`, never a file. The host key is pinned (`SSH_KNOWN_HOSTS`, no `ssh-keyscan` in CI).
- The backend's `.env` is piped over ssh into a 0600 file on the server, so it is never written on the runner.
- GitLab masks none of these values, so jobs never `set -x` or echo them.

## Rotation and leaks

Any `fnox.toml` change on `main` re-runs `deploy-telemetry`, so a rotated `telemetry` value reaches the server's `.env` on push.

Re-encrypting does not revoke ciphertext already in git history. If an identity leaks, rotate it **and** change the values it could read (`scripts/secrets.sh set`), then commit. 1Password is the backup of record for both identities.
