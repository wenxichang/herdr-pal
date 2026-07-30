# Herdr Pal 审计服务部署指南

本文部署一套单机审计服务，由 Loki 原生 OTLP 接口直接接收 `herdr-pal-server` 产生的日志，
持久化保存用户输入和终端文本输出，并可选择通过 Grafana 查询。该方案适合当前单机、内网和
中小规模部署；需要高可用或跨机房容灾时，应改用对象存储和多实例 Loki。

## 1. 架构与安全边界

```text
herdr-pal-server
       │ OTLP/HTTP protobuf Logs
       ▼
Loki ──▶ Grafana（可选查询界面）
:3100    :3000
```

- Server 直连 Loki 的 `/otlp/v1/logs`，不需要 OpenTelemetry Collector。
- Loki 使用本机文件系统持久化，默认保留 30 天。
- Grafana 是可选查询界面，启用后会自动配置 Loki 数据源。
- 所有服务端口都只监听 `127.0.0.1`。Loki 本身不提供认证，不得直接暴露公网。
- Grafana 通过 SSH 端口转发访问，不开启匿名访问和用户自助注册。
- 审计正文包含脱敏后的用户输入和终端文本，仍应按敏感数据管理。

官方参考：

- [OpenTelemetry Collector Docker 部署](https://opentelemetry.io/docs/collector/install/docker/)
- [Loki Docker 部署](https://grafana.com/docs/loki/latest/setup/install/docker/)
- [Loki 原生 OTLP 接口](https://grafana.com/docs/loki/latest/send-data/otel/)

## 2. 环境要求

- Linux AMD64 或 ARM64。
- Docker Engine 与 Docker Compose v2。
- 建议至少 2 CPU、4 GiB 内存和独立的可用磁盘空间。
- `3100` 和 `3000` 的本机回环端口未被占用。

Ubuntu 可使用发行版软件包安装：

```sh
sudo apt-get update
sudo apt-get install -y docker.io docker-compose-v2
sudo systemctl enable --now docker
```

## 3. 安装审计服务

在 Herdr Pal 仓库根目录执行：

```sh
mkdir -p ~/herdr-pal-audit
cp -R deploy/audit/. ~/herdr-pal-audit/
cd ~/herdr-pal-audit
```

创建 `.env`。即使暂不启用 Grafana，也建议预先生成管理密码，避免以后启动时临时修改部署
文件：

```sh
umask 077
printf 'GRAFANA_ADMIN_USER=admin\nGRAFANA_ADMIN_PASSWORD=%s\n' \
  "$(openssl rand -base64 24 | tr -d '\n')" > .env
```

默认从 Docker Hub 拉取官方镜像。如果服务器无法访问 Docker Hub，可在 `.env` 中覆盖镜像
地址而不修改 Compose 文件，例如使用组织允许的可信镜像仓库：

```text
LOKI_IMAGE=你的仓库/grafana/loki:3.7.0
GRAFANA_IMAGE=你的仓库/grafana/grafana:13.1.0
```

镜像代理只应改变仓库前缀，不应改变版本；正式环境还应记录并核对镜像摘要。

校验配置并只启动必需的 Loki：

```sh
sudo docker compose --env-file .env config --quiet
sudo docker compose --env-file .env pull loki
sudo docker compose --env-file .env up -d loki
```

检查运行状态：

```sh
sudo docker compose --env-file .env ps
curl -fsS http://127.0.0.1:3100/ready
```

查看组件日志：

```sh
sudo docker compose --env-file .env logs --tail=200 loki
```

需要网页查询界面时再启动 Grafana：

```sh
sudo docker compose --env-file .env pull grafana
sudo docker compose --env-file .env up -d grafana
curl -fsS http://127.0.0.1:3000/api/health
```

## 4. 配置 herdr-pal-server

修改默认配置 `~/.config/herdr-pal/server-config.json`，增加或替换以下部分：

```json
"admin": {
  "listen": "0.0.0.0:4001",
  "loki_url": "http://127.0.0.1:3100"
},
"audit": {
  "type": "otlp",
  "endpoint": "http://127.0.0.1:3100/otlp/v1/logs",
  "skip_verify": false,
  "stderr": false
}
```

`audit.endpoint` 是业务审计写入地址；`admin.loki_url` 是 Web 管理台查询使用的 Loki 基础
地址，两者用途不同。管理台会自行追加 `/loki/api/v1/query_range`，因此不要在
`admin.loki_url` 中填写 API 路径、query 或 fragment。

本机回环 Loki 不需要认证 Header。若以后把 OTLP 服务独立部署到其他机器，应使用 HTTPS
和认证，并通过标准环境变量配置请求头：

```sh
export OTEL_EXPORTER_OTLP_LOGS_HEADERS='Authorization=Bearer%20你的令牌'
```

配置只在 `herdr-pal-server` 启动时读取，修改后需要重启服务端。审计输出是 fail-open：
Loki 临时不可用不会阻断企业微信操作，但在服务端内存队列和重试窗口耗尽后可能丢失
审计事件。

Loki 查询同样隔离故障：`admin.loki_url` 留空、Loki 超时或返回协议错误时，只有管理台
“审计日志”页返回暂不可用；企业微信、HPRP、HPAP、其他管理页面和 OTLP 写入不受影响。

如果需要更长时间的转发重试、批处理或额外转换，可以启动模板中保留的 Collector：

```sh
sudo docker compose --profile collector --env-file .env up -d otel-collector
```

此时把 endpoint 改为 `http://127.0.0.1:4318/v1/logs`。Collector 会继续把日志转发给 Loki。

## 5. 访问与查询

Herdr Pal 内嵌管理台可以直接执行日常审计查询。访问：

```text
https://SERVER:4001/admin/audit
```

登录后可按企业微信用户 ID 精确匹配，按 `machine_id` 和关键字进行不区分大小写的包含
匹配，并限制开始/结束时间。查询由 Server 构造固定 LogQL，不接受任意 LogQL。管理台只把
正文保留在当前页面 DOM 中，关闭详情或切页后清除，不写入浏览器持久化存储。

需要使用 Grafana 进行自由探索时，再从管理电脑建立 SSH 隧道：

```sh
ssh -L 3000:127.0.0.1:3000 用户名@服务器地址
```

打开 `http://127.0.0.1:3000`。用户名默认为 `admin`，密码在服务器
`~/herdr-pal-audit/.env` 中。进入 Explore，选择 `Herdr Pal Audit` 数据源，先用以下 LogQL
查询全部 Herdr Pal 审计日志：

```logql
{service_name="herdr-pal-server"}
```

OTLP 属性中的点会由 Loki 规范化为下划线。可在查询结果的结构化元数据中查看：

- `herdr_pal_audit_principal_id`：企业微信用户 ID。
- `herdr_pal_audit_machine_id`、`herdr_pal_audit_pane_id`：终端目标。
- `herdr_pal_audit_outcome`、`herdr_pal_audit_delivery`：处理和投递结果。
- `herdr_pal_audit_content_bytes`：正文 UTF-8 字节数。

例如筛选被限速的输入：

```logql
{service_name="herdr-pal-server"} | herdr_pal_audit_outcome="rate_limited"
```

终端输出事件带有 `herdr_pal_audit_machine_id`、`herdr_pal_audit_pane_id` 和
`herdr_pal_audit_presentation`，用户输入事件没有终端目标，可据此继续筛选。

## 6. 运维

更新镜像：

```sh
cd ~/herdr-pal-audit
sudo docker compose --env-file .env pull loki
sudo docker compose --env-file .env up -d loki
```

已启用 Grafana 时单独更新：

```sh
sudo docker compose --env-file .env pull grafana
sudo docker compose --env-file .env up -d grafana
```

停止和恢复：

```sh
sudo docker compose --env-file .env stop
sudo docker compose --env-file .env start
```

普通 `docker compose down` 不会删除命名卷；不要使用 `down -v`，否则会删除 Loki 日志和
Grafana 配置。部署目录中的 `.env` 包含 Grafana 密码，权限应保持 `0600`，不得提交到 Git。

Loki 默认保留 30 天，可修改 `loki.yaml` 中的 `retention_period`，修改后执行：

```sh
sudo docker compose --env-file .env up -d loki
```

## 7. 故障排查

- 服务端出现“审计事件输出失败”：检查 Loki `/ready`、磁盘空间和 Loki 日志。
- 使用可选 Collector 时出现导出重试：检查 Collector 日志以及 Loki `/ready`。
- Grafana 没有数据：确认服务端已经用 `audit.type=otlp` 重启，并检查时间范围与
  `{service_name="herdr-pal-server"}` 查询。
- Loki 无法写入：检查 Docker 卷、宿主机磁盘和 Loki 容器日志。
- 配置修改未生效：执行 `docker compose config` 后重新 `up -d`，并确认当前目录和 `.env`
  文件正确。
