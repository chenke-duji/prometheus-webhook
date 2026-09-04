# prometheus-webhook 设计文档

> **状态**：设计阶段，待确认后实施
> **定位**：CEP 体系采集/接入端，与 trap-daemon、syslog-daemon 同级
> **语言**：Go（与现有 daemon 保持一致）
> **日期**：2026-09-04

---

## 1. 项目定位

prometheus-webhook 是一个 HTTP webhook 守护进程，用于接收 Prometheus Alertmanager 推送的告警通知，将每条告警拆分为独立的 RawEvent，批量转发给 cep-engine。

与现有 daemon 的关系：

| Daemon | 入站协议 | source 值 | agentType | 典型数据源 |
|---|---|---|---|---|
| trap-daemon | UDP SNMP Trap (v1/v2c/v3) | `snmp_trap` | `generic` / 厂商 MIB | 网络设备 |
| syslog-daemon | UDP Syslog (RFC3164/5424) | `syslog` | `syslog` | 网络设备/主机日志 |
| **prometheus-webhook** | **HTTP POST (Alertmanager Webhook)** | **`alertmanager`** | **`alertmanager`** | **Prometheus 指标告警** |

三者共享相同的下游管道（批量队列 → HTTP 转发 → cep-engine `/api/v1/events/batch`），区别仅在前端入站协议和数据解析方式。

---

## 2. 架构设计

```
Alertmanager                            cep-engine
    |                                       ^
    | HTTP POST /webhook                    |
    v                                       |
 prometheus-webhook                         |
    ├── HTTP Server                         |
    │   ├── /webhook  (接收 Alertmanager)   |
    │   ├── /metrics (Prometheus 自监控)    |
    │   └── /healthz (健康检查)            |
    ├── Alert 解析 (拆分 alerts 数组)       |
    ├── RawEvent 构建 (source 可配置)       |
    ├── BatchQueue (有界队列 + 背压)         |
    └── HTTPForwarder ────────────────────┘
                    POST /api/v1/events/batch
```

### 核心流程

1. Alertmanager 配置 webhook receiver，将告警通知 POST 到 `http://<webhookd>:9093/webhook`
2. webhookd 的 HTTP Server 接收请求，解析 Alertmanager JSON payload
3. 遍历 `alerts[]` 数组，将每条 alert 构建为一个 RawEvent（`source` 可配置，默认 `"alertmanager"`）
4. 每个 RawEvent 进入 BatchQueue，由 worker 池攒批后 POST 到 cep-engine
5. cep-engine 的 `EventProcessingChain` 通过 `ScriptRegistry.matchScript` 按 `source` 匹配到对应的 Groovy parser，执行解析，将 RawEvent 转换为 AlarmEvent

### HTTP 端点

webhookd 在同一个 HTTP server 上暴露三个端点：

| 路径 | 方法 | 用途 |
|---|---|---|
| `/webhook`（可配置） | `POST` | 接收 Alertmanager 告警通知 |
| `/metrics` | `GET` | Prometheus 格式自监控指标，供 Prometheus scrape |
| `/healthz` | `GET` | 健康检查，返回 `{"status":"ok"}` |

> 不再为 metrics 单独开一个端口（对比 syslog-daemon 的双端口设计）。webhookd 本身就是 HTTP server，在同一端口上用不同路径区分即可，简化部署和 Service 配置。

### 安全设计

webhookd 作为 HTTP 入口暴露在网络中，必须防止非授权的 Alertmanager 或任意客户端注入伪造告警。采用**三层防御**：

#### 第 1 层：Bearer Token 认证（主防线）

Alertmanager 的 `webhook_configs` 原生支持 `http_config.authorization`，可携带 Bearer token。webhookd 在 `/webhook` handler 入口校验 `Authorization` 头：

```
Authorization: Bearer <token>
```

- token 在 webhookd 的 `config.yaml` 中配置（`webhook.authToken`），或通过环境变量 `AMWH_WEBHOOK_AUTH_TOKEN` 注入
- token 为空时（默认）**拒绝所有请求**——必须显式配置才能工作，避免"忘记配置导致裸奔"
- 校验使用 `subtle.ConstantTimeCompare` 防止时序攻击
- `/metrics` 和 `/healthz` 端点**不受 token 保护**（供 Prometheus scrape 和 k8s probe 使用），但可通过 IP 限制访问

#### 第 2 层：IP/CIDR 白名单 + 反向代理感知（网络防线）

在 token 校验之前，先检查请求来源 IP。**反向代理终止 TLS 时，`RemoteAddr` 是代理 IP 而非 Alertmanager IP**，必须通过 XFF 机制提取真实客户端 IP。

**IP 提取逻辑**：

```
请求到达 webhookd
  │
  ├─ RemoteAddr 在 trustedProxies 中？
  │   ├─ 是 → 从 X-Forwarded-For 最左端提取 realClientIP
  │   └─ 否 → RemoteAddr 即 realClientIP（直连场景）
  │
  └─ 对 realClientIP 执行 allowedCIDRs 白名单检查
```

- `webhook.trustedProxies` 配置信任的反向代理 IP/CIDR 列表，如 `["10.0.0.2/32"]`
- **`trustedProxies` 为空时不信任任何 XFF 头**，直接使用 `RemoteAddr`——防止直连场景下伪造 XFF 绕过白名单
- `webhook.allowedCIDRs` 配置允许的 Alertmanager IP/CIDR 列表，如 `["10.0.0.0/8"]`
- 空列表表示**不限制**（仅靠 token 防护）
- 匹配使用 `net/netip` 前缀匹配，支持 IPv4 和 IPv6
- 非白名单 IP 返回 `403 Forbidden`，不进入 token 校验流程
- **`sourceIp` 字段也使用 `realClientIP`**：反向代理场景下 RawEvent 的 `sourceIp` 仍反映真实 Alertmanager IP，排障和 TransportDeduplicator 指纹不受代理层影响

#### 第 3 层：mTLS（可选，零信任环境）

对于跨网络或零信任部署，启用双向 TLS：

- webhookd 的 HTTP server 配置 `tls.certificate`（服务端证书）和 `tls.clientCAFile`（客户端 CA）
- Alertmanager 的 `webhook_configs.http_config.tls_config` 配置客户端证书
- 客户端证书由 webhookd 信任的 CA 签发，建立 TLS 连接时校验
- mTLS 开启时，Bearer token 仍需配置（双重保障）

