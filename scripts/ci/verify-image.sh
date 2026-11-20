#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
release_file="${FRONTEND_RELEASE_FILE:-${repo_root}/deploy/frontend-release.env}"
run_nonce="${BASHPID:-$$}-${RANDOM}-${RANDOM}"
parent_run="${IMAGE_GATE_PARENT_RUN:-}"
environment_marker="moviepilot-go-image-gate-v1"
image_ref="${IMAGE_REF:-}"
sbom_file="${SBOM_FILE:-${repo_root}/artifacts/moviepilot-go.cdx.json}"
expected_image_id="${EXPECTED_IMAGE_ID:-}"
tmp_dir=""
container_name="moviepilot-image-verify-${run_nonce}"
verification_image_id=""
verification_succeeded=0

fail() {
  printf 'image verification failed: %s\n' "$*" >&2
  exit 1
}

cleanup() {
  local status=$?
  local cleanup_failed=0
  local cleanup_message=""
  local container_labels=""
  local container_ids=""
  local container_list_status=0
  trap - EXIT
  set +e

  container_ids="$(docker container ls --all --quiet --no-trunc --filter "name=^/${container_name}$" 2>/dev/null)"
  container_list_status=$?
  if [[ "${container_list_status}" -ne 0 ]]; then
    cleanup_failed=1
    cleanup_message+=" container-list(${container_name})"
  elif [[ -n "${container_ids}" ]]; then
    if [[ "$(printf '%s\n' "${container_ids}" | sed '/^$/d' | wc -l | tr -d ' ')" -ne 1 ]]; then
      cleanup_failed=1
      cleanup_message+=" container-not-unique(${container_name})"
    else
      container_labels="$(docker container inspect --format '{{index .Config.Labels "com.moviepilot.image-gate.environment"}} {{index .Config.Labels "com.moviepilot.image-gate.run"}} {{index .Config.Labels "com.moviepilot.image-gate.kind"}} {{with index .Config.Labels "com.moviepilot.image-gate.parent-run"}}{{.}}{{end}}' "${container_ids}" 2>/dev/null)"
      if [[ $? -ne 0 ]]; then
        cleanup_failed=1
        cleanup_message+=" container-inspect(${container_name})"
      elif [[ "${container_labels}" != "${environment_marker} ${run_nonce} verifier ${parent_run}" ]]; then
        cleanup_failed=1
        cleanup_message+=" container-identity-mismatch(${container_name})"
      elif ! docker container rm --force "${container_ids}" >/dev/null 2>&1; then
        cleanup_failed=1
        cleanup_message+=" container(${container_name})"
      fi
    fi
  fi
  container_ids="$(docker container ls --all --quiet --no-trunc --filter "name=^/${container_name}$" 2>/dev/null)"
  container_list_status=$?
  if [[ "${container_list_status}" -ne 0 ]]; then
    cleanup_failed=1
    cleanup_message+=" container-post-list(${container_name})"
  elif [[ -n "${container_ids}" ]]; then
    cleanup_failed=1
    cleanup_message+=" container-remains(${container_name})"
  fi

  if [[ -n "${tmp_dir}" ]]; then
    if ! rm -rf -- "${tmp_dir}"; then
      cleanup_failed=1
      cleanup_message+=" temp-dir(${tmp_dir})"
    fi
  fi
  if [[ "${cleanup_failed}" -ne 0 ]]; then
    printf 'image verification cleanup failed:%s\n' "${cleanup_message}" >&2
    [[ "${status}" -ne 0 ]] || status=1
  fi
  if [[ "${status}" -eq 0 && "${verification_succeeded}" -eq 1 ]]; then
    printf 'verified image %s (frontend %s); SBOM: %s\n' "${image_ref}" "${FRONTEND_VERSION}" "${sbom_file}"
  fi
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

command -v docker >/dev/null || fail "docker is required"
command -v readelf >/dev/null || fail "readelf is required"
command -v go >/dev/null || fail "go is required"
command -v syft >/dev/null || fail "syft is required"
[[ -n "${image_ref}" ]] || fail "IMAGE_REF is required"
[[ -f "${release_file}" ]] || fail "release metadata not found: ${release_file}"
if [[ -n "${parent_run}" && ! "${parent_run}" =~ ^[a-z0-9][a-z0-9.-]{0,63}$ ]]; then
  fail "IMAGE_GATE_PARENT_RUN has an invalid format"
fi

FRONTEND_VERSION=""
FRONTEND_SHA256=""
while IFS= read -r line || [[ -n "${line}" ]]; do
  line="${line%$'\r'}"
  [[ -z "${line}" || "${line}" == \#* ]] && continue
  case "${line}" in
    FRONTEND_VERSION=*)
      [[ -z "${FRONTEND_VERSION}" ]] || fail "duplicate FRONTEND_VERSION"
      FRONTEND_VERSION="${line#*=}"
      ;;
    FRONTEND_SHA256=*)
      [[ -z "${FRONTEND_SHA256}" ]] || fail "duplicate FRONTEND_SHA256"
      FRONTEND_SHA256="${line#*=}"
      ;;
    *) fail "unexpected release metadata: ${line%%=*}" ;;
  esac
