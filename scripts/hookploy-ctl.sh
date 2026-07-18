#!/bin/bash
# hookploy-ctl.sh — 控制与本脚本同目录的 hookploy 二进制（PID 文件方式）。
# 实例隔离：每个部署目录（测试 /opt/apps/hookploy_test、正式 /opt/apps/hookploy）
# 各放一份本脚本，只控制自己目录里的进程，绝不 pkill、绝不影响旁的实例。

cd "$(dirname "$0")" || exit 1

BIN="./hookploy"
CONFIG="hookploy.yaml"
PID_FILE="hookploy.pid"
LOG_FILE="main.log"

start_main() {
    if [ -f "$PID_FILE" ]; then
        local pid=$(cat "$PID_FILE")
        if kill -0 "$pid" 2>/dev/null; then
            echo "hookploy is already running (PID: $pid)"
            return 1
        else
            echo "Removing stale PID file"
            rm "$PID_FILE"
        fi
    fi

    echo "Starting hookploy main ($(pwd))..."
    nohup "$BIN" main -f "$CONFIG" < /dev/null > "$LOG_FILE" 2>&1 &
    local pid=$!
    echo "$pid" > "$PID_FILE"

    sleep 1
    if kill -0 "$pid" 2>/dev/null; then
        echo "hookploy started (PID: $pid)"
        tail -n 5 "$LOG_FILE"
    else
        echo "Failed to start hookploy:"
        tail -n 20 "$LOG_FILE"
        rm -f "$PID_FILE"
        return 1
    fi
}

stop_main() {
    if [ ! -f "$PID_FILE" ]; then
        echo "hookploy is not running (no PID file)"
        return 1
    fi

    local pid=$(cat "$PID_FILE")
    if ! kill -0 "$pid" 2>/dev/null; then
        echo "hookploy is not running (stale PID file)"
        rm -f "$PID_FILE"
        return 1
    fi

    echo "Stopping hookploy (PID: $pid)..."
    kill "$pid"

    local count=0
    while kill -0 "$pid" 2>/dev/null && [ $count -lt 10 ]; do
        sleep 1
        count=$((count + 1))
    done

    if kill -0 "$pid" 2>/dev/null; then
        echo "Force killing hookploy..."
        kill -9 "$pid"
    fi

    rm -f "$PID_FILE"
    echo "hookploy stopped"
}

status_main() {
    if [ ! -f "$PID_FILE" ]; then
        echo "hookploy is not running (no PID file)"
        return 1
    fi

    local pid=$(cat "$PID_FILE")
    if kill -0 "$pid" 2>/dev/null; then
        echo "hookploy is running (PID: $pid, dir: $(pwd))"
        return 0
    else
        echo "hookploy is not running (stale PID file)"
        rm -f "$PID_FILE"
        return 1
    fi
}

logs_main() {
    if [ ! -f "$LOG_FILE" ]; then
        echo "No log file found at $LOG_FILE"
        return 1
    fi

    if [ "$1" = "-f" ]; then
        tail -f "$LOG_FILE"
    else
        tail -n 50 "$LOG_FILE"
    fi
}

case "$1" in
    start)
        start_main
        ;;
    stop)
        stop_main
        ;;
    status)
        status_main
        ;;
    restart)
        stop_main
        sleep 1
        start_main
        ;;
    logs)
        logs_main "$2"
        ;;
    *)
        echo "Usage: $0 {start|stop|status|restart|logs [-f]}"
        exit 1
        ;;
esac