> **反向代理终止 TLS 是更常见的部署方式**。此时代理↔webhookd 为明文 HTTP，TLS 安全由代理层保障。webhookd 应配合 `trustedProxies` 配置，从 XFF 提取真实客户端 IP。不需要同时在 webhookd 侧启用 mTLS——除非代理与 webhookd 之间的网络不可信。

#### 请求处理流程

```
HTTP 请求到达 /webhook
  │
  ├─ 1. 提取 realClientIP（trustedProxies 感知 XFF）
  │
  ├─ 2. IP 白名单检查 → 不在白名单 → 403 Forbidden
  │
  ├─ 3. Bearer Token 校验 → 不匹配/缺失 → 401 Unauthorized
  │
  ├─ 4. Body 大小限制 → 超限 → 413 Request Entity Too Large
  │
  ├─ 5. JSON 解析 → 格式错误 → 400 Bad Request
  │
  └─ 6. 正常处理 → 拆分 alerts → 入队 → 200/202
```

#### 部署拓扑与安全模型

| 拓扑 | TLS 终止位置 | IP 白名单检查对象 | Token | 说明 |
|---|---|---|---|---|
| 直连（无代理） | webhookd mTLS 或裸 HTTP | `RemoteAddr` = AM IP | 必须 | 最简单，白名单直接对 AM IP |
| 反向代理终止 TLS | Nginx/HAProxy（HTTPS） | XFF 提取的 AM IP | 必须 | webhookd 信任代理 IP，从 XFF 取 AM IP；代理↔webhookd 为明文 |
| 反向代理 + mTLS 到 webhookd | 代理↔webhookd 也加密 | XFF 提取的 AM IP | 必须 | 最严格，代理和 webhookd 之间也加密 |

#### 安全配置要求

| 场景 | 推荐配置 |
|---|---|
| 最小可用 | `authToken` 设为随机 32+ 字符串，`allowedCIDRs` 留空 |
| 直连生产 | `authToken` + `allowedCIDRs` 限定 Alertmanager IP 段 |
| 反向代理生产 | `authToken` + `allowedCIDRs`（AM IP 段）+ `trustedProxies`（代理 IP） |
| 零信任/跨网 | `authToken` + `allowedCIDRs` + `trustedProxies` + mTLS |

> **token 生成建议**：`openssl rand -hex 32` 生成 64 字符十六进制字符串。token 仅存储在 webhookd 的 config.yaml（或环境变量）和 Alertmanager 的 alertmanager.yml 中，不记录到日志。

#### 反向代理 Nginx 配置示例

webhookd 前置 Nginx 终止 TLS 并传递真实客户端 IP：

```nginx
# /etc/nginx/conf.d/prometheus-webhook.conf

upstream prometheus_webhook {
    server 127.0.0.1:9093;
    keepalive 16;
}

server {
    listen 443 ssl http2;
    server_name prometheus-webhook.internal;

    # ---- TLS 证书 ----
    ssl_certificate     /etc/nginx/tls/webhook.crt;
    ssl_certificate_key /etc/nginx/tls/webhook.key;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_ciphers         HIGH:!aNULL:!MD5;

    # ---- 仅允许 Alertmanager 来源 IP（纵深防御）----
    allow 10.0.0.0/8;       # Alertmanager 网段
    allow 192.168.1.0/24;   # 备用 Alertmanager 网段
    deny  all;

    # ---- /webhook：转发到 webhookd ----
    location /webhook {
        proxy_pass http://prometheus_webhook;

        # 传递真实客户端 IP（webhookd 的 trustedProxies 需配 Nginx IP）
        proxy_set_header X-Forwarded-For  $proxy_add_x_forwarded_for;
        proxy_set_header X-Real-IP       $remote_addr;
        proxy_set_header Host            $host;

        # 请求体大小限制（与 webhookd maxBodyBytes 一致）
        client_max_body_size 1m;

        # 超时
        proxy_connect_timeout 5s;
        proxy_send_timeout    10s;
        proxy_read_timeout    30s;
    }

    # ---- /metrics 和 /healthz：仅内部访问 ----
    location /metrics {
        allow 127.0.0.1;          # Prometheus scrape IP
        allow 10.0.1.0/24;        # 监控网段
        deny all;
        proxy_pass http://prometheus_webhook;
        proxy_set_header Host $host;
    }

    location /healthz {
        allow 127.0.0.1;
        allow 10.0.1.0/24;
        deny all;
        proxy_pass http://prometheus_webhook;
        proxy_set_header Host $host;
    }

    # 拒绝其他路径
    location / {
        return 404;
    }
}
```

**Nginx 配置要点**：

| 配置项 | 说明 |
|---|---|
| `ssl_certificate` / `ssl_certificate_key` | TLS 证书，由 CA 签发，覆盖 Alertmanager → Nginx 链路加密 |
| `allow` / `deny` | Nginx 层 IP 限制（纵深防御），与 webhookd 的 `allowedCIDRs` 形成双重过滤 |
| `proxy_set_header X-Forwarded-For` | 传递真实 Alertmanager IP，webhookd 通过 `trustedProxies` 信任并解析 |
| `proxy_set_header X-Real-IP` | 备用真实 IP 头，部分场景 webhookd 可作为 XFF 的 fallback |
| `client_max_body_size` | 与 webhookd 的 `maxBodyBytes` 保持一致，避免代理层放行但 webhookd 拒绝 |
| `/metrics` 和 `/healthz` 的 `allow/deny` | 限制仅监控网段可访问，防止指标数据泄露 |

**webhookd 配置（配合上述 Nginx）**：

```yaml
webhook:
  listenAddr: "0.0.0.0:9093"     # 仅监听本地或内部网络
  authToken: "<random-token>"
  allowedCIDRs:
    - "10.0.0.0/8"               # Alertmanager 网段
    - "192.168.1.0/24"
  trustedProxies:
    - "127.0.0.1/32"             # Nginx（同机部署）
    # - "10.0.0.2/32"            # Nginx（跨机部署时填 Nginx IP）
tls:
  enabled: false                 # TLS 由 Nginx 终止，webhookd 侧不需要
```

### Active-Active 去重

多个 webhookd 实例可部署在负载均衡后。Alertmanager 的 `--webhook.config` 支持多 receiver URL。同一告警通知被多个 webhookd 实例接收时，cep-engine 的 `TransportDeduplicator` 负责去重。

