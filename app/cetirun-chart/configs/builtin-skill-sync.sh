#!/bin/sh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Synchronize image-provided CetiRun skills into a persistent agent home without
# overwriting a skill that may have been customized by the instance owner.
#
# A managed skill has a .cetirun-managed-skill manifest. Its payload checksum
# covers the complete directory tree except the manifest itself. Only a target
# whose own manifest and payload both validate can be replaced on an image
# upgrade. A missing or invalid manifest is treated as a user-owned conflict and
# is deliberately preserved with a warning.

CETIRUN_MANAGED_SKILL_MANIFEST=".cetirun-managed-skill"

cetirun_skill_checksum() {
  skill_dir="$1"
  # Python is part of the shared runtime base. Keep path handling byte-safe and
  # print a checksum only after the complete tree has been validated. The
  # manifest format is line-oriented, so names containing its record
  # delimiters are rejected rather than serialized ambiguously.
  python3 - "$skill_dir" "$CETIRUN_MANAGED_SKILL_MANIFEST" <<'PY'
import hashlib
import os
import stat
import sys


root = os.fsencode(sys.argv[1])
manifest_name = os.fsencode(sys.argv[2])
entries = []


def fail_walk(error):
    raise error


try:
    root_metadata = os.lstat(root)
    if not stat.S_ISDIR(root_metadata.st_mode):
        raise ValueError("managed skill root is not a directory")
    for current, directories, files in os.walk(
        root, topdown=True, onerror=fail_walk, followlinks=False
    ):
        names = list(directories) + list(files)
        for name in names:
            if name == manifest_name:
                continue
            path = os.path.join(current, name)
            relative = b"./" + os.path.relpath(path, root)
            if b"\n" in relative or b"\r" in relative:
                raise ValueError("managed skill path contains a record delimiter")
            metadata = os.lstat(path)
            if stat.S_ISDIR(metadata.st_mode):
                entries.append((relative, b"d", path))
            elif stat.S_ISREG(metadata.st_mode):
                entries.append((relative, b"f", path))
            else:
                raise ValueError("managed skill contains an unsupported entry")

    payload = hashlib.sha256()
    nofollow = getattr(os, "O_NOFOLLOW", 0)
    for relative, kind, path in sorted(entries, key=lambda entry: entry[0]):
        if kind == b"d":
            payload.update(b"d " + relative + b"\n")
            continue
        content = hashlib.sha256()
        descriptor = os.open(path, os.O_RDONLY | nofollow)
        try:
            if not stat.S_ISREG(os.fstat(descriptor).st_mode):
                raise ValueError("managed skill file changed during validation")
            while True:
                chunk = os.read(descriptor, 1024 * 1024)
                if not chunk:
                    break
                content.update(chunk)
        finally:
            os.close(descriptor)
        payload.update(
            b"f " + relative + b" " + content.hexdigest().encode("ascii") + b"\n"
        )
except (OSError, ValueError):
    sys.exit(1)

print(payload.hexdigest())
PY
}

cetirun_managed_skill_metadata() {
  skill_dir="$1"
  manifest="$skill_dir/$CETIRUN_MANAGED_SKILL_MANIFEST"
  [ -f "$manifest" ] && [ ! -L "$manifest" ] || return 1

  manifest_format=''
  manifest_skill=''
  manifest_version=''
  manifest_checksum=''
  while IFS= read -r manifest_line || [ -n "$manifest_line" ]; do
    case "$manifest_line" in
      format=*)
        [ -z "$manifest_format" ] || return 1
        manifest_format=${manifest_line#format=}
        ;;
      skill=*)
        [ -z "$manifest_skill" ] || return 1
        manifest_skill=${manifest_line#skill=}
        ;;
      version=*)
        [ -z "$manifest_version" ] || return 1
        manifest_version=${manifest_line#version=}
        ;;
      content_sha256=*)
        [ -z "$manifest_checksum" ] || return 1
        manifest_checksum=${manifest_line#content_sha256=}
        ;;
      previous_content_sha256=*)
        printf '%s' "${manifest_line#previous_content_sha256=}" | grep -Eq '^[0-9a-f]{64}$' || return 1
        ;;
      *) return 1 ;;
    esac
  done < "$manifest"
  [ "$manifest_format" = "1" ] || return 1
  printf '%s' "$manifest_skill" | grep -Eq '^[a-z0-9]+(-[a-z0-9]+)*$' || return 1
  case "$manifest_version" in
    ''|*[!0-9]*) return 1 ;;
  esac
  printf '%s' "$manifest_checksum" | grep -Eq '^[0-9a-f]{64}$' || return 1
  printf '%s|%s|%s\n' "$manifest_skill" "$manifest_version" "$manifest_checksum"
}

# A target must match an image-known payload revision, not merely carry a
# self-consistent manifest from the writable PVC. This prevents a user-created
# same-name skill from opting itself into replacement by adding a manifest.
cetirun_is_recognized_managed_checksum() {
  source_skill="$1"
  candidate_checksum="$2"
  current_checksum="$3"
  [ "$candidate_checksum" = "$current_checksum" ] && return 0
  sed -n 's/^previous_content_sha256=//p' \
    "$source_skill/$CETIRUN_MANAGED_SKILL_MANIFEST" \
    | grep -Fx -- "$candidate_checksum" >/dev/null 2>&1
}

