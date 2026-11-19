#!/bin/sh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -eu
if [ "${1:-}" = "--help" ]; then
  printf '%s\n' "Initialize Custom HOME with SOURCE_HOME, TARGET_HOME and CETIRUN_INIT_IMAGE."
  exit 0
fi

. "${CETIRUN_INIT_COMMON:-/etc/cetirun/init-home-common.sh}"
LEGACY_HOME_MARKER=

agent_check_requirements() {
  [ -r "$SOURCE_HOME" ] && [ -x "$SOURCE_HOME" ] && [ -w "$TARGET_HOME" ] || {
    init_error "HomePermissionsInvalid"
    return 1
  }
  if [ ! -f "$TARGET_HOME/.cetirun-home-seeded" ] &&
    { [ -L "$SOURCE_HOME/.agents" ] ||
      { [ -e "$SOURCE_HOME/.agents" ] && [ ! -d "$SOURCE_HOME/.agents" ]; }; }; then
    init_error "ImageAgentsPathInvalid"
    return 1
  fi
  [ -r "${CETIRUN_CONTEXT_DIR:-/etc/cetirun-context}/AGENTS.md" ] || {
      init_error "ContextBindingPending"
      return 1
    }
  if [ -z "${CETIRUN_CONTEXT_VIEW:-}" ] &&
    [ ! -r "${CETIRUN_CONTEXT_DIR:-/etc/cetirun-context}/skills/cetirun-memory/SKILL.md" ]; then
    init_error "ContextBindingPending"
    return 1
  fi
}

agent_prepare_home() {
  # The whole approved view will shadow this mountpoint in the main container.
  # Refuse a symlink/file; never repair it by removing image or user content.
  home_ensure_dir .agents
}

agent_apply_config() { :; }
agent_prepare_integrations() { :; }
agent_verify() {
  [ -r "$TARGET_HOME/.cetirun-home-seeded" ] || {
    init_error "HomeSeedIncomplete"
    return 1
  }
}

run_agent_init
