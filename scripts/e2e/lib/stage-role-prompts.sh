#!/usr/bin/env bash
# Stages the shared role prompt bundle beside a freshly built Go legion binary. Sourced, never run.
#
# stage_role_prompts ROOT WORK — copies ROOT/packages/pi-envoy/roles to WORK/role-prompts.
stage_role_prompts() {
  local root=${1:?stage_role_prompts requires the repository root}
  local work=${2:?stage_role_prompts requires the run work directory}

  cp -a "$root/packages/pi-envoy/roles" "$work/role-prompts"
}
