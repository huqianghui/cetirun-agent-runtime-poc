#!/bin/sh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# init-skills.sh — install npm-packaged skills into the PVC.
#
# Environment:
#   NPM_PACKAGES   space-separated list of npm package specs (e.g. "matrix @scope/foo@1.2.3")
#   SKILLS_DIR     target directory on the PVC (e.g. /home-data/.cetirun/skills/npm)
#   NPM_CACHE_DIR  working directory + npm cache root on the PVC
#   NPM_REGISTRY   package registry; defaults to the public npm registry
#
# Behavior:
#   - For each package, install into NPM_CACHE_DIR/<dir>/, then copy
#     node_modules/<pkg-name>/ to SKILLS_DIR/<dir>/.
#   - Idempotent: <dir>/.installed-version stores the requested spec; skipped if matches.
#   - Copies and completion markers are staged before replacing the installed directory.
#   - Prunes <dir> entries no longer in the requested list.
#   - Per-package failures are recorded; script exits non-zero if any failed.
set -u

: "${NPM_PACKAGES:=}"
: "${SKILLS_DIR:=/home-data/.cetirun/skills/npm}"
: "${NPM_CACHE_DIR:=/home-data/.cetirun/.npm-cache}"
: "${NPM_REGISTRY:=https://registry.npmjs.org/}"

if [ -n "$NPM_REGISTRY" ]; then
  export npm_config_registry="$NPM_REGISTRY"
  # Rewrite lockfile-style npmjs URLs but preserve mirror-owned tarball URLs.
  export npm_config_replace_registry_host=npmjs
fi

ERRORS_LOG="${SKILLS_DIR}/.errors.log"

mkdir -p "$SKILLS_DIR" "$NPM_CACHE_DIR"
: > "$ERRORS_LOG"

# These names are reserved for disposable copies from prior init attempts.
for abandoned in "$SKILLS_DIR"/.stage.??????; do
  [ -e "$abandoned" ] || [ -L "$abandoned" ] || continue
  name=${abandoned##*/}
  case "${name#.stage.}" in *[!a-zA-Z0-9]*) continue;; esac
  if [ -L "$abandoned" ] || [ ! -d "$abandoned" ] || ! rm -rf "$abandoned"; then
    echo "[init-skills] FAILED reclaiming prior staging directory" | tee -a "$ERRORS_LOG" >&2
    exit 1
  fi
done

# Sanitize a package spec into a directory name:
#   "@scope/name@1.2.3" -> "@scope__name"
#   "matrix"            -> "matrix"
sanitize() {
  spec="$1"
  # If spec starts with @, version is after the SECOND @; else after first.
  case "$spec" in
    @*)
      name="@$(printf %s "$spec" | cut -c2- | cut -d@ -f1)"
      ;;
    *)
      name="$(printf %s "$spec" | cut -d@ -f1)"
      ;;
  esac
  printf %s "$name" | sed 's|/|__|g'
}

# Extract the package name (without version) so we can copy from node_modules/<name>/.
pkg_name() {
  spec="$1"
  case "$spec" in
    @*)
      printf '@%s\n' "$(printf %s "$spec" | cut -c2- | cut -d@ -f1)"
      ;;
    *)
      printf '%s\n' "$(printf %s "$spec" | cut -d@ -f1)"
      ;;
  esac
}

keep_list=""
fail_count=0

for spec in $NPM_PACKAGES; do
  [ -n "$spec" ] || continue
  case "$spec" in
    -*)
      echo "[init-skills] FAILED $spec — package spec must not start with '-'" | tee -a "$ERRORS_LOG"
      fail_count=$((fail_count + 1))
      continue
      ;;
  esac
  dir=$(sanitize "$spec")
  name=$(pkg_name "$spec")
  keep_list="$keep_list $dir"

  target="$SKILLS_DIR/$dir"
  marker="$target/.installed-version"

  if [ -f "$marker" ] && [ "$(cat "$marker" 2>/dev/null)" = "$spec" ]; then
    echo "[init-skills] $spec — up to date (skip)"
    continue
  fi

  echo "[init-skills] installing $spec → $target"
  work="$NPM_CACHE_DIR/$dir"
  rm -rf "$work"
  mkdir -p "$work"
  (
    cd "$work" || exit 1
    # --prefix . installs into ./node_modules; --no-save avoids requiring package.json.
    npm install \
      --prefix . \
      --no-save \
      --omit=dev \
      --no-audit \
      --no-fund \
      --cache "$NPM_CACHE_DIR/_npm" \
      "$spec"
  )
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "[init-skills] FAILED npm install $spec (exit $rc)" | tee -a "$ERRORS_LOG"
    fail_count=$((fail_count + 1))
    continue
  fi

  src="$work/node_modules/$name"
  if [ ! -d "$src" ]; then
    echo "[init-skills] FAILED $spec — $src not found after install" | tee -a "$ERRORS_LOG"
    fail_count=$((fail_count + 1))
    continue
  fi

  stage=$(mktemp -d "$SKILLS_DIR/.stage.XXXXXX") || {
    echo "[init-skills] FAILED staging $spec" | tee -a "$ERRORS_LOG"
    fail_count=$((fail_count + 1))
    continue
  }
  if ! cp -a "$src/." "$stage/" || ! rm -f "$stage/.installed-version" || ! printf '%s\n' "$spec" > "$stage/.installed-version"; then
    echo "[init-skills] FAILED copying or recording $spec" | tee -a "$ERRORS_LOG"
    rm -rf "$stage"
    fail_count=$((fail_count + 1))
    continue
  fi
  if ! rm -rf "$target" || ! mv -T "$stage" "$target"; then
    echo "[init-skills] FAILED publishing $spec" | tee -a "$ERRORS_LOG"
    rm -rf "$stage"
    fail_count=$((fail_count + 1))
    continue
  fi
  echo "[init-skills] OK $spec"
done

# Prune entries that are no longer requested.
if [ -d "$SKILLS_DIR" ]; then
  for existing in "$SKILLS_DIR"/*; do
    [ -d "$existing" ] || continue
    base=$(basename "$existing")
    case " $keep_list " in
      *" $base "*) ;;
      *)
        echo "[init-skills] pruning $existing (no longer requested)"
        rm -rf "$existing"
        ;;
    esac
  done
fi

if [ "$fail_count" -gt 0 ]; then
  echo "[init-skills] $fail_count package(s) failed; see $ERRORS_LOG" >&2
  exit 1
fi

echo "[init-skills] done."