**TransportDeduplicator 指纹构成**（`TransportDeduplicator.buildFingerprint()`）：

```
fingerprint = source | sourceIp | originTimestamp | rawEvent.hashCode()
```

| 指纹分量 | Alertmanager 场景取值 | 多实例一致性 |
|---|---|---|
| `source` | 配置值（如 `"alertmanager"`） | 相同（所有实例配置相同） |
| `sourceIp` | realClientIP（XFF 感知后的 AM IP） | 相同（同一 AM 发出；代理场景由 trustedProxies + XFF 提取） |
| `originTimestamp` | alert 的 `startsAt` 转 epoch millis | 相同（Alertmanager 保证） |
| `rawEvent.hashCode()` | 确定性渲染的告警文本的哈希 | 相同（见下） |

**`rawEvent` 确定性渲染**——必须包含以下字段才能保证：
- **同一告警的多实例指纹相同**（正确去重）
- **不同告警的指纹不同**（不误去重）

| rawEvent 包含字段 | 作用 | 为何重要 |
|---|---|---|
| `status` | `firing` / `resolved` | 同一告警的 firing 和 resolved 是不同事件，必须区分 |
| `labels`（完整标签集） | alertname, instance, severity, job 等 | **不同告警可能有相同 `startsAt`**（同一评估周期触发多条告警），labels 是区分不同告警的唯一依据 |
| `fingerprint` | Alertmanager 自带的标签集哈希 | 冗余校验：与 labels 一致，额外保证确定性 |
| `startsAt` | 告警触发时间 | 与 originTimestamp 一致，写入以保持 rawEvent 自包含 |
| `generatorURL` | Prometheus 表达式链接 | 含告警规则标识，辅助区分 |

| rawEvent **排除**字段 | 原因 |
|---|---|
| `receivedAt` | webhookd 实例特定的接收时刻，各实例不同 |
| `endsAt`（resolved 时） | Alertmanager 在 resolved 通知中设置结束时间，虽然同一 AM 下各实例相同，但 firing 和 resolved 的 endsAt 不同——已在 `status` 中体现，不需要重复 |

> **关键点**：`startsAt` 单独不足以区分不同告警。Alertmanager 一次评估周期可能同时触发多条 `startsAt` 相同的告警（如 CPU 高 + 磁盘满同时发生）。`labels` + `status` 才是指纹的核心区分字段。

---

## 3. Alertmanager Webhook 接口设计

### 3.1 接收端点

| 属性 | 值 |
|---|---|
| Method | `POST` |
| Path | 可配置，默认 `/webhook` |
| Content-Type | `application/json` |
| 请求体 | Alertmanager Webhook Payload (v4+) |

### 3.2 Alertmanager Webhook Payload 结构

Alertmanager 推送的 JSON 结构（参考 Alertmanager 官方文档）：

```json
{
  "version": "4",
  "groupKey": "alertname=HighCPU,instance=node1",
  "truncatedAlerts": 0,
  "status": "firing",
  "receiver": "cep-webhook",
  "groupLabels": { "alertname": "HighCPU" },
  "commonLabels": { "alertname": "HighCPU", "severity": "critical" },
  "commonAnnotations": { "summary": "CPU usage > 80%" },
  "externalURL": "http://alertmanager:9093",
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "HighCPU",
        "instance": "node1.example.com",
        "severity": "critical",
        "job": "node_exporter"
      },
      "annotations": {
        "summary": "CPU usage > 80%",
        "description": "CPU usage on node1 is 92%"
      },
      "startsAt": "2026-09-04T10:00:00.000Z",
      "endsAt": "2026-09-04T10:05:00.000Z",
      "generatorURL": "http://prometheus:9090/graph?g0.expr=cpu_usage"
    }
  ]
}
```

### 3.3 HTTP 响应

| 场景 | HTTP Status | Body |
|---|---|---|
| 成功 | `200 OK` | `{"status":"accepted","count":N}` |
| 来源 IP 不在白名单 | `403 Forbidden` | `{"status":"error","reason":"ip not allowed"}` |
| Token 缺失或不匹配 | `401 Unauthorized` | `{"status":"error","reason":"unauthorized"}` |
| 请求体超过大小限制 | `413 Too Large` | `{"status":"error","reason":"body too large"}` |
| 请求体解析失败 | `400 Bad Request` | `{"status":"error","reason":"..."}` |
| 空 alerts 数组 | `200 OK` | `{"status":"accepted","count":0}` |
| 队列满且策略为 drop | `202 Accepted` | `{"status":"accepted","count":N,"dropped":M}` |
| 内部错误 | `500 Internal Server Error` | `{"status":"error","reason":"..."}` |

> 安全错误响应（401/403）统一返回相同的 JSON 结构，**不泄露具体失败原因**（如 token 是否存在、IP 具体值等）。响应体中的 reason 仅为前端排查用，生产环境可通过日志级别控制是否记录详细原因。

---

## 4. RawEvent 构建

每条 alert 构建为一个 RawEvent，JSON 结构严格对齐 cep-engine 的 `com.dujitech.cep.model.RawEvent`（Gson 反序列化，JSON key 必须与 Java 字段名一致）：

```json
{
  "source": "alertmanager",
  "sourceIp": "192.168.1.100",
  "receivedAt": 1725441600000,
  "originTimestamp": 1725441600000,
  "rawEvent": "alertmanager|firing|HighCPU|node1.example.com|critical|node_exporter|e3b0c44298fc1c14|2026-09-04T10:00:00.000Z|http://prometheus:9090/graph?g0.expr=cpu_usage",
  "metadata": {
    "status": "firing",
    "fingerprint": "e3b0c44298fc1c14",
    "labels": {
      "alertname": "HighCPU",
      "instance": "node1.example.com",
      "severity": "critical",
      "job": "node_exporter"
    },
    "annotations": {
      "summary": "CPU usage > 80%",
      "description": "CPU usage on node1 is 92%"
    },
    "startsAt": "2026-09-04T10:00:00.000Z",
    "endsAt": "2026-09-04T10:05:00.000Z",
    "generatorURL": "http://prometheus:9090/graph?g0.expr=cpu_usage",
    "groupKey": "alertname=HighCPU,instance=node1",
    "receiver": "cep-webhook",
    "externalURL": "http://alertmanager:9093",
    "commonLabels": { "alertname": "HighCPU", "severity": "critical" },
    "groupLabels": { "alertname": "HighCPU" },
    "domainId": "default"
  }
}
```

