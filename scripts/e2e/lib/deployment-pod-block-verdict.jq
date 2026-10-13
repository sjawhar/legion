# Stage 4b's verdict on a deployment's worker pod block (stage4b-sandbox-tree.sh's
# deployment-pod-block): whether the issue pod the daemon launched from the block carries all of it.
# Input: the pod as the API server returns it (kubectl get pod -o json).
# Arguments, by name ($ARGS.named):
#   block      the block, `runtime.kubernetes.pod` as the run's legion.yaml holds it
#              (--slurpfile, one JSON object): service_account, env, volumes, volume_mounts, in the
#              shape Legion's loader reads (packages/daemon/internal/config/kubernetes.go).
#   processes  what the pod's architect container runs (--rawfile): for each process, a line
#              `#<pid> <comm>`, then one `NAME=VALUE` line for each of the block's env names the
#              process has. The block's environment reaches a role's processes when its launcher
#              starts them, not through the container spec.
# Output: {process, missing, wrong}:
#   process    {pid, comm} of the process holding the most of the block's env names (the lowest pid
#              on a tie), or null when no process holds one;
#   missing    what the block names and the pod lacks: a volume, a projected source, a mount in a
#              role container, an env name on that process;
#   wrong      what the pod carries differently: its ServiceAccount, a volume's kind or object, a
#              projected token's audience, expiry or path, a mount's sub_path or read_only, an env
#              value. A value is never printed, only its name, so a verdict never carries an env value.
# Every container of the pod but its init containers is a role container, and each must carry every
# mount. A projected token with no expiration_seconds expires in 3600 s (the API server's default),
# and a mount with no read_only is read-only (Legion's loader). A Secret or ConfigMap volume is
# compared by its kind and object name, not its `items`; a token is checked as carried, never as
# accepted by the service it is for.
($block[0]) as $b
| . as $pod
| ($pod.spec.volumes // [] | map({key: .name, value: .}) | from_entries) as $volumes
| ($processes
   | split("\n")
   | reduce .[] as $line ([];
       if ($line | startswith("#")) then
         . + [{pid: ($line[1:] | split(" ")[0] | tonumber), comm: ($line[1:] | split(" ")[1:] | join(" ")), env: {}}]
       elif ($line | length) > 0 and length > 0 and ($line | contains("=")) then
         .[-1].env[($line | split("=")[0])] = ($line | split("=")[1:] | join("="))
       else . end)) as $procs
| ($b.env // {}) as $env
| (if ($env | length) == 0 then null
   else ($procs | map(select(.env | length > 0)) | sort_by([-(.env | length), .pid]) | first) end) as $process
| def source_kind: if .service_account_token then "service_account_token" elif .secret then "secret" elif .config_map then "config_map" else "unknown" end;
  def pod_source_kind: if .serviceAccountToken then "service_account_token" elif .secret then "secret" elif .configMap then "config_map" else "unknown" end;
  def volume_kind: if .projected then "projected" elif .secret then "secret" elif .config_map then "config_map" else "unknown" end;
  def pod_volume_kind: if .projected then "projected" elif .secret then "secret" elif .configMap then "config_map" else "unknown" end;
  # source_findings(want; got; where): a projected source of the block against the pod's.
  def source_findings($want; $got; $where):
    ($want | source_kind) as $k
    | if $got == null then [{missing: "\($where) (\($k))"}]
      elif ($got | pod_source_kind) != $k then [{wrong: "\($where) is \($got | pod_source_kind), not \($k)"}]
      elif $k == "service_account_token" then
        [ (if ($got.serviceAccountToken.audience // "") != ($want.service_account_token.audience // "") then
             {wrong: "\($where) token audience \($got.serviceAccountToken.audience // "") is not \($want.service_account_token.audience // "")"} else empty end),
          (if ($got.serviceAccountToken.expirationSeconds // 3600) != ($want.service_account_token.expiration_seconds // 3600) then
             {wrong: "\($where) token expires in \($got.serviceAccountToken.expirationSeconds // 3600) s, not \($want.service_account_token.expiration_seconds // 3600)"} else empty end),
          (if $got.serviceAccountToken.path != $want.service_account_token.path then
             {wrong: "\($where) token path \($got.serviceAccountToken.path) is not \($want.service_account_token.path)"} else empty end) ]
      elif $k == "secret" then
        (if $got.secret.name != $want.secret.name then [{wrong: "\($where) is Secret \($got.secret.name), not \($want.secret.name)"}] else [] end)
      else
        (if $got.configMap.name != $want.config_map.name then [{wrong: "\($where) is ConfigMap \($got.configMap.name), not \($want.config_map.name)"}] else [] end)
      end;
  [
    (if $b.service_account != null and $pod.spec.serviceAccountName != $b.service_account then
       {wrong: "the pod runs as ServiceAccount \($pod.spec.serviceAccountName // "default"), not \($b.service_account)"} else empty end),
    (($b.volumes // [])[] as $v
     | ($volumes[$v.name]) as $pv
     | ($v | volume_kind) as $k
     | if $pv == null then {missing: "volume \($v.name)"}
       elif ($pv | pod_volume_kind) != $k then {wrong: "volume \($v.name) is \($pv | pod_volume_kind), not \($k)"}
       elif $k == "secret" then (if $pv.secret.secretName != $v.secret.name then {wrong: "volume \($v.name) is Secret \($pv.secret.secretName), not \($v.secret.name)"} else empty end)
       elif $k == "config_map" then (if $pv.configMap.name != $v.config_map.name then {wrong: "volume \($v.name) is ConfigMap \($pv.configMap.name), not \($v.config_map.name)"} else empty end)
       else
         ($v.projected.sources | to_entries[]) as $s
         | source_findings($s.value; $pv.projected.sources[$s.key]; "volume \($v.name) source \($s.key)")[]
       end),
    (($pod.spec.containers // [])[] as $c
     | ($b.volume_mounts // [])[] as $m
     # read_only is true unless the block says false; `//` would read a false as absent.
     | (if $m | has("read_only") then $m.read_only else true end) as $want_ro
     | ([$c.volumeMounts // [] | .[] | select(.name == $m.volume and .mountPath == $m.mount_path)] | first) as $pm
     | if $pm == null then {missing: "mount \($m.volume) at \($m.mount_path) in container \($c.name)"}
       else
         (if ($pm.subPath // "") != ($m.sub_path // "") then {wrong: "mount \($m.volume) at \($m.mount_path) in container \($c.name) has sub_path \($pm.subPath // "")"} else empty end),
         (if ($pm.readOnly == true) != $want_ro then {wrong: "mount \($m.volume) at \($m.mount_path) in container \($c.name) has read_only \($pm.readOnly == true)"} else empty end)
       end),
    (($env | to_entries[]) as $e
     | if $process == null then {missing: "env \($e.key) on any process of the architect container"}
       elif $process.env[$e.key] == null then {missing: "env \($e.key) on process \($process.pid)"}
       elif $process.env[$e.key] != ($e.value | tostring) then {wrong: "env \($e.key) on process \($process.pid) differs from the block"}
       else empty end)
  ] as $findings
| {process: (if $process == null then null else {pid: $process.pid, comm: $process.comm} end),
   missing: [$findings[] | .missing // empty],
   wrong: [$findings[] | .wrong // empty]}
