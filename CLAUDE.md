## 项目概述

npm-auto-proxy 是一个面向 npm registry 的高并发 HTTP 路径转发代理（Go 1.24）。对每个请求**并发竞速 HEAD 探测**多个上游，再从**最高优先级的健康上游**下载，下载失败自动回退到下一个。每个上游可单独配置固定 IP 解析与 HTTP/SOCKS5 代理。可选开启**包元数据 tarball URL 重写**，把上游返回的 `dist.tarball` 绝对 URL 改写为指向本代理，便于下游经本代理下载 tarball。

## 常用命令

构建 / 运行：
- `./build.sh v1.0.0` — 构建二进制并通过 ldflags 注入 `main.version`
- `go build` — 普通构建
- `./npm-auto-proxy start` — 启动（不带子命令时默认即 `start`）
- `DEBUG=1 ./npm-auto-proxy start` — 开启 per-request debug 日志
- `./npm-auto-proxy check` — 逐个探测上游连通性（全部成功 exit 0，否则 exit 1）

质量：
- `go vet ./...` — 静态检查
- `go test ./...` — 全量测试
- `go test ./internal/proxy/ -run TestRaceSkipsUnhealthy` — 运行单个测试
- `go test -race ./...` — 竞速检测（HEAD race 的并发测试在 `router_race_test.go`，改动竞速逻辑时务必带 `-race`）

环境变量：
- `CONFIG_PATH` — 配置文件路径（默认 `config.yaml`）
- `DEBUG` — 任意非空值开启 debug 日志
- `REMOTE_CONFIG_URL` — `download-config` 子命令下载配置的目标 URL
- 启动时自动读取可选的 `.env`，已有环境变量不被覆盖（见 `main.go:loadEnv`）

## 架构

`main.go` 按子命令分发：加载 config → 构建 `proxy.Router` → 交给 `httpserver.Server`。核心逻辑集中在 `internal/proxy`。

### 请求的两阶段策略（最重要的"大局"）

`Router.ServeHTTP` 做最长前缀路由匹配，命中后交给 `compiledRoute.serve`，后者按方法分三条路径：

1. **单候选**：直接 `Forward`，失败回 502。
2. **GET**：先 `raceHead` 并发 HEAD 探测所有候选 → 得到健康上游 → 按 priority 升序逐个下载，某上游 `Forward` 失败则回退下一个；全部失败回 502。
3. **非 GET**：请求 body 只能被消费一次，**既无法 race 也无法回退**，只转发给最高优先级候选。

改动请求流程时必须同时兼顾 GET 的回退链与非 GET 的"一锤定音"差异。

### HEAD race 三段式（`raceHead` / `raceHeadOnce`）

- 并发对所有候选发 HEAD，先等 `strategy.head.firstTimeout` 内的**首个**成功；
- 首个成功后把等待重置为 `strategy.head.grace`，给其余上游一个宽限期；
- 若整轮全部超时，按 `strategy.head.retries` 重跑整轮（总尝试 = retries+1）；
- status 在 `[200, 400)` 视为健康。

### `Upstream.Forward` 的返回约定（fallback 的基石）

`Forward` 返回 `true` 表示响应**已提交**（status < 400）；返回 `false` 表示连接/协议错误或上游 ≥ 400，此时**绝不向 ResponseWriter 写入任何字节**，把回退或 502 的决定权留给 `serve`。任何改动都要保持"false 即零写入"这一不变式，否则回退链会写出脏响应。

### 包元数据 tarball URL 重写（rewrite，`internal/proxy/metadata.go`）

开启 `rewrite.enabled` 后，`Forward` 在响应已确定提交（status < 400）之后，对**包元数据**请求（`GET` 且路径不含 `/-/`，见 `isPackageMetadataPath`）做特殊处理：缓冲整个响应体，解析 JSON，把每个 `versions[].dist.tarball` 与顶层 `dist.tarball` 的 `scheme://host` 替换为本代理地址（保留原 path/query，见 `rewriteTarballURL`）后输出。tarball 下载与其他响应仍走原始流式透传，不受影响。

**gzip 处理**：下游（Verdaccio/npm）会带 `Accept-Encoding: gzip`，若原样转发，上游返回的 gzip 元数据无法被 `json.Unmarshal` 解析、tarball 不会被重写（直接透传未重写的上游字节）。故元数据 GET 在**出站前剥离 `Accept-Encoding`**，由 Go transport 自行加 gzip 并透明解压，保证 `resp.Body` 恒为明文 JSON。仅对元数据路径生效，tarball 下载仍保留客户端的编码。

重写目标 base **按每个请求**动态确定（`rewriteBaseURL`），优先级：配置 `rewrite.externalUrl` > `X-Forwarded-Proto` + `X-Forwarded-Host`（经反向代理） > 请求自身 `Host`；转发头取逗号链最左（原始客户端）值。重写后 body 长度可能变化，故删 `Content-Length` 并强制 `Content-Type: application/json`。**保留**上游 `ETag`/`Last-Modified`：重写是上游 body 的确定性函数，上游文档不变 ⟹ 重写后 body 不变，故上游校验器对重写字节仍有效。转发客户端的 `If-None-Match`/`If-Modified-Since`，上游回 304 时 `serveRewrittenMetadata` 直接透传 304（带 ETag、空 body），下游复用本地缓存、避免重复拉取整份元数据。缓冲上限 `maxMetadataBytes`（64 MB）防异常大响应。读 body 失败仍返回 false（零写入）以保留回退。
### tarball 磁盘缓存（cache，`internal/cache/`）

