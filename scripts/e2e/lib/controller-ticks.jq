# The ticks a controller's Oh My Pi session received, read from the envoy-message custom messages of
# its transcript: stage4b-sandbox-tree.sh's controller checkpoint, which holds every tick's
# openCapabilities to the open rows of the daemon running when the tick was sent. The tick's payload
# is `{kind: "tick", openCapabilities: [...]}` (record.ControllerNotice; admit.Admission.wakeController
# puts the deployment's Open() rows on a tick alone, and none on a tick with no gap). The pi-envoy
# plugin renders a delivery into the session as TOON (envoy-client's renderInbound): the payload,
# parsed, is the card's `message` block, where a list of one reads `openCapabilities[1]: secrets` and
# a list of two `openCapabilities[2]: a,b`, on its own line. The rows are the running daemon's, never
# a set recorded earlier: every `legion start` re-probes the image, and pool-capacity is that probe's
# own verdict (packages/daemon/internal/capabilities/deployment.go), so a daemon restarted after boot
# can find the pool warm where boot's scaled it. Loaded with `jq -L scripts/e2e/lib` and
# `include "controller-ticks";`; includes nothing, and its defs call only each other (jq 1.8.2 aborts
# at compile on a def chain reached through an include, see lib/omp-tool-calls.jq).

# tick_open_capabilities: the openCapabilities list a rendered tick delivery carries. pi-envoy renders
# a delivery into the session as TOON: the payload's list of one reads `openCapabilities[1]: secrets`
# and a list of two `openCapabilities[2]: a,b`, on its own line; a delivery without the line names no
# gap. Input: the rendered text of one delivery.
def tick_open_capabilities:
  ((capture("\\n *openCapabilities\\[[0-9]+\\]: *(?<list>[^\\n]*)") // {list: ""}).list
    | split(",") | map(ltrimstr(" ") | rtrimstr(" ") | select(. != "")));

# controller_ticks($tick): the tick deliveries of a controller session, read as one raw string
# (`jq -R -s`): every custom_message of customType envoy-message whose rendered text carries $tick
# (the tick's needle), each {open, delivery} — the list and the delivery on one line, trimmed.
def controller_ticks($tick):
  [split("\n")[] | fromjson?
    | select(.type == "custom_message" and .customType == "envoy-message")
    | (if (.content | type) == "string" then .content else ([.content[]? | select(.type == "text") | .text] | join("\n")) end)
    | select(contains($tick))
    | {open: tick_open_capabilities, delivery: (gsub("\\s+"; " ") | .[0:300])}];

# ticks_name_open($open): input an array of the {open} ticks a controller received; $open the open
# rows of the daemon running now (`legion state --json`'s .capabilities with status open), never a
# set recorded earlier: every `legion start` re-probes the image, so pool-capacity can differ between
# boot's daemon and the one whose ticks these are. True when there is at least one tick, $open names
# secrets (the gap the proof's legion.yaml leaves open on purpose), and every tick's list is $open as
# a set.
def ticks_name_open($open):
  length > 0 and ($open | index("secrets") != null) and all(.[]; (.open | sort) == ($open | sort));
