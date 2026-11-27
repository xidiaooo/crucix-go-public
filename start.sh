#!/bin/sh
# 可选启动脚本：平台一般直接用启动命令就行，这个只是方便本地跑
set -e
cd "$(dirname "$0")"
BIN="${BIN:-./crucix-linux-amd64}"
[ -f .env ] && set -a && . ./.env && set +a
exec "$BIN"
