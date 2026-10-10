#!/bin/sh

# start nemo server
cd /opt/nemo || exit
# 魔改：追加 --mcp（AutoShow 经 MCP 5500 端口编排 nemo：采集/安全分析/渗透）
nohup ./server_linux_amd64 --web --service --cron --mcp &
# wait for nemo server to start
sleep 3s
# start nemo daemon worker
cd /opt/nemo_worker || exit
nohup ./daemon_worker_linux_amd64 --service 127.0.0.1 --auth da8Ae0e295eba72a7rgb1d34b13d86b --no-redis-proxy &
# keep running...
cd /opt || exit
tail -f /dev/null
