#!/bin/bash
# hookploy-ctl.sh — 控制与本脚本同目录的 hookploy 二进制（PID 文件方式）。
# 实例隔离：每个部署目录（测试 /opt/apps/hookploy_test、正式 /opt/apps/hookploy）
# 各放一份本脚本，只控制自己目录里的进程，绝不 pkill、绝不影响旁的实例。
#
# 角色：main（默认）与 edge。edge 需要同目录下两个 dotfile：
#   .edge_main    — main 的 gRPC URL（如 http://127.0.0.1:9181 或 https://hookploy.example.com）
#   .server_token — server token（hps_...，`hookploy server token create <name>` 生成）

cd "$(dirname "$0")" || exit 1

BIN="./hookploy"
CONFIG="hookploy.yaml"

# start <pid_file> <log_file> <name> <cmd...>
start_proc() {
    local pid_file="$1" log_file="$2" name="$3"
    shift 3
    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file")
        if kill -0 "$pid" 2>/dev/null; then
            echo "$name is already running (PID: $pid)"
            return 1
        else
            echo "Removing stale PID file"
            rm "$pid_file"
        fi
    fi

    echo "Starting $name ($(pwd))..."
    nohup "$@" < /dev/null > "$log_file" 2>&1 &
    local pid=$!
    echo "$pid" > "$pid_file"

    sleep 1
    if kill -0 "$pid" 2>/dev/null; then
        echo "$name started (PID: $pid)"
        tail -n 5 "$log_file"
    else
        echo "Failed to start $name:"
        tail -n 20 "$log_file"
        rm -f "$pid_file"
        return 1
    fi
}

# stop <pid_file> <name>
stop_proc() {
    local pid_file="$1" name="$2"
    if [ ! -f "$pid_file" ]; then
        echo "$name is not running (no PID file)"
        return 1
    fi

    local pid=$(cat "$pid_file")
    if ! kill -0 "$pid" 2>/dev/null; then
        echo "$name is not running (stale PID file)"
        rm -f "$pid_file"
        return 1
    fi

    echo "Stopping $name (PID: $pid)..."
    kill "$pid"

    local count=0
    while kill -0 "$pid" 2>/dev/null && [ $count -lt 10 ]; do
        sleep 1
        count=$((count + 1))
    done

    if kill -0 "$pid" 2>/dev/null; then
        echo "Force killing $name..."
        kill -9 "$pid"
    fi

    rm -f "$pid_file"
    echo "$name stopped"
}

# status <pid_file> <name>
status_proc() {
    local pid_file="$1" name="$2"
    if [ ! -f "$pid_file" ]; then
        echo "$name is not running (no PID file)"
        return 1
    fi

    local pid=$(cat "$pid_file")
    if kill -0 "$pid" 2>/dev/null; then
        echo "$name is running (PID: $pid, dir: $(pwd))"
        return 0
    else
        echo "$name is not running (stale PID file)"
        rm -f "$pid_file"
        return 1
    fi
}

# logs <log_file> [-f]
logs_proc() {
    local log_file="$1"
    if [ ! -f "$log_file" ]; then
        echo "No log file found at $log_file"
        return 1
    fi

    if [ "$2" = "-f" ]; then
        tail -f "$log_file"
    else
        tail -n 50 "$log_file"
    fi
}

edge_args() {
    if [ ! -f .edge_main ] || [ ! -f .server_token ]; then
        echo "edge requires .edge_main (main URL) and .server_token in $(pwd)" >&2
        return 1
    fi
}

case "$1" in
    start)
        start_proc hookploy.pid main.log "hookploy main" "$BIN" main -f "$CONFIG"
        ;;
    stop)
        stop_proc hookploy.pid "hookploy main"
        ;;
    status)
        status_proc hookploy.pid "hookploy main"
        ;;
    restart)
        stop_proc hookploy.pid "hookploy main"
        sleep 1
        start_proc hookploy.pid main.log "hookploy main" "$BIN" main -f "$CONFIG"
        ;;
    logs)
        logs_proc main.log "$2"
        ;;
    edge-start)
        edge_args || exit 1
        start_proc edge.pid edge.log "hookploy edge" "$BIN" edge --main "$(cat .edge_main)" --token "$(cat .server_token)"
        ;;
    edge-stop)
        stop_proc edge.pid "hookploy edge"
        ;;
    edge-status)
        status_proc edge.pid "hookploy edge"
        ;;
    edge-restart)
        stop_proc edge.pid "hookploy edge"
        sleep 1
        edge_args || exit 1
        start_proc edge.pid edge.log "hookploy edge" "$BIN" edge --main "$(cat .edge_main)" --token "$(cat .server_token)"
        ;;
    edge-logs)
        logs_proc edge.log "$2"
        ;;
    *)
        echo "Usage: $0 {start|stop|status|restart|logs [-f]|edge-start|edge-stop|edge-status|edge-restart|edge-logs [-f]}"
        exit 1
        ;;
esac