cetirun_install_managed_skill() {
  source_skill="$1"
  destination_skill="$2"
  skill_directory_name="$3"

  source_metadata=$(cetirun_managed_skill_metadata "$source_skill") || {
    echo "[cetirun-builtins] WARNING: image skill ${skill_directory_name} has an invalid managed manifest; no PVC skill was changed." >&2
    return 0
  }
  source_skill_id=${source_metadata%%|*}
  source_metadata_rest=${source_metadata#*|}
  source_version=${source_metadata_rest%%|*}
  source_checksum=${source_metadata_rest#*|}
  source_actual_checksum=$(cetirun_skill_checksum "$source_skill") || {
    echo "[cetirun-builtins] WARNING: image skill ${skill_directory_name} could not be checksummed; no PVC skill was changed." >&2
    return 0
  }
  [ "$source_actual_checksum" = "$source_checksum" ] || {
    echo "[cetirun-builtins] WARNING: image skill ${skill_directory_name} checksum does not match its manifest; no PVC skill was changed." >&2
    return 0
  }

  if [ ! -e "$destination_skill" ]; then
    if cp -a "$source_skill" "$destination_skill"; then
      echo "[cetirun-builtins] Installed managed skill: ${skill_directory_name}"
    else
      echo "[cetirun-builtins] WARNING: failed to install managed skill ${skill_directory_name}; preserving the existing PVC state." >&2
    fi
    return 0
  fi
  if [ ! -d "$destination_skill" ] || [ -L "$destination_skill" ]; then
    echo "[cetirun-builtins] WARNING: preserving conflicting ${skill_directory_name}; the existing path is not a directory." >&2
    return 0
  fi

  if destination_metadata=$(cetirun_managed_skill_metadata "$destination_skill"); then
    destination_skill_id=${destination_metadata%%|*}
    destination_metadata_rest=${destination_metadata#*|}
    destination_version=${destination_metadata_rest%%|*}
    destination_checksum=${destination_metadata_rest#*|}
    if ! destination_actual_checksum=$(cetirun_skill_checksum "$destination_skill" 2>/dev/null) || \
      [ "$destination_skill_id" != "$source_skill_id" ] || \
      [ "$destination_actual_checksum" != "$destination_checksum" ]; then
      echo "[cetirun-builtins] WARNING: preserving customized managed skill ${skill_directory_name}; its checksum no longer matches its manifest." >&2
      return 0
    fi
    if ! cetirun_is_recognized_managed_checksum \
      "$source_skill" "$destination_checksum" "$source_checksum"; then
      echo "[cetirun-builtins] WARNING: preserving conflicting ${skill_directory_name}; its payload is not an image-recognized managed revision." >&2
      return 0
    fi
    if [ "$destination_checksum" = "$source_checksum" ] && \
      [ "$destination_version" = "$source_version" ]; then
      echo "[cetirun-builtins] Managed skill is current: ${skill_directory_name}"
      return 0
    fi
  else
    if ! destination_checksum=$(cetirun_skill_checksum "$destination_skill" 2>/dev/null); then
      echo "[cetirun-builtins] WARNING: preserving conflicting ${skill_directory_name}; its payload could not be checksummed." >&2
      return 0
    fi
    if cetirun_is_recognized_managed_checksum \
      "$source_skill" "$destination_checksum" "$source_checksum"; then
      echo "[cetirun-builtins] Adopting the recognized legacy managed skill: ${skill_directory_name}"
    else
      echo "[cetirun-builtins] WARNING: preserving conflicting ${skill_directory_name}; it is not a recognized managed skill." >&2
      return 0
    fi
  fi

  stage_dir="${destination_skill}.cetirun-stage.$$"
  backup_dir="${destination_skill}.cetirun-backup.$$"
  if [ -e "$stage_dir" ] || [ -e "$backup_dir" ]; then
    echo "[cetirun-builtins] WARNING: could not safely stage managed skill ${skill_directory_name}; preserving the existing PVC skill." >&2
    return 0
  fi
  if ! cp -a "$source_skill" "$stage_dir"; then
    echo "[cetirun-builtins] WARNING: failed to stage managed skill ${skill_directory_name}; preserving the existing PVC skill." >&2
    return 0
  fi
  if ! mv "$destination_skill" "$backup_dir"; then
    echo "[cetirun-builtins] WARNING: failed to back up managed skill ${skill_directory_name}; preserving the existing PVC skill." >&2
    rm -rf "$stage_dir"
    return 0
  fi
  if mv "$stage_dir" "$destination_skill"; then
    rm -rf "$backup_dir"
    echo "[cetirun-builtins] Upgraded managed skill: ${skill_directory_name}"
    return 0
  fi

  echo "[cetirun-builtins] WARNING: failed to activate managed skill ${skill_directory_name}; restoring the prior PVC skill." >&2
  mv "$backup_dir" "$destination_skill" || \
    echo "[cetirun-builtins] ERROR: manual recovery is required for ${skill_directory_name}." >&2
  rm -rf "$stage_dir"
  return 0
}

cetirun_sync_builtin_skills() {
  image_skills_dir="$1"
  persistent_skills_dir="$2"
  [ -d "$image_skills_dir" ] || return 0
  mkdir -p "$persistent_skills_dir" || {
    echo "[cetirun-builtins] WARNING: cannot create the persistent skills directory." >&2
    return 0
  }
  for source_skill in "$image_skills_dir"/*/; do
    [ -d "$source_skill" ] || continue
    skill_directory_name=$(basename "$source_skill")
    cetirun_install_managed_skill \
      "$source_skill" \
      "$persistent_skills_dir/$skill_directory_name" \
      "$skill_directory_name"
  done
}
