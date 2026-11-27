# 通用 PaaS 部署（Go 单进程版）

目标：只用一个 HTTP 端口在平台上跑起来，面板能开、隧道能用。

## 先记三条

1. **端口必须读 `PORT`**：程序已经这么做了，别在启动命令里写死。
2. **`PUBLIC_HOST` 必须填平台给的域名**，否则订阅链接里是 `localhost`，导进客户端连不上。
3. **健康检查填 `/api/health`**，返回 200 就算活。

## 通用步骤（所有平台一样）

1. 上传这个包（或者只上传二进制）。
2. 平台会问启动命令，填：

   ```sh
   chmod +x crucix-linux-amd64 && ./crucix-linux-amd64
   ```

   ARM 机器换成 `crucix-linux-arm64`，不确定就用 `uname -m` 看一眼：`x86_64` 用 amd64，`aarch64` 用 arm64。

3. 环境变量至少加这一条：

   ```
   PUBLIC_HOST = 你的平台域名，比如 xxx.koyeb.app
   ```

   建议再加一条把凭证钉住（见下面「容器重建不换凭证」）。

4. 部署完打开 `https://你的域名/`，看到面板就是通了。
5. 拿节点：`https://你的域名/s/随机串?token=随机串`，这两串在启动日志里会打印，或者直接看 `https://你的域名/api/health` 下面说的方式。

> 订阅路径和 token 不在 health 里。第一次部署看平台日志：
> `[Tunnel] 首次启动，已生成独立凭证 → 访问 /s/xxxx 或 /api/health 查看状态`。
> 或者进容器执行 `cat runs/.creds.json`。

## 各平台要点

### Koyeb

- 类型选 **Web Service**，端口填 `8080`（或者你设的 `PORT`）。
- 部署方式选 **Dockerfile** 最省事（包里带了），或者用二进制 + 启动命令。
- Koyeb 的实例名会给一个 `xxx.koyeb.app` 域名，把它填进 `PUBLIC_HOST`。
- 免费档会缩容到 0，缩了隧道就断，这是免费档的通病。

### Railway

- 新建项目 → Deploy from repo / 上传。
- Railway 会自动检测 `go.mod` 自己编译；想跳过编译就把二进制传上去，启动命令直接跑它。
- 端口：Railway 会注入 `PORT`，程序读它，不用改。
- Settings → Networking → Generate Domain，把生成的域名填进 `PUBLIC_HOST`。

### Render

- 类型选 **Web Service**，Language 选 **Docker**（用包里 Dockerfile）或 **Go**（用源码）。
- Health Check Path 填 `/api/health`。
- 免费档 15 分钟没流量会睡，醒了冷启动几秒。

### Zeabur

- 部署 Go 项目或 Dockerfile 都行。
- 绑定域名后把域名填进 `PUBLIC_HOST`。
- 它的免费额度按流量算，面板本身几乎不耗。

### Fly.io

- `fly launch` 时它会生成 `fly.toml`，把 `internal_port` 改成你的 `PORT`。
- `fly secrets set PUBLIC_HOST=你的app.fly.dev`
- 注意：Fly 的免费额度是按机器算的，`shared-cpu-1x` + 256MB 够了。

### 普通 VPS / Docker

```sh
docker build -t crucix .
docker run -d --name crucix --restart=always \
  -p 8080:8080 \
  -e PORT=8080 \
  -e PUBLIC_HOST=你的域名 \
  -e CREDS_JSON='{"uuid":"...","subToken":"...","streamPath":"/ws/...","subPath":"/s/..."}' \
  crucix
```

前面套一层 Nginx/Caddy 反代到 8080，记得给 WebSocket 放行：

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Host $host;
    proxy_read_timeout 3600s;
}
```

## 容器重建不换凭证

PaaS 每次重建容器，文件系统都会重置，`runs/.creds.json` 就没了 —— 结果是 UUID 和路径全变，客户端得重导。

解决办法：把凭证整段塞进环境变量 `CREDS_JSON`。

1. 第一次部署后拿到 `runs/.creds.json` 的内容（平台的文件浏览或日志）。
2. 压成一行：

   ```json
   {"uuid":"xxxx-xxxx-xxxx-xxxx","subToken":"...","streamPath":"/ws/...","subPath":"/s/...","createdAt":"..."}
   ```

3. 填进环境变量 `CREDS_JSON`。

以后重启、重建、换实例，UUID 和两条路径都不变，客户端一次都不用重导。

从旧 Node 版迁移：直接把旧包的 `runs/.creds.json` 内容填进来就行，格式完全一样。

## 排坑表

| 现象 | 原因 | 处理 |
|---|---|---|
| 首页 502 / 平台说端口不对 | 没读 `PORT` 或者监听了 127.0.0.1 | 确认启动命令没写死端口；程序默认 `0.0.0.0` |
| 面板能开，节点连不上 | `PUBLIC_HOST` 没填，链接里是 localhost | 填上平台域名，重新导订阅 |
| 节点连上但没网 | 平台拦了 WebSocket 或出站 | 看平台文档；部分免费档只放行 HTTP，隧道会被掐 |
| 部署后过一会就断 | 免费档休眠 | 定时访问 `/api/health` 保活，或者升级付费档 |
| 容器重启后节点失效 | 文件系统重置 | 用 `CREDS_JSON` 固定凭证 |
| 内存超限被杀 | 平台给的内存太小 | `MEM_LIMIT_MB=64` + `FEED=off` + `PANEL=off`，能压到 9MB |
| 面板数据不动 | 出站被拦或 `FEED=off` | 看 `/api/health` 的 `lastError` |
| 日志里有个 `node server.js` | 这是进程伪装 | 正常，`PROC_MASK=off` 可以关掉 |

## 自测（可选）

源码目录里带了端到端测试，覆盖：

- VLESS over WS（TCP）
- Trojan over WS（TCP）
- 首包分片重组
- 200KB 大包回环
- VLESS UDP（DNS 那种场景）
- 错误 UUID 拒绝
- 错误 WS 路径拒绝
- 30 条并发 × 256KB 回环

```sh
cd src
go test -v -count=1 .
```

注意：测试是去连 `127.0.0.1:3117` 上已经跑着的实例，先起服务再跑测试。