done < "${release_file}"

[[ "${FRONTEND_VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "invalid FRONTEND_VERSION"
[[ "${FRONTEND_SHA256}" =~ ^[0-9a-f]{64}$ ]] || fail "invalid FRONTEND_SHA256"

verification_image_id="$(docker image inspect --format '{{.Id}}' "${image_ref}")"

if [[ -n "${expected_image_id}" && "${verification_image_id}" != "${expected_image_id}" ]]; then
  fail "image ID does not match EXPECTED_IMAGE_ID"
fi
[[ -n "${verification_image_id}" ]] || fail "image has no inspectable ID"
if [[ "${image_ref}" == *@sha256:* ]]; then
  image_digests="$(docker image inspect --format '{{json .RepoDigests}}' "${verification_image_id}")"
  [[ "${image_digests}" == *"\"${image_ref}\""* ]] || fail "image is not present under the requested digest"
else
  image_tags="$(docker image inspect --format '{{json .RepoTags}}' "${verification_image_id}")"
  [[ "${image_tags}" == *"\"${image_ref}\""* ]] || fail "image is not present under the requested tag"
fi

tmp_dir="$(mktemp -d)"

image_user="$(docker image inspect --format '{{.Config.User}}' "${verification_image_id}")"
[[ "${image_user}" == "nonroot:nonroot" ]] || fail "image user is ${image_user:-unset}, want nonroot:nonroot"

entrypoint_json="$(docker image inspect --format '{{json .Config.Entrypoint}}' "${verification_image_id}")"
[[ "${entrypoint_json}" == '["/app/moviepilot-go"]' ]] || fail "image must have exactly one application entrypoint"
image_cmd_json="$(docker image inspect --format '{{json .Config.Cmd}}' "${verification_image_id}")"
[[ "${image_cmd_json}" == "null" || "${image_cmd_json}" == "[]" ]] || fail "image must not add a second command"

if ! preexisting_container_ids="$(docker container ls --all --quiet --no-trunc --filter "name=^/${container_name}$")"; then
  fail "cannot query Docker before creating ${container_name}"
fi
if [[ -n "${preexisting_container_ids}" ]]; then
  fail "refusing to reuse existing verifier container: ${container_name}"
fi
create_args=(
  --name "${container_name}"
  --label "com.moviepilot.image-gate.environment=${environment_marker}"
  --label "com.moviepilot.image-gate.run=${run_nonce}"
  --label "com.moviepilot.image-gate.kind=verifier"
)
if [[ -n "${parent_run}" ]]; then
  create_args+=(--label "com.moviepilot.image-gate.parent-run=${parent_run}")
fi
create_args+=(
  --network none \
  --read-only \
  --cap-drop ALL \
  --security-opt no-new-privileges=true
)
docker create "${create_args[@]}" "${verification_image_id}" --version >/dev/null
version_output="$(docker start --attach "${container_name}")"
version_line="$(printf '%s\n' "${version_output}" | sed '/^[[:space:]]*$/d' | tail -n 1)"
[[ "${version_line}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || fail "--version returned an invalid value"

docker export --output "${tmp_dir}/rootfs.tar" "${container_name}"
mkdir "${tmp_dir}/rootfs"
tar --extract --file "${tmp_dir}/rootfs.tar" --directory "${tmp_dir}/rootfs" --no-same-owner --no-same-permissions

frontend_version_file="${tmp_dir}/rootfs/app/web/dist/version.txt"
[[ -f "${frontend_version_file}" ]] || fail "frontend version file is missing"
installed_frontend_version="$(tr -d '\r\n' < "${frontend_version_file}")"
[[ "${installed_frontend_version}" == "${FRONTEND_VERSION}" ]] || fail "frontend version does not match release metadata"
[[ ! -e "${tmp_dir}/rootfs/app/web/dist/nginx.conf" ]] || fail "frontend-only nginx.conf leaked into the runtime image"

for forbidden_path in \
  usr/lib/python usr/local/lib/python usr/local/lib/node_modules \
  etc/nginx usr/share/nginx root/.cargo root/.rustup; do
  [[ ! -e "${tmp_dir}/rootfs/${forbidden_path}" ]] || fail "forbidden runtime path found: /${forbidden_path}"
done
while IFS= read -r -d '' path; do
  case "${path#${tmp_dir}/rootfs}" in
    /usr/lib/python*|/usr/local/lib/python*)
      fail "forbidden Python runtime path found: ${path#${tmp_dir}/rootfs}"
      ;;
  esac
done < <(find "${tmp_dir}/rootfs/usr" -mindepth 1 -maxdepth 4 -print0 2>/dev/null || true)

while IFS= read -r -d '' path; do
  [[ -x "${path}" ]] || continue
  name="$(basename "${path}")"
  name="${name,,}"
  case "${name}" in
    python|python[0-9]*|pypy|pypy[0-9]*|node|nodejs|npm|npx|corepack|nginx|rustc|cargo)
      fail "forbidden runtime executable found: ${path#${tmp_dir}/rootfs}"
      ;;
  esac
done < <(find "${tmp_dir}/rootfs" \( -type f -o -type l \) -print0)

elf_files=()
while IFS= read -r -d '' path; do
  if readelf --file-header "${path}" >/dev/null 2>&1; then
    elf_files+=("${path}")
  fi
done < <(find "${tmp_dir}/rootfs" -type f -print0)

app_binary="${tmp_dir}/rootfs/app/moviepilot-go"
[[ "${#elf_files[@]}" -eq 1 && "${elf_files[0]}" == "${app_binary}" ]] || {
  printf 'ELF files found:\n' >&2
  printf '  %s\n' "${elf_files[@]#${tmp_dir}/rootfs}" >&2
  fail "runtime image must contain only the Go application ELF"
}
go version -m "${app_binary}" >/dev/null 2>&1 || fail "application ELF is not a Go binary"
program_headers="$(readelf --program-headers --wide "${app_binary}")"
if grep -q '[[:space:]]INTERP[[:space:]]' <<< "${program_headers}"; then
  fail "application ELF has a dynamic interpreter"
fi

mkdir -p "$(dirname "${sbom_file}")"
syft "docker:${verification_image_id}" --output cyclonedx-json > "${sbom_file}"
[[ -s "${sbom_file}" ]] || fail "SBOM was not generated"

final_image_id="$(docker image inspect --format '{{.Id}}' "${image_ref}")"
[[ "${final_image_id}" == "${verification_image_id}" ]] || fail "image tag changed during verification"
verification_succeeded=1