### `rawEvent` 确定性渲染规则

`rawEvent` 字段是 TransportDeduplicator 指纹的核心组成部分，必须满足**确定性**（同一告警在所有 webhookd 实例中产生相同字符串）和**区分性**（不同告警产生不同字符串）。

渲染格式为 `|` 分隔的有序字段拼接：

```
source|status|alertname|instance|severity|job|fingerprint|startsAt|generatorURL
```

- 各字段从 alert 的 `labels` 和 payload 中提取
- 缺失字段渲染为空字符串（保留 `|` 分隔符位置不变）
- `fingerprint` 取自 Alertmanager payload 中 alert 的 `fingerprint` 字段
- 不含 `receivedAt`、`endsAt`、`annotations`

### 字段来源

| RawEvent 字段 | 取值来源 | 说明 |
|---|---|---|
| `source` | **配置项 `webhook.source`**，默认 `"alertmanager"` | 可配置以支持多实例（见 §5.2），用于 ScriptRegistry 路由匹配 |
| `sourceIp` | `realClientIP`（trustedProxies 感知 XFF） | 反向代理场景下从 XFF 提取真实 Alertmanager IP；直连场景为 RemoteAddr |
| `receivedAt` | `time.Now().UnixMilli()` | webhookd 接收时刻（不参与去重指纹） |
| `originTimestamp` | alert `startsAt` 解析为 epoch millis | 确定性：同一告警在所有实例中值相同；解析失败时回退到 `startsAt` 字符串的确定性 hash |
| `rawEvent` | 确定性渲染（见上） | 不含 receivedAt，用于去重指纹 |
| `metadata` | 见下表 | 携带 Alertmanager 结构化字段供 Groovy parser 消费 |

### metadata 字段

| metadata key | 类型 | 说明 |
|---|---|---|
| `status` | string | `"firing"` 或 `"resolved"` |
| `fingerprint` | string | Alertmanager 自带的告警指纹（标签集哈希），firing 和 resolved 相同 |
| `labels` | map[string]string | 告警标签（alertname, instance, severity, job 等） |
| `annotations` | map[string]string | 告警注释（summary, description 等） |
| `startsAt` | string (RFC3339) | 告警触发时间 |
| `endsAt` | string (RFC3339) | 告警结束时间（resolved 时有值） |
| `generatorURL` | string | Prometheus 表达式链接 |
| `groupKey` | string | Alertmanager 分组键 |
| `receiver` | string | Alertmanager receiver 名称 |
| `externalURL` | string | Alertmanager 外部 URL |
| `commonLabels` | map[string]string | 分组内公共标签 |
| `groupLabels` | map[string]string | 分组标签 |
| `domainId` | string | 可选，从 webhookd 配置注入（默认 `"default"`） |

---

## 5. CEP Engine 接入

### 5.1 事件接入路径

```
webhookd → POST /api/v1/events/batch → EventIngestionController
  → TransportDeduplicator (source + sourceIp + originTimestamp + rawEvent 指纹去重)
  → EventProcessingChain.process()
    → ScriptRegistry.matchScript(rawJson)  // 按 source 匹配对应的 alertmanager parser
    → alertmanager_parser.groovy 执行
    → 返回 AlarmEvent (agentType 由 parser 设置)
    → severity_grade / maintain_check / flash_detect / problem_resolution hooks
    → tryDedup (domain 级去重)
    → MongoBatchWriter 持久化
```

### 5.2 ScriptRegistry 路由匹配与多实例支持

#### 现有机制

`ScriptRegistry.matchScript()` 解析 RawEvent JSON 后，遍历所有已注册 parser 的 match rules，选择 **weight 最高**（匹配规则最多）的 parser。匹配规则通过 `ScriptMatchRule` 定义，支持 `equal` 和 `contain` 两种 operator，可匹配任意 JSON 路径（如 `source`、`metadata.trapOid`）。

`extractOidMatchRules()` 方法对无 trapMap 的通用 parser，按文件名自动生成 source 匹配规则：

```java
// 现有逻辑（ScriptRegistry.java:269-276）
if (rules.isEmpty()) {
    String name = file.getName();
    if (name.contains("syslog")) {
        rules.add(sourceRule("syslog"));      // OP_EQUAL: source == "syslog"
    } else if (name.contains("snmp_trap")) {
        rules.add(sourceRule("snmp_trap"));   // OP_EQUAL: source == "snmp_trap"
    }
}
```

`sourceRule()` 使用 `OP_EQUAL`，即**精确匹配** source 字段。

#### 多实例需求

用户可能接入不同来源的 Alertmanager（如生产环境、预发环境、不同租户），每个来源需要独立的 webhookd 实例和定制 parser。例如：

| webhookd 实例 | config.source | 对应 groovy parser | agentType |
|---|---|---|---|
| 默认 | `"alertmanager"` | `alertmanager_parser.groovy` | `"alertmanager"` |
| 生产 | `"alertmanager_prod"` | `alertmanager_prod_parser.groovy` | `"alertmanager_prod"` |
| 预发 | `"alertmanager_staging"` | `alertmanager_staging_parser.groovy` | `"alertmanager_staging"` |

#### 推荐方案：文件名 → source 精确匹配

修改 `extractOidMatchRules()`，对以 `alertmanager` 开头的文件名，从文件名中提取完整 source 值：

```java
// 修改后
if (rules.isEmpty()) {
    String name = file.getName();
    if (name.contains("syslog")) {
        rules.add(sourceRule("syslog"));
    } else if (name.contains("snmp_trap")) {
        rules.add(sourceRule("snmp_trap"));
    } else if (name.startsWith("alertmanager")) {
        // 从文件名提取 source：
        //   alertmanager_parser.groovy          → "alertmanager"
        //   alertmanager_prod_parser.groovy     → "alertmanager_prod"
        //   alertmanager_staging_parser.groovy → "alertmanager_staging"
        String scriptName = name.replace(".groovy", "");
        String source = scriptName.replace("_parser", "");
        rules.add(sourceRule(source));
    }
}
```

**匹配流程**：

1. webhookd 配置 `source: "alertmanager_prod"`，RawEvent 的 `source` 字段为 `"alertmanager_prod"`
2. `ScriptRegistry.matchScript()` 遍历所有 parser，`alertmanager_prod_parser.groovy` 的 rule (`source equal "alertmanager_prod"`) 匹配，weight=1
3. `alertmanager_parser.groovy` 的 rule (`source equal "alertmanager"`) 不匹配，weight=0
4. weight 最高的 `alertmanager_prod_parser.groovy` 被选中

