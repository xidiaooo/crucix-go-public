#!/bin/sh
# 入口：先起 cloudflared 隧道（配了 TUNNEL_TOKEN 才启动），再启动主程序。
# 隧道把公网流量直接送到本地 8080，绕过平台入口对 WebSocket 的限制。
if [ -n "$TUNNEL_TOKEN" ]; then
  cloudflared tunnel --no-autoupdate run --token "$TUNNEL_TOKEN" &
fi
exec /app/crucix