可选功能，配置 `cache.directories`（数组，每项 `{path, type}`，`type` ∈ `read`（默认）/`write`，**最多一个 `write`**）即开启。**仅缓存压缩包**——`isCachablePath` 判定路径以 `.tgz`/`.tar.gz`/`.zip`/`.gz`/... 结尾；包元数据路径无此后缀，天然不缓存。缓存**永久**（无 TTL / 无淘汰 / 无容量上限）。缓存键即**请求路径**（`cacheRelPath`：剥前导 `/`、拒绝 `..` 防穿越；带查询串时追加短哈希避免碰撞），例如 `/@scope/pkg/-/pkg-1.0.0.tgz` → `<writeDir>/@scope/pkg/-/pkg-1.0.0.tgz`。**不**兼容 pnpm 的 content-addressable store（pnpm 存的是解压后的单文件 blob，无整包 tarball 可读），缓存目录是本代理自管的目录。

`Router` 持有 `*cache.Cache`（`NewRouter` 从 `cfg.CacheReadDirs()`/`cfg.CacheWriteDir()` 构建，无缓存配置时为 nil）。`compiledRoute.serve` 里两处接入（均在 GET 且命中可缓存路径时）：

1. **读命中（`ServeHit`）**：在竞速 / 加锁之前先查所有读目录（含 write 目录）。命中即直接流式回客户端（`Content-Type: application/octet-stream`、`Content-Length`、`X-Cache: HIT`），**完全绕过** HEAD 竞速与上游下载。
2. **按路径加锁去重（`Acquire`）**：读未命中后，`Acquire` 返回按缓存键分桶的 `*sync.Mutex`（引用计数，最后一个释放时从 map 删除，避免无限增长）。持锁期间再次 `ServeHit`（可能已被并发的前一个 leader 缓存）；故同一路径并发请求只下载一次，其余等待后命中缓存。锁在 `serve` 返回时释放（`defer`），保证缓存文件落盘后才放行后续 waiter。
3. **写入（`Capture` + `teeResponseWriter`）**：`captureWriter` 把 `w` 包成 tee，下载字节边流给客户端边 tee 到 `<writeDir>/.tmp/cap-*` 临时文件。`finalize(true)` 用 `os.MkdirAll` + `os.Rename` **原子落盘**（同文件系统，POSIX rename 原子替换目标，并发写入互不冲突）；失败 / 未写入时 `finalize(false)` 删除临时文件。`New` 时清理上次崩溃残留的 `.tmp` 临时文件。

**保持 `Forward` 的零写入不变式**：失败（`Forward` 返回 false）时不写入任何字节，临时文件根本不会创建（懒创建于首次 `Write`），`finalize(false)` 是廉价 no-op。另：`teeResponseWriter.truncated` 比对响应头 `Content-Length` 与实际写入字节，**上游中途断流时丢弃残缺包**（`Forward` 在 status<400 时即便 `io.Copy` 出错也返回 true，靠此守卫避免缓存截断的 tarball）；客户端断开（`Write` 报错）时 `writeErr` 置位同样丢弃。非可缓存路径 / 无 write 目录 / 非 GET 时 `Capture` 原样返回 `(w, nil)`，零开销。

### 优先级语义

`priority` **数值越小越优先**。`sortByPriority` 做升序稳定排序，下载顺序与健康列表排序都依赖它。

### 每上游独立运行时（`internal/proxy/upstream.go`）

每个上游有独立的 `http.Transport`（连接池）和两个 client：`headClient`（不跟随重定向）与 `forwardClient`（跟随重定向，带下载超时）。可选能力在 `buildTransport` 内按顺序叠加：先 `resolve`（固定 IP 拨号，保留 Host 头 / TLS SNI），再 `proxy`（`http`/`https`/`socks5`/`socks5h`，proxy 启用时优先于 resolve）。

### 配置加载（`internal/config/config.go`）

`Load` 顺序：填默认值 → YAML 解析 → `normalizeRoutes`（候选来源优先级：`upstreams` > `upstream` > 全部上游）→ `validate` → 按最长前缀稳定排序路由（保证 `/` 兜底不淹没具体路由）。所有时长字段经 `parseDurationDefault`，空串回落到默认值。

### 日志分层

- `Upstream` 层（download 失败、HEAD 整轮重试等）用 `log.Printf` **无条件**输出，属运维必需的错误日志，生产环境也保留。
- per-request 的路由匹配 / HEAD 竞速 / 回退细节由 `Router.debug`（`DEBUG` 环境变量）控制，经 `(*Router).debugf` 输出带 `[debug]` 前缀的行，关闭时为零开销空函数。

约定：新增请求级诊断信息走 `debugf`（仅 debug 时可见），新增真实错误走 `log.Printf`（始终可见）。