> 由于使用 `OP_EQUAL` 精确匹配，不同 source 值不会交叉匹配。每个 source 精确对应一个 parser 文件。

**约束**：
- webhookd 的 `config.source` 值必须与 groovy 文件名匹配（`<source>_parser.groovy`）
- 默认 source 为 `"alertmanager"`，对应默认 parser `alertmanager_parser.groovy`
- 若只需一个通用 parser，保持默认 `"alertmanager"` 即可

### 5.3 Problem/Resolution 自动恢复

Alertmanager 的告警生命周期天然映射到 CEP 的 Problem/Resolution 模型：

| Alertmanager status | CEP eventType | 说明 |
|---|---|---|
| `firing` | `PROBLEM` (code="1") | 告警触发 |
| `resolved` | `RESOLUTION` (code="2") | 告警恢复 |

#### pairKey 设计：使用 Alertmanager 原生 fingerprint

`problem_resolution` hook 通过 `pairKey` 匹配 RESOLUTION 与 PROBLEM。pairKey 在 parser 中构建，hook 中的构建逻辑为：

```groovy
// problem_resolution.groovy (现有逻辑，不可改动)
def segments = [event.getDomainId(), agentType, event.getNode(),
                event.getAlertGroup(), event.getAlertKey()]
def pairKey = segments.findAll { !it?.toString()?.trim()?.isEmpty() }
        .collect { it.toString().trim() }.join("|")
def problemIdentifier = pairKey + "|" + EventType.PROBLEM.code
```

**原设计问题**：原方案使用 `alertKey = alertname:instance` 作为 pairKey 的一部分。这是人工构造的，存在风险——如果两条告警的 alertname 和 instance 相同但其他 labels 不同（如 severity 不同），它们会错误配对。

**改进方案**：直接使用 Alertmanager 自带的 `fingerprint` 字段作为 `alertKey`。Alertmanager 的 `fingerprint` 是对完整标签集的哈希，保证：
- 同一告警的 firing 和 resolved 通知的 fingerprint **相同**（仅 status/endsAt 变化，labels 不变）
- 不同标签集的告警 fingerprint **不同**
- 不依赖人工选定的字段组合，由 Alertmanager 自身的配对机制保证准确性

pairKey 构成：

| 段 | AlarmEvent 字段 | alertmanager parser 取值 | 在 pairKey 中的作用 |
|---|---|---|---|
| domainId | `domainId` | metadata.domainId 或 "default" | 域隔离 |
| agentType | `agentType` | 与 source 一致（如 "alertmanager"） | 接口隔离 |
| node | `node` | labels.instance | 展示用途（fingerprint 已含 instance，冗余但无害） |
| alertGroup | `alertGroup` | labels.alertname | 展示用途（fingerprint 已含 alertname） |
| **alertKey** | **alertKey** | **metadata.fingerprint** | **核心配对字段** |

identifier 构建：

```
pairKey  = domainId | agentType | node | alertGroup | fingerprint
identifier = pairKey | eventType
```

- Firing 事件：`default|alertmanager|node1|HighCPU|e3b0c44298fc1c14|1`
- Resolved 事件：`default|alertmanager|node1|HighCPU|e3b0c44298fc1c14|2`

`problem_resolution` hook 用 `pairKey + "|1"` 在 active store 中找到 Problem，匹配后自动清除。

> Alertmanager 的 `fingerprint` 是 Go `model.Labels.Fingerprint()` 计算的 64 位哈希，序列化为十六进制字符串（如 `"e3b0c44298fc1c14"`）。它在 alert payload 中始终存在，firing 和 resolved 相同。**不需要我们在 Groovy 侧重新计算，直接取用即可。**

> `node` 和 `alertGroup` 虽然包含在 pairKey 中，但因为它们也是从 labels 派生的，而 fingerprint 已经编码了完整 labels，所以同一告警的 firing/resolved 的 node 和 alertGroup 也必然相同。它们在 pairKey 中是冗余的，但为 CEP UI 展示提供上下文，且不影响正确性。

---

## 6. parser.groovy 设计

文件位置：`cep-engine/conf/groovy/formal/alertmanager_parser.groovy`

### 6.1 设计要点

| AlarmEvent 字段 | 取值逻辑 |
|---|---|
| `domainId` | `metadata.domainId` 或 `"default"` |
| `agentType` | 与 RawEvent `source` 一致（默认 `"alertmanager"`，多实例时可配置） |
| `node` | `labels.instance`（回退到 `labels.node`，再回退到 `sourceIp`） |
| `nodeAlias` | `labels.instance` |
| `alertGroup` | `labels.alertname`（回退到 `"alertmanager"`） |
| `alertKey` | **`metadata.fingerprint`**（Alertmanager 原生指纹，firing/resolved 相同） |
| `summary` | `annotations.summary`（回退到 `alertname`） |
| `severity` | `labels.severity` 映射到 CEP 0-5 |
| `eventType` | `status=="resolved"` → `RESOLUTION`；否则 `PROBLEM` |
| `firstOccurrence` | `startsAt` 解析为 epoch millis |
| `lastOccurrence` | `startsAt` 解析为 epoch millis |
| `eventClass` | `"alertmanager"` |
| `vendor` | `"prometheus"` |
| `rawEvent` | `rawEvent.getRawEvent()` 原文 |
| `identifier` | `pairKey + "\|" + eventType` |

> **`alertKey = fingerprint` 的关键优势**：Alertmanager 的 `fingerprint` 是对完整标签集的哈希，天然保证同一告警在 firing 和 resolved 状态下值相同。无需人工选定 `alertname:instance` 这样的字段组合——后者可能在某些告警规则下缺失 instance 标签或在多维标签下不够唯一。`fingerprint` 始终由 Alertmanager 计算、始终存在、始终唯一。

### 6.2 Severity 映射

Alertmanager 的 `labels.severity` 是用户自定义字符串，常见值为 `critical` / `warning` / `info`。映射到 CEP Severity 枚举（0-5）：

