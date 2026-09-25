#!/bin/sh
# freeze-pull.sh <main|edge> [count] — SIGSTOP the next <count> `docker pull`
# children of this test dir's main/edge process (found via its PID file, never
# by process name), simulating a pull that hangs forever. Gives up after 300s.
role=$1; count=${2:-1}
cd "$(dirname "$0")" || exit 1
case $role in main) pidf=hookploy.pid ;; edge) pidf=edge.pid ;; *) echo "role?"; exit 1 ;; esac
parent=$(cat "$pidf")
n=0; seen=""
end=$(( $(date +%s) + 300 ))
while [ "$n" -lt "$count" ] && [ "$(date +%s)" -lt "$end" ]; do
  for p in $(ps -o pid= --ppid "$parent"); do
    args=$(tr '\0' ' ' < "/proc/$p/cmdline" 2>/dev/null)
    case "$args" in
      "docker pull "*)
        case " $seen " in *" $p "*) continue ;; esac
        kill -STOP "$p" && seen="$seen $p" && n=$((n+1)) &&
          echo "$(date +%T.%3N) SIGSTOP pid $p (child of $role $parent): $args"
        ;;
    esac
  done
  sleep 0.02
done
echo "$(date +%T.%3N) done, froze $n"
