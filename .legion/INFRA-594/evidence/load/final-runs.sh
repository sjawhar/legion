#!/usr/bin/env bash
# final-runs.sh: the 10 measured restart runs (5 with resume points, 5 WARM=0 LOADTEST_NO_RESUME=1)
# on step-10x, then 2 with the step's Dispatch request load off (--requests 0), each analysed.
L=/legion/scratch/tmp/infra594/load
cd "$L"
for i in 1 2 3 4 5; do
  ./restart-run.sh final-resume-$i >runs/final-resume-$i.driver.log 2>&1
  python3 analyze_restart.py final-resume-$i >runs/final-resume-$i.analysis.json
  WARM=0 LOADTEST_NO_RESUME=1 ./restart-run.sh final-noresume-$i >runs/final-noresume-$i.driver.log 2>&1
  python3 analyze_restart.py final-noresume-$i >runs/final-noresume-$i.analysis.json
done
for i in 1 2; do
  REQUESTS=0 ./restart-run.sh final-noload-resume-$i >runs/final-noload-resume-$i.driver.log 2>&1
  python3 analyze_restart.py final-noload-resume-$i >runs/final-noload-resume-$i.analysis.json
done
echo final-runs-done