| Alertmanager severity | CEP Severity (level) | 枚举 |
|---|---|---|
| `critical` | 5 | CRITICAL |
| `major` | 4 | MAJOR |
| `warning` / `warn` | 3 | MINOR |
| `info` | 2 | WARNING |
| `debug` / `trace` | 1 | INDETERMINATE |
| `none` / `ok` / `resolved` | 0 | CLEAR |
| 其他/缺失 | 2 | WARNING（默认） |

### 6.3 Groovy 脚本结构

```groovy
/**
 * alertmanager_parser.groovy
 *
 * 消费 prometheus-webhook 构建的 RawEvent（source = "alertmanager"）。
 * 将 Alertmanager 告警映射到 AlarmEvent，使用 Alertmanager 原生
 * fingerprint 构建 pairKey，实现 Problem/Resolution 自动恢复。
 *
 * Input variables:
 *   rawEvent  - RawEvent (source, sourceIp, rawEvent, metadata)
 *   rawJson   - 序列化 JSON
 *   gson      - Gson 实例
 *
 * Metadata fields:
 *   status (string), fingerprint (string), labels (map), annotations (map),
 *   startsAt (RFC3339), endsAt (RFC3339), generatorURL,
 *   groupKey, receiver, externalURL, commonLabels, groupLabels, domainId
 *
 * agentType = source 值（默认 "alertmanager"，多实例时与 source 一致）
 */

import com.dujitech.cep.model.AlarmEvent
import com.dujitech.cep.model.EventType

def event = new AlarmEvent()
def metadata = rawEvent.getMetadata() ?: [:]

// Domain
event.setDomainId(metadata.get("domainId")?.toString() ?: "default")

// agentType: 与 source 一致，多实例时可区分不同 Alertmanager 来源
String agentType = rawEvent.getSource()?.toString() ?: "alertmanager"

// labels 和 annotations
def labels = metadata.get("labels") ?: [:]
def annotations = metadata.get("annotations") ?: [:]

// Node: labels.instance > labels.node > sourceIp
String instance = labels.get("instance")?.toString() ?: ""
String nodeLabel = labels.get("node")?.toString() ?: ""
String sourceIp = rawEvent.getSourceIp()?.toString()?.trim() ?: ""
String node = instance ?: nodeLabel ?: sourceIp

// alertGroup: labels.alertname (展示用途)
String alertname = labels.get("alertname")?.toString() ?: "alertmanager"

// alertKey: 使用 Alertmanager 原生 fingerprint
// fingerprint 是对完整标签集的哈希，firing 和 resolved 相同，
// 是 problem_resolution hook 配对的最可靠依据。
String fingerprint = metadata.get("fingerprint")?.toString()?.trim() ?: ""
String alertKey = fingerprint ?: "${alertname}:${instance ?: sourceIp}"

// Summary
String summary = annotations.get("summary")?.toString()?.trim() ?: alertname

// Severity mapping
int sev = mapSeverity(labels.get("severity")?.toString())
event.setSeverity(sev)
event.setOriginalSeverity(sev)

// EventType: resolved -> RESOLUTION, else PROBLEM
String status = metadata.get("status")?.toString() ?: "firing"
String eventType = (status == "resolved") ? EventType.RESOLUTION.code : EventType.PROBLEM.code
event.setEventType(eventType)

// Timestamps from startsAt
long ts = parseMillis(metadata.get("startsAt")?.toString())
if (ts <= 0) ts = rawEvent.getOriginTimestamp() ?: System.currentTimeMillis()
event.setFirstOccurrence(ts)
event.setLastOccurrence(ts)

// Set fields
event.setNode(node)
event.setNodeAlias(instance)
event.setAgentType(agentType)
event.setAlertGroup(alertname)
event.setAlertKey(alertKey)
event.setSummary(summary)
event.setVendor("prometheus")
event.setEventClass("alertmanager")

// RawEvent (保留确定性渲染原文)
String rawText = rawEvent.getRawEvent()?.toString() ?: ""
event.setRawEvent(rawText)

// Build pairKey and identifier
// pairKey = domainId|agentType|node|alertGroup|alertKey(fingerprint)
// identifier = pairKey|eventType
// 同一告警的 firing (eventType=1) 和 resolved (eventType=2) 的 pairKey 相同，
// problem_resolution hook 据此自动配对恢复。
def pairKey = [event.getDomainId(), event.getAgentType(), event.getNode(),
               event.getAlertGroup(), event.getAlertKey()]
        .findAll { it != null && !it.toString().trim().isEmpty() }
        .collect { it.toString().trim() }.join("|")
event.setIdentifier(pairKey + "|" + event.getEventType())

return event

// --- Helpers ---

int mapSeverity(String sev) {
    if (sev == null) return 2
    switch (sev.toLowerCase()) {
        case "critical": return 5
        case "major":    return 4
        case "warning":  return 3
        case "warn":     return 3
        case "info":     return 2
        case "debug":    return 1
        case "trace":    return 1
        case "none":     return 0
        case "ok":       return 0
        case "resolved": return 0
        default:         return 2
    }
}

long parseMillis(String s) {
    if (s == null) return 0
    try {
        String norm = s.replace(" ", "T")
        java.time.OffsetDateTime odt = java.time.OffsetDateTime.parse(norm)
        return odt.toInstant().toEpochMilli()
    } catch (Exception ignored) {
        try { return Long.parseLong(s) } catch (Exception ignored2) { return 0 }
    }
}
```

---

## 7. 项目结构

```
prometheus-webhook/
├── cmd/
│   └── webhookd/
│       └── main.go                  # 入口：HTTP server + 装配 + 优雅退出
├── internal/
│   ├── config/
│   │   └── config.go                # YAML + env 配置
│   ├── webhook/
│   │   ├── server.go                # HTTP server + 路由 + 请求处理
│   │   ├── parser.go                # Alertmanager payload 解析 + alert 拆分
│   │   └── server_test.go           # parser + handler 单元测试
│   ├── model/
│   │   ├── rawevent.go              # RawEvent 构建（source="alertmanager"）
│   │   └── rawevent_test.go
│   ├── forward/
│   │   ├── batch_queue.go            # 批量队列 + 背压（复用 syslog-daemon 模式）
│   │   ├── forwarder.go             # Forwarder 接口 + ForwardConfig
│   │   ├── http_forwarder.go        # HTTP 转发 cep-engine（重试 + 指数退避）
│   │   └── *_test.go
│   └── metrics/
│       ├── metrics.go               # Prometheus 自监控
│       └── metrics_test.go
├── config.example.yaml
├── deploy/
│   └── prometheus-webhook.service   # systemd unit
├── .github/workflows/ci.yml         # CI: vet + test + lint + build
├── .golangci.yml
├── .gitignore
├── Makefile
├── go.mod
├── LICENSE
├── README.md
└── DEPLOY.md
```

