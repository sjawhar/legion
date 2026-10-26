# shellcheck shell=bash disable=SC2034  # the sourcing script uses these variables
# The POSIX sh programs a live proof runs in an issue pod's `roles` container to reach one role.
# Sourced by stage4b-sandbox-tree.sh and share-volumes-kind.sh.
#
# An issue pod has one container, `roles`, whose PID 1 is the launcher of every role; each role's
# generation runs in Linux user, mount, PID and IPC namespaces of its own, whose init is that
# generation's `legion launcher role-init --role <role> …` (internal/launcher/roleinit.go), which
# binds the role's own gh, private, state and agent-secrets directories at the fixed paths under
# /var/run/legion, covers every role's directory of the roles directory, and runs the role's
# `legion worker-shim` (with its Oh My Pi) in a nested user namespace as the image's user.
# role_enter_sh is the POSIX sh, run in the `roles` container as `sh -c "$role_enter_sh" role-enter
# ROLE COMMAND…`, that runs COMMAND as ROLE's agent sees the pod: it finds ROLE's role-init by its
# argv (`launcher role-init`, then `--role ROLE` before the shim's `--`), enters that init's user,
# mount, PID and IPC namespaces (the container's user owns them, so nsenter needs no capability the
# container lacks), finds the role's worker-shim there (the process whose parent is the namespaces'
# PID 1 and whose argv[1] is worker-shim), and enters the shim's own user namespace, the agent's,
# uid 1000. COMMAND then sees the role's /proc (its role-init as PID 1, its shim and agent, nothing
# of another role), its /tmp and its gh bundle; stdin and the exit status pass through. gVisor
# refuses a PID namespace entered through the shim's user namespace, so the two hops are in that
# order (measured in a gVisor pod of the worker image, 2026-10-24). No role-init of ROLE, or two of
# them (a generation still ending as the next starts), is exit 1 naming why, as a wait expects.
# Every program here reads `${proc_root:-/proc}`: the container's own /proc, unless a test points
# proc_root at a fake tree, since a test scanning the host's /proc would find the worker-shim and
# Oh My Pi of any Legion role running on that host.
# shellcheck disable=SC2016  # expanded by the pod's shell
role_enter_sh='
role=$1
shift
init=
for d in "${proc_root:-/proc}"/[0-9]*; do
  tr "\0" "\n" 2>/dev/null <"$d/cmdline" | awk -v role="$role" "
    NR == 2 && \$0 != \"launcher\" { bad = 1; exit }
    NR == 3 && \$0 != \"role-init\" { bad = 1; exit }
    \$0 == \"--\" { exit }
    after && \$0 == role { found = 1 }
    { after = (\$0 == \"--role\") }
    END { exit bad || !found }" || continue
  if [ -n "$init" ]; then
    echo "two role-init processes of $role run in this container: ${init} and ${d##*/}" >&2
    exit 1
  fi
  init=${d##*/}
done
if [ -z "$init" ]; then
  echo "no role-init of $role runs in this container" >&2
  exit 1
fi
exec nsenter -t "$init" -U -m -p -i --preserve-credentials -- sh -c "
  shim=
  for d in \"\${proc_root:-/proc}\"/[0-9]*; do
    [ \"\$(sed -n \"s/^PPid:[[:space:]]*//p\" \"\$d/status\" 2>/dev/null)\" = 1 ] || continue
    [ \"\$(tr \"\\\\0\" \"\\\\n\" 2>/dev/null <\"\$d/cmdline\" | sed -n 2p)\" = worker-shim ] || continue
    if [ -n \"\$shim\" ]; then
      echo \"two worker-shims run in the namespaces of $role: \$shim and \${d##*/}\" >&2
      exit 1
    fi
    shim=\${d##*/}
  done
  if [ -z \"\$shim\" ]; then
    echo \"no worker-shim runs in the namespaces of $role\" >&2
    exit 1
  fi
  exec nsenter -t \"\$shim\" -U --preserve-credentials -- \"\$@\"" role-shim "$@"
'
# role_kill_sh, run in the `roles` container as `sh -c "$role_kill_sh" role-kill INIT`, sends
# SIGKILL to the process group of the worker-shim role-init INIT runs: the shim is the child of
# role-init whose argv[1] is worker-shim (as role_enter_sh finds it; role-init reaps every orphan
# of its namespace, so a process the agent detached is a child of it too and is skipped), started
# in a process group of its own (Setpgid, internal/launcher/roleinit_linux.go runRoleShim), and Oh
# My Pi, the shim's child, is in that group. The container's user owns the role's user namespaces,
# so it may signal them. No shim, two of them, or a shim that is not the leader of its own group
# is refused rather than signalled, since that group would be someone else's.
# shellcheck disable=SC2016  # expanded by the pod's shell
role_kill_sh='
init=$1 shim= proc=${proc_root:-/proc}
for d in "$proc"/[0-9]*; do
  [ "$(sed -n "s/^PPid:[[:space:]]*//p" "$d/status" 2>/dev/null)" = "$init" ] || continue
  [ "$(tr "\0" "\n" 2>/dev/null <"$d/cmdline" | sed -n 2p)" = worker-shim ] || continue
  [ -z "$shim" ] || { echo "role-init $init runs two worker-shims: $shim and ${d##*/}" >&2; exit 1; }
  shim=${d##*/}
done
[ -n "$shim" ] || { echo "role-init $init runs no worker-shim" >&2; exit 1; }
group=$(sed "s/.*) //" "$proc/$shim/stat" | cut -d" " -f3)
[ "$group" = "$shim" ] || { echo "the shim $shim of role-init $init is in process group $group, not its own" >&2; exit 1; }
kill -s KILL -- "-$group"'
