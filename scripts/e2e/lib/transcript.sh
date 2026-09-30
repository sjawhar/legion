# shellcheck shell=bash
# A stage proof's transcript. Sourced by the entry script before its first output, never run.
#
# transcript_to FILE sends the calling shell's stdout and stderr to FILE as well as to whatever read
# stdout before, so every line of the run is kept whoever is reading.
#
# The tee shares the driver's process group. A signal to the group (Ctrl-C, a closed pane,
# timeout's TERM) would end it before cleanup writes, and cleanup's first write would then die of
# SIGPIPE, so tee ignores the signals a driver traps and outlives the driver's last line. It ignores
# SIGPIPE as well, so a reader that goes first (a supervised launcher's own tee, stopped with the
# run) costs tee that one output. Without it, a group TERM after the reader had gone would end a
# driver with a TERM trap before its EXIT trap ran: bash writes its Terminated notice for the
# interrupted command to the dead pipe first, even when cleanup itself writes nothing. The trap
# carries SIGPIPE rather than `tee -p`, because busybox tee rejects -p.
#
# GNU tee stops once every output has failed (coreutils 9.4 tee.c:280, :317), and never reopens one
# (:215). With the reader gone, one ENOSPC on the transcript would end it for good, and cleanup's
# commands would die on their next write. /dev/null is an output that never fails, so GNU tee keeps
# draining the driver's output. busybox tee keeps writing every output and reports errors only at
# EOF, so it picks the transcript up again once its disk has room.
#
# The tee is one of the run's processes: its argv names FILE, and its working directory is the
# caller's at the call. lib/rig.sh's run_processes matches $work in either, and a cleanup that
# SIGKILLs what it names would kill the tee before its own last writes. So FILE and the caller's
# working directory stay outside $work, or the caller opens FILE on a descriptor and passes
# /dev/fd/N, as stage 2 and stage3-4b13b-acceptance.sh do.
transcript_to() {
  exec > >(trap '' HUP INT TERM PIPE && exec tee -a "$1" /dev/null) 2>&1
}