### 包职责

| 包 | 职责 |
|---|---|
| `cmd/webhookd` | 程序入口：加载配置、装配 pipeline、HTTP server 生命周期、优雅退出、日志轮转 |
| `internal/config` | YAML 配置加载 + 默认值 + 环境变量覆盖 + 校验 |
| `internal/webhook` | HTTP server（监听 + 路由）、Alertmanager JSON payload 解析、alert 拆分 |
| `internal/model` | RawEvent 结构体定义 + `NewFromAlert()` 构建函数（对齐 cep-engine Gson 契约） |
| `internal/forward` | Forwarder 接口、BatchQueue（有界队列 + 背压）、HTTPForwarder（重试 + 指数退避） |
| `internal/metrics` | Prometheus 指标（received/forwarded/failed/dropped/queue_depth/throughput） |

### 复用策略

`forward` 和 `metrics` 包的逻辑与 syslog-daemon 基本一致（BatchQueue、HTTPForwarder、Forwarder 接口、Metrics 结构），仅以下差异：

- Metrics 指标前缀从 `syslog_` / `trap_` 改为 `alertmanager_`
- `model.RawEvent` 的 `Metadata` 类型为 `map[string]interface{}`（与 syslog-daemon 一致，非 trap-daemon 的强类型 `Metadata` struct）
- `model.NewFromAlert()` 替代 `NewFromSyslog()` / `NewFromTrapData()`

---

## 8. 配置设计

`config.example.yaml`：

```yaml
# ============================================================
# prometheus-webhook 配置文件示例
# 复制为 config.yaml 并修改后使用。
# 所有路径均支持环境变量覆盖（前缀 AMWH_*）。
# ============================================================

# ---- Webhook HTTP Server ----
webhook:
  listenAddr: "0.0.0.0:9093"     # HTTP 监听地址（/webhook + /metrics + /healthz 共用）
  path: "/webhook"               # Alertmanager webhook 接收路径
  source: "alertmanager"         # RawEvent.source 值，也用于 ScriptRegistry 路由匹配
                                   # 多实例时改为唯一值如 "alertmanager_prod"
                                   # 对应 cep-engine 的 alertmanager_prod_parser.groovy
  maxBodyBytes: 1048576          # 请求体大小上限（1 MiB）
  allowedCIDRs: []               # Alertmanager 来源 IP/CIDR 白名单；空则仅靠 token 防护
                                   # 反向代理场景：配 AM 网段，trustedProxies 配代理 IP
                                   # 示例: ["10.0.0.0/8", "192.168.1.100/32"]
  trustedProxies: []             # 信任的反向代理 IP/CIDR；空=不信任任何 XFF，直连模式
                                   # 反向代理场景必填，如: ["127.0.0.1/32", "10.0.0.2/32"]
  authToken: ""                 # Bearer token，必须显式配置（空=拒绝所有请求）
                                   # 生成: openssl rand -hex 32
  metricsPath: "/metrics"        # Prometheus 指标暴露路径
  healthPath: "/healthz"        # 健康检查路径

# ---- TLS (可选，零信任环境) ----
tls:
  enabled: false
  certFile: ""                   # 服务端证书文件路径
  keyFile: ""                    # 服务端私钥文件路径
  clientCAFile: ""               # 客户端 CA 证书（启用 mTLS 校验）

# ---- Downstream cep-engine ----
cepEngine:
  baseUrl: "http://127.0.0.1:8080"
  batchPath: "/api/v1/events/batch"
  singlePath: "/api/v1/events"
  authToken: ""                  # 可选 Bearer token
  timeoutMs: 5000
  retryMax: 3
  retryBaseMs: 200

# ---- Forwarding queue ----
forward:
  batchSize: 50                  # 攒批条数阈值
  batchFlushIntervalMs: 200      # 攒批时间阈值（毫秒）
  workers: 4                     # worker pool 并发数
  queueCapacity: 10000           # 有界队列容量
  queueFullPolicy: "drop"       # drop | block | single
  dropLogEnabled: true

# ---- Logging ----
logging:
  level: "info"                  # debug | info | warn | error
  file: ""                       # 空 -> stdout
  maxSizeMB: 100
  maxBackups: 5
```

### 环境变量

| 环境变量 | 对应配置项 |
|---|---|
| `AMWH_WEBHOOK_LISTENADDR` | `webhook.listenAddr` |
| `AMWH_WEBHOOK_PATH` | `webhook.path` |
| `AMWH_WEBHOOK_SOURCE` | `webhook.source` |
| `AMWH_WEBHOOK_ALLOWED_CIDRS` | `webhook.allowedCIDRs`（逗号分隔） |
| `AMWH_WEBHOOK_TRUSTED_PROXIES` | `webhook.trustedProxies`（逗号分隔） |
| `AMWH_WEBHOOK_AUTH_TOKEN` | `webhook.authToken` |
| `AMWH_TLS_ENABLED` | `tls.enabled` |
| `AMWH_TLS_CERT_FILE` | `tls.certFile` |
| `AMWH_TLS_KEY_FILE` | `tls.keyFile` |
| `AMWH_TLS_CLIENT_CA_FILE` | `tls.clientCAFile` |
| `AMWH_CEPENGINE_BASEURL` | `cepEngine.baseUrl` |
| `AMWH_CEPENGINE_AUTHTOKEN` | `cepEngine.authToken` |
| `AMWH_LOGGING_LEVEL` | `logging.level` |
| `AMWH_LOGGING_FILE` | `logging.file` |

---

## 9. Metrics 指标

webhookd 在同一 HTTP server 上暴露 `/metrics` 端点（Prometheus exposition format），供 Prometheus scrape。无需额外端口或组件。

