# npm-auto-proxy

面向 npm registry 的高并发 HTTP 路径转发代理，设计上放在 [Verdaccio](https://verdaccio.org) 等前置作为上游转发器。对每个请求**并发竞速 HEAD 探测**多个上游，再从**最高优先级的健康上游**下载，失败自动回退到下一个。每个上游可单独配置固定 IP 解析与 HTTP/SOCKS5(S) 代理（支持鉴权）。

## 特性

- 并发 HEAD 竞速，发现当前哪些上游可服务该资源
- 按优先级下载，失败自动回退到下一个健康上游
- 宽限窗口：首个 HEAD 成���后，给其余上游一小段额外时间再忽略它们
- 重试：若整轮 HEAD 全部超时，按配置次数重跑整轮竞速
- 每上游独立连接池（高并发、流式下载）
- 每上游固定 IP 拨号（保留 Host 头 / TLS SNI）—— 适合内网域名
- 每上游可选代理：`http://`、`https://`、`socks5://`、`socks5h://`（支持鉴权）
- 路径前缀路由（最长前缀匹配），可选前缀剥离
- 可选重写包元数据 `dist.tarball` URL 指向本代理（解决本地镜像 / 反向代理下的 tarball 自指或不可达；重写目标按 `externalUrl` → `X-Forwarded-*` → 请求 `Host` 推断）
- 可选 tarball 磁盘缓存：仅缓存压缩包（`.tgz`/`.tar.gz`/`.zip`/…），**永不缓存元数据**；永久缓存、按请求路径建键；支持多个只读目录 + 一个写入目录；命中即直接回源绕过上游；同一路径并发请求加锁去重，只下载一次
- 优雅关闭，可配置各类服务端超时

## 工作原理

对一个匹配到路由的 `GET` 请求：

1. **并发 HEAD 探测**所有候选上游。
2. 在 `strategy.head.firstTimeout` 内等待**首个**成功；一旦有成功，再额外等待 `strategy.head.grace` 收集其余上游，随后忽略未返回的。
3. 若窗口内**无**任何 HEAD 成功，按 `strategy.head.retries` 重跑整轮竞速。
4. 在 HEAD 健康的上游中，按**优先级（数值越小越优先）**依次下载。某次下载失败（网络错误或 HTTP ≥ 400）则回退到下一个健康上游。
5. 若竞速后无任何健康上游，降级为按优先级尝试所有候选。

非 `GET` 请求跳过竞速（请求 body 只能被消费一次），直接转发给单个最高优先级候选。

> `priority` 的语义是**数��越小越优先**。想让某个上游排在最前，就给它最小的数。

## 快速开始

```bash
./build.sh v1.0.0
cp config-example.yaml config.yaml   # 编辑上游 / 路由 / 代理
./npm-auto-proxy check               # 验证每个上游是否可达
./npm-auto-proxy start
DEBUG=1 ./npm-auto-proxy start       # 开启 per-request debug 日志
```

## 配置

完整注释示例见 [config-example.yaml](config-example.yaml)。

```yaml
http:
  addr: ":8080"
  readHeaderTimeout: 10s

strategy:
  head:
    firstTimeout: 30s   # 等待首个 HEAD 成功的最长时间
    grace: 5s           # 首个成功后，继续等待其余上游的宽限期
    retries: 2          # 整轮全部超时后的重试次数（总尝试 = retries+1）
  download:
    timeout: 0s         # 单上游下载超时（0 = 不限）

upstreams:
  - name: intra
    url: http://your-intranet-registry.example.com
    resolve: 10.0.0.10          # 固定 IP 拨号（保留 Host/SNI）
    priority: 1                 # 最优先（最快）

  - name: npmmirror
    url: https://registry.npmmirror.com
    priority: 2                 # 快且全

  - name: npmjs
    url: https://registry.npmjs.org
    priority: 3                 # 兜底（走代理）
    proxy:
      enabled: true
      url: socks5://127.0.0.1:7891          # 或 http://127.0.0.1:7891
      # 带鉴权： socks5://user:pass@127.0.0.1:7891

routes:
  - prefix: /
    upstreams: [intra, npmmirror, npmjs]     # 参与竞速的候选；省略则用全部上游
    stripPrefix: false

rewrite:
  enabled: false        # 重写元数据里的 dist.tarball URL 指向本代理（默认关）
  # externalUrl: http://127.0.0.1:8080      # 显式重写 base；留空则按请求动态推断

cache:
  directories:                        # 可选；省略整段即关闭缓存
    # - path: /home/you/.local/share/pnpm/store   # 只读缓存目录（例如复用本机已有缓存）
    #   type: read                                  # type 默认 read
    - path: /var/cache/npm-auto-proxy # 写入目录（同时参与读取）；最多只能有一个 write
      type: write
```

### Upstream 字段

| 字段 | 说明 |
|-------|-------------|
| `name` | 上游唯一标识 |
| `url` | registry 基础 URL |
| `priority` | 数值越小越优先（在 HEAD 健康的上游中先试） |
| `resolve` | 固定拨号到该 IP（保留 Host/SNI） |
| `maxIdleConns` / `idleConnsPerHost` | 连接池大小（默认 100 / 32） |
| `insecureSkipVerify` | 跳过上游 TLS 校验（谨慎使用） |
| `proxy.enabled` | 让该上游的流量走 `proxy.url` |
| `proxy.url` | `http://`、`https://`、`socks5://` 或 `socks5h://`（支持鉴权） |

### Routes 字段

| 字段 | 说明 |
|-------|-------------|
| `prefix` | 匹配的路径前缀（必须以 `/` 开头） |
| `upstreams` | 参与竞速的候选上游名；省略则用全部上游 |
| `upstream` | 单候选简写（会填充到 `upstreams`） |
| `stripPrefix` | 转发前剥除 `prefix` |

匹配会自动按最长前缀优先排序。

### Cache 字段

可选的 tarball 磁盘缓存。**仅缓存压缩包**（路径以 `.tgz`/`.tar.gz`/`.zip`/`.gz`/… 结尾），**包元数据永不缓存**。缓存永久有效（无 TTL、无淘汰、无容量上限）。

| 字段 | 说明 |
|-------|-------------|
| `directories` | 缓存目录数组，按顺序参与读取 |
| `directories[].path` | 目录路径（相对路径会按工作目录转为绝对） |
| `directories[].type` | `read`（默认，只读）或 `write`（同时读写）。**最多一个** `write` |

要点：

- 命中即直接从磁盘流式回给客户端，**完全绕过** HEAD 竞速与上游下载，响应带 `X-Cache: HIT`。
- 写入采用「临时文件 + 原子改名」（`<writeDir>/.tmp/` 临时区 → `os.Rename`），并发写入互不冲突；启动时自动清理上次崩溃残留的临时文件。
- 同一路径的并发请求通过**按路径加锁去重**：首个请求下载并缓存，其余等待后命中缓存，因此同一 tarball 同时只下载一次。
- 下载被中途截断（与声明的 `Content-Length` 不符，或客户端断开）时不写入缓存，避免缓存残缺文件。
- 缓存键即请求路径，例如 `/@scope/pkg/-/pkg-1.0.0.tgz` 存到 `<writeDir>/@scope/pkg/-/pkg-1.0.0.tgz`；含查询串的 URL 会附加短哈希以避免碰撞。

## 命令

| 命令 | 说明 |
|---------|-------------|
| `start` | 启动代理（默认） |
| `check` | 检查每个配置上游的连通性 |
| `help` | 显示帮助 |
| `download-config` | 从 `REMOTE_CONFIG_URL` 下载配置 |
| `version` | 显示版本 |

## 环境变量

| 变量 | 说明 | 默认值 |
|----------|-------------|---------|
| `CONFIG_PATH` | 配置文件路径 | `config.yaml` |
| `DEBUG` | 开启 per-request debug 日志（每个请求的路由匹配、HEAD 竞速、选中的上游与耗时） | （未设置） |
| `REMOTE_CONFIG_URL` | `download-config` 使用的 URL | （未设置） |

## 构建

```bash
./build.sh [version]   # 本地构建
go vet ./...           # 静态检查
go test ./...          # 测试（覆盖路由、竞速、回退、重试）
```

## License

Apache 2.0
