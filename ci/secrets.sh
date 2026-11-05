# shellcheck shell=sh
# Sourced by CI jobs that need a secret: `. ci/secrets.sh`, then `secrets_load <profile>…`
# or `secrets_export <profile>` for a dotenv on stdout.
#
# Every CI secret lives in fnox.toml as age ciphertext, encrypted only to the CI tier that
# may read it (docs/SECRETS.md). The job's GitLab environment (release/deploy) scopes ONE
# protected File-type variable to it: FNOX_AGE_KEY_FILE, that tier's age identity. fnox
# decrypts with it; GitLab stores nothing else.
#
# fnox and age (whose age-plugin-pq fnox needs for the post-quantum keys) come from
# mise.toml: the job runs `mise install fnox age` first.
#
# GitLab masks none of these values in the job log (they are not GitLab variables):
# never `set -x`, never echo them.

export FNOX_NON_INTERACTIVE=true

ci_fnox() { mise exec fnox age -- fnox "$@"; }

# secrets_available — whether this job was given an identity (protected ref, environment
# set, FNOX_AGE_KEY_FILE configured in GitLab).
secrets_available() { [ -n "${FNOX_AGE_KEY_FILE:-}" ]; }

_secrets_require_identity() {
  if ! secrets_available; then
    echo "secrets: no FNOX_AGE_KEY_FILE in this job: does environment ${CI_ENVIRONMENT_NAME:-none} have one in GitLab (scripts/secrets.sh sync-ci), and is the ref protected?" >&2
    exit 1
  fi
}

# secrets_load <profile>… — export the named profiles' secrets into THIS shell. Exits the
# job when the identity cannot decrypt them (a wrong tier is a bug).
secrets_load() {
  _secrets_require_identity
  _sp=$(printf '%s' "$*" | tr ' ' ',')
  if ! _so=$(ci_fnox export --format shell --no-defaults --if-missing error -P "${_sp}"); then
    echo "secrets: this job's identity cannot decrypt fnox profile(s): $*" >&2
    exit 1
  fi
  eval "${_so}"
  unset _so _sp
  echo "secrets: loaded fnox profile(s): $*"
}

# secrets_export <profile> — print the profile as a dotenv (pipe it straight on).
secrets_export() {
  _secrets_require_identity
  ci_fnox export --format env --no-defaults --if-missing error -P "$1"
}

# deploy_ssh_setup — load the `deploy` profile and make the badge backend's server
# reachable: the key goes straight into ssh-agent (never a file), the host key is pinned
# (SSH_KNOWN_HOSTS, no ssh-keyscan trust-on-first-use). Sets SERVER=user@host.
deploy_ssh_setup() {
  secrets_load deploy
  eval "$(ssh-agent -s)" >/dev/null
  printf '%s' "${SSH_KEY}" | base64 -d | ssh-add - >/dev/null
  unset SSH_KEY
  mkdir -p ~/.ssh && chmod 700 ~/.ssh
  printf '%s\n' "${SSH_KNOWN_HOSTS}" >~/.ssh/known_hosts
  export SERVER="${TELEMETRY_USER}@${TELEMETRY_HOST}"
}