| 指标名 | 类型 | 标签 | 说明 |
|---|---|---|---|
| `alertmanager_received_total` | counter | `source` | 累计接收的 alert 数量 |
| `alertmanager_forwarded_total` | counter | - | 转发成功数 |
| `alertmanager_forward_failed_total` | counter | - | 转发失败数 |
| `alertmanager_dropped_total` | counter | `reason` | 丢弃数（队列满/解析失败等） |
| `alertmanager_webhook_requests_total` | counter | `status` | 累计 webhook HTTP 请求数（按响应状态码） |
| `alertmanager_auth_rejected_total` | counter | `reason`(ip/token) | 被安全层拒绝的请求数（401/403） |
| `alertmanager_alerts_by_status_total` | counter | `status`(firing/resolved) | 按 firing/resolved 统计的 alert 数 |
| `alertmanager_queue_depth` | gauge | - | 当前队列深度 |
| `alertmanagerd_start_time_seconds` | gauge | - | 进程启动时间 |
| `alertmanager_throughput_5m` | gauge | - | last 5min 吞吐量（条/s） |
| `alertmanager_http_request_duration_seconds` | histogram | `path` | HTTP 请求延迟分布 |

### Prometheus scrape 配置示例

```yaml
scrape_configs:
  - job_name: 'prometheus-webhook'
    static_configs:
      - targets: ['prometheus-webhook:9093']
    metrics_path: /metrics
```

---

## 10. Alertmanager 对接配置

Alertmanager 的 `alertmanager.yml` 配置示例：

```yaml
route:
  receiver: cep-webhook
  group_by: ["alertname", "instance"]
  group_wait: 10s
  group_interval: 30s
  repeat_interval: 1h
  routes:
    - matchers: ["severity = critical"]
      receiver: cep-webhook
      group_wait: 5s

receivers:
  - name: cep-webhook
    webhook_configs:
      - url: "http://prometheus-webhook:9093/webhook"
        send_resolved: true    # 关键：发送 resolved 通知以触发自动恢复
        max_alerts: 0          # 不限制单次推送数量
        http_config:
          authorization:
            type: Bearer
            credentials: "<AMWH_WEBHOOK_AUTH_TOKEN>"  # 与 webhookd 的 authToken 一致
          # 可选 mTLS：
          # tls_config:
          #   ca_file: /etc/alertmanager/certs/ca.pem
          #   cert_file: /etc/alertmanager/certs/client.pem
          #   key_file: /etc/alertmanager/certs/client.key
          #   server_name: prometheus-webhook.default.svc.cluster.local
```

**关键配置项**：
- `send_resolved: true` 必须开启，否则 Alertmanager 不会发送 resolved 通知，CEP 无法实现自动恢复
- `http_config.authorization` 中的 `credentials` 必须与 webhookd 的 `webhook.authToken` 完全一致
- token 建议通过 Alertmanager 的 `--config.file` 结合环境变量模板注入，不硬编码到配置文件中

---

## 11. 与现有项目的差异对比

| 维度 | trap-daemon | syslog-daemon | prometheus-webhook |
|---|---|---|---|
| 入站协议 | UDP 162 (SNMP) | UDP 514 (Syslog) | HTTP POST 9093 |
| 入站模式 | 被动监听 | 被动监听 | HTTP Server |
| 请求来源 | 网络设备 | 网络设备/主机 | Alertmanager |
| 数据格式 | BER 编码 SNMP PDU | RFC3164/5424 文本 | JSON |
| 解析复杂度 | OID→字段名映射 | 正则/结构化解析 | JSON unmarshal |
| 单次请求 | 1 trap = 1 event | 1 syslog = 1 event | 1 webhook = N alerts (数组) |
| originTimestamp 来源 | sysUpTime 或 hash | syslog header 时间戳或 hash | alert.startsAt |
| source 值 | `snmp_trap`（常量） | `syslog`（常量） | **可配置**（默认 `alertmanager`） |
| agentType | `generic`/厂商 | `syslog`（常量） | **与 source 一致**（多实例可区分） |
| Metrics 端口 | 独立 9094 | 独立 9094 | **与 webhook 共用 9093**（`/metrics` 路径） |
| alertKey 构建 | OID + trapMap | appName + message | **Alertmanager 原生 fingerprint** |
| 入站安全 | 无（UDP 无认证） | 无（UDP 无认证） | **Bearer Token + IP 白名单 + 可选 mTLS** |
| external 依赖 | gosnmp | 无（标准库） | 无（标准库 net/http） |

---

## 12. 修改清单

### 新建：prometheus-webhook 项目（工作区 `D:/63.CEP/prometheus-webhook/`）

1. Go 模块初始化 + go.mod
2. `cmd/webhookd/main.go` — HTTP server 入口（/webhook + /metrics + /healthz）
3. `internal/config/config.go` — 配置加载（含 `source` 可配置项）
4. `internal/webhook/server.go` — HTTP server + handler（三端点路由 + Bearer Token 校验 + IP 白名单 + trustedProxies XFF 提取 + mTLS）
5. `internal/webhook/parser.go` — Alertmanager payload 解析
6. `internal/model/rawevent.go` — RawEvent 构建（source 从配置注入，sourceIp 经 XFF 提取，rawEvent 确定性渲染含 fingerprint）
7. `internal/forward/` — 批量队列 + HTTP 转发（移植自 syslog-daemon，调整指标前缀）
8. `internal/metrics/metrics.go` — Prometheus 自监控（/metrics 端点注册 + auth_rejected 指标）
9. `config.example.yaml` — 配置示例（含 source 字段）
10. `Makefile` — 构建脚本
11. `.github/workflows/ci.yml` — CI 流水线
12. `.golangci.yml` — lint 规则
13. `deploy/prometheus-webhook.service` — systemd unit
14. `README.md` / `DEPLOY.md` — 文档
15. 单元测试（parser、rawevent 确定性渲染、去重指纹一致性）

### 修改：cep-engine（`D:/63.CEP/cep-engine/`）

1. 新增 `conf/groovy/formal/alertmanager_parser.groovy` — 默认 Groovy parser（alertKey = fingerprint）
2. 修改 `src/main/java/com/dujitech/cep/groovy/ScriptRegistry.java` — `extractOidMatchRules()` 新增 alertmanager 分支：
   - 从文件名提取完整 source 值（`alertmanager_parser.groovy` → `"alertmanager"`，`alertmanager_prod_parser.groovy` → `"alertmanager_prod"`）
   - 约 5 行替换（含注释）

---

## 13. 不在本次范围

- Alertmanager 规则配置（由用户在 Alertmanager 侧配置）
- Prometheus 告警规则设计
- cep-web 前端的适配（如需在 Web UI 显示 alertmanager 类型事件，后续可扩展）
- TLS/HTTPS 支持（如需加密传输，建议在反向代理层处理）
