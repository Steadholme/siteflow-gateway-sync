# siteflow-gateway-sync

把 SiteFlow 部署站点的公网 host 同步成 Steadholme 网关（Sluice）的 public 路由，让
部署站点能从外网访问。纯 Go、无状态、单向 reconciler，照 estate Go 服务惯例
（`net/http` healthz、结构化 slog、优雅退出、scratch 多阶段镜像）。

## 它做什么

每 30s（`SYNC_INTERVAL` 可调）一轮：

1. 从 **只读** SiteFlow 库收集应公网可达的 host：
   - `siteflow_artifact_routes.host` 中排除内网（`*.holdfast.internal`）后的其余
     host（公网 preview / 生产 host，形如 `*.siteflow.w33d.xyz`）。
   - 并上 `siteflow_project_domains WHERE verified = true` 的 `hostname`（自定义
     域，仅已校验）。
   - 去重、小写化、合法性过滤（非空、含点、无空格、无 `*`、不等于控制台
     `siteflow.w33d.xyz`）。
2. 对每个 host upsert 一行到 Steadholme `routes`：
   `name='sfsite-'+substr(sha1(host),0,12)`、`path_prefix='/'`、
   `upstream=$SITEFLOW_UPSTREAM`、`protected=false`、`auth='public'`、
   `require_group=''`，`waf` 用列默认（不写、不覆盖）。
3. **prune**：删除 `routes` 中 `name LIKE 'sfsite-%'` 且 host 不在本轮集合的行。

## 安全红线

- **命名空间隔离**：只读写 `name LIKE 'sfsite-%'` 的行。upsert / delete 都在
  应用层（`assertManaged`）+ SQL 层（`WHERE routes.name LIKE 'sfsite-%'` /
  `WHERE name = $1 AND name LIKE 'sfsite-%'`）双重约束，非 `sfsite-` 前缀的
  estate 核心路由（relay-*、cistern-* 等）**在任何代码路径下都不会被改动**。
- **只读源**：SiteFlow 连接池以 `default_transaction_read_only=on` 打开，本服务
  无法写 SiteFlow 库；且只发 SELECT。
- **verified-only**：自定义域只取 `verified = true`（SQL 层过滤），绝不路由未校验
  的用户可控 host（防开放代理 + LE 证书滥签）。
- **fail-safe prune**：读 SiteFlow 出错→整轮中止（不 upsert、不 prune）；成功但
  快照为空→跳过 prune（宁可多留旧路由，也不误删下线一堆站点）。
- 控制台 `siteflow.w33d.xyz`（SSO）永不写成 public 路由。
- SQL 全参数化。

## 环境变量

| 变量 | 必填 | 默认 | 说明 |
|------|------|------|------|
| `SITEFLOW_DATABASE_URL` | 是 | — | **只读** SiteFlow 库 DSN（host 来源）|
| `ROUTES_DATABASE_URL` | 是 | — | **读写** Steadholme 库 DSN（`routes` 表）|
| `SITEFLOW_UPSTREAM` | 否 | `http://siteflow-api:9360` | 站点路由的上游 |
| `SYNC_INTERVAL` | 否 | `30s` | 轮询间隔（Go duration，如 `30s`）|
| `BIND_ADDR` | 否 | `0.0.0.0:9385` | healthz 监听地址 |

healthz：`GET /healthz` → `200 OK`。端口 **9385**。

## 运行

Go 二进制不在默认 PATH 上：

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./...
go vet ./...
go test ./... -race
```

## Compose stanza（草案，供部署接线）

> 端口 9385 仅内部（healthz），不对外发布。两个 DSN 应指向 estate 内网 Postgres：
> SiteFlow 库用只读角色，Steadholme 库用能写 `routes` 的角色。
> 注意：`SITEFLOW_UPSTREAM` 默认 `http://siteflow-api:9360`，若 SiteFlow API 实际
> 服务名 / 端口不同（如 compose 里的 `8787`），部署时按实际值覆盖。

```yaml
  siteflow-gateway-sync:
    image: ${SITEFLOW_GATEWAY_SYNC_IMAGE:?pin by digest for production}
    restart: unless-stopped
    read_only: true
    user: "65532:65532"
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    environment:
      # 只读 SiteFlow 库（建议专用只读角色）
      SITEFLOW_DATABASE_URL: "postgres://sync_ro@siteflow-postgres:5432/siteflow?sslmode=disable"
      # 读写 Steadholme routes 库
      ROUTES_DATABASE_URL: "postgres://routes_rw@holdfast-postgres:5432/steadholme?sslmode=disable"
      SITEFLOW_UPSTREAM: "http://siteflow-api:9360"
      SYNC_INTERVAL: "30s"
      BIND_ADDR: "0.0.0.0:9385"
    networks:
      - estate-internal
    # HEALTHCHECK 内建于镜像（-healthcheck 探 /healthz）
```
