#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf -- "${test_root}"' EXIT

fake_bin="${test_root}/bin"
mkdir -p "${fake_bin}"

cat >"${fake_bin}/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail

{
  printf 'docker'
  printf '\t%s' "$@"
  printf '\n'
} >>"${DOCKER_CALL_LOG}"

if [[ "${1:-}" == "image" && "${2:-}" == "inspect" ]]; then
  case "${4:-}" in
    '{{.Id}}') printf '%s\n' 'sha256:test-image-id' ;;
    '{{json .RepoTags}}') printf '["%s"]\n' "${IMAGE_REF}" ;;
    '{{.Config.User}}') printf '%s\n' 'nonroot:nonroot' ;;
    '{{json .Config.Entrypoint}}') printf '%s\n' '["/app/moviepilot-go"]' ;;
    '{{json .Config.Cmd}}') printf '%s\n' 'null' ;;
    *) printf 'unexpected image inspect format: %s\n' "${4:-}" >&2; exit 2 ;;
  esac
  exit 0
fi

if [[ "${1:-}" == "container" && "${2:-}" == "ls" ]]; then
  exit 0
fi

if [[ "${1:-}" == "create" ]]; then
  printf '%s\n' 'test-container-id'
  exit 0
fi

if [[ "${1:-}" == "start" ]]; then
  printf '%s\n' 'intentionally-invalid-version'
  exit 0
fi

printf 'unexpected docker invocation: %s\n' "$*" >&2
exit 2
EOF
chmod +x "${fake_bin}/docker"

for tool in readelf go syft; do
  cat >"${fake_bin}/${tool}" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  chmod +x "${fake_bin}/${tool}"
done

release_file="${test_root}/frontend-release.env"
cat >"${release_file}" <<'EOF'
FRONTEND_VERSION=v2.15.6
FRONTEND_SHA256=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
EOF

run_case() {
  local name="$1"
  local parent_run="$2"
  local call_log="${test_root}/${name}.calls"
  local output_file="${test_root}/${name}.output"
  local image_ref="moviepilot-go:image-content-${name}"
  local status=0

  : >"${call_log}"
  set +e
  PATH="${fake_bin}:${PATH}" \
    DOCKER_CALL_LOG="${call_log}" \
    FRONTEND_RELEASE_FILE="${release_file}" \
    IMAGE_REF="${image_ref}" \
    VERIFY_IMAGE_BUILD=0 \
    IMAGE_GATE_PARENT_RUN="${parent_run}" \
    bash "${repo_root}/scripts/ci/verify-image.sh" >"${output_file}" 2>&1
  status=$?
  set -e

  [[ "${status}" -ne 0 ]] || {
    printf '%s: verifier unexpectedly succeeded\n' "${name}" >&2
    return 1
  }
  grep -Fq $'docker\tcreate\t' "${call_log}" || {
    printf '%s: verifier did not reach docker create\n' "${name}" >&2
    cat "${output_file}" >&2
    return 1
  }
  grep -Fq -- '--version returned an invalid value' "${output_file}" || {
    printf '%s: verifier failed before the intended stop point\n' "${name}" >&2
    cat "${output_file}" >&2
    return 1
  }

  if [[ -n "${parent_run}" ]]; then
    grep -Fq "com.moviepilot.image-gate.parent-run=${parent_run}" "${call_log}" || {
      printf '%s: parent label is missing\n' "${name}" >&2
      return 1
    }
  elif grep -Fq 'com.moviepilot.image-gate.parent-run=' "${call_log}"; then
    printf '%s: standalone verifier unexpectedly received a parent label\n' "${name}" >&2
    return 1
  fi
}

run_case standalone ''
run_case nested parent-run-123

printf '%s\n' 'verify-image Bash helper tests passed'
