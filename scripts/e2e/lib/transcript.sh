# shellcheck shell=bash
# A stage proof's transcript. Sourced by the entry script before its first output, never run.
#
# transcript_to FILE sends the calling shell's stdout and stderr to FILE as well as to whatever read
# them before, so every line of the run is kept whoever is reading.
#
# The tee shares the driver's process group. A signal to the group (Ctrl-C, a closed pane,
# timeout's TERM) would end it before cleanup writes, and cleanup's first write would then die of
# SIGPIPE, so tee ignores the signals a driver traps and outlives the driver's last line. It ignores
# SIGPIPE as well, so a reader that goes first (a supervised launcher's own tee, stopped with the
# run) costs tee that one output. The trap carries SIGPIPE rather than `tee -p`, because busybox
# tee rejects -p.
#
# GNU tee stops once every output has failed, and never reopens one (tee.c:278, :317). With the
# reader gone, one ENOSPC on the transcript would end it for good, and cleanup's commands would die
# on their next write. /dev/null is an output that never fails, so GNU tee keeps draining the
# driver's output. busybox tee keeps writing every output and reports errors only at EOF.
transcript_to() {
  exec > >(trap '' HUP INT TERM PIPE && exec tee -a "$1" /dev/null) 2>&1
}
