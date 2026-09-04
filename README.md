# prometheus-webhook

> Alertmanager Webhook Receiver for CEP (Complex Event Processing) Engine

`prometheus-webhook` 是一个 HTTP webhook 守护进程，接收 Prometheus Alertmanager 推送的告警通知，将每条告警拆分为独立的 RawEvent，批量转发给 cep-engine。与 `trap-daemon`（SNMP Trap）、`syslog-daemon`（Syslog）同级，共享相同的下游管道。

## 核心特性

- **Alertmanager Webhook 接收**：HTTP POST `/webhook`，解析 Alertmanager JSON payload，拆分 `alerts[]` 数组
- **安全三层防御**：Bearer Token 认证（fail-closed）+ IP/CIDR 白名单 + 可选 mTLS
- **反向代理感知**：`trustedProxies` 配置 + X-Forwarded-For 真实 IP 提取
- **Active-Active 去重**：确定性 `rawEvent` 渲染，保证多实例指纹一致
- **有界队列 + 背压**：多 worker 攒批转发，支持 drop/block/single 策略
- **Prometheus 自监控**：`/metrics` 端点暴露 received/forwarded/dropped/auth_rejected 等指标
- **Alertmanager 原生 fingerprint**：`alertKey = fingerprint`，firing/resolved 自动配对恢复

## 快速开始

### 编译

```bash
# 本机编译
make build

# Windows + Linux 交叉编译
make build-all

# 输出到 release/ 目录
make release
```

### 配置

```bash
cp config.example.yaml config.yaml
# 编辑 config.yaml，至少设置 cepEngine.baseUrl 和 webhook.authToken
```

### 运行

```bash
./bin/webhookd -config config.yaml
```

### 关键配置项

| 配置项 | 说明 | 默认值 |
|---|---|---|
| `webhook.listenAddr` | HTTP 监听地址 | `0.0.0.0:9093` |
| `webhook.source` | RawEvent.source 值，用于 ScriptRegistry 路由 | `alertmanager` |
| `webhook.authToken` | Bearer token，空=拒绝所有请求 | `""` (fail-closed) |
| `webhook.allowedCIDRs` | Alertmanager IP/CIDR 白名单 | `[]` (不限) |
| `webhook.trustedProxies` | 信任的反向代理 IP，空=不信任 XFF | `[]` |
| `cepEngine.baseUrl` | cep-engine REST 地址 | 必须配置 |

## HTTP 端点

| 路径 | 方法 | 用途 |
|---|---|---|
| `/webhook` | POST | 接收 Alertmanager 告警通知 |
| `/metrics` | GET | Prometheus 格式自监控指标 |
| `/healthz` | GET | 健康检查 `{"status":"ok"}` |

## 安全模型

### 三层防御

1. **Bearer Token**（主防线）：`webhook.authToken` 为空时拒绝所有请求；使用 `subtle.ConstantTimeCompare` 防时序攻击
2. **IP/CIDR 白名单**：`webhook.allowedCIDRs` 限定 Alertmanager 来源 IP；反向代理场景通过 `trustedProxies` 从 XFF 提取真实 IP
3. **mTLS**（可选）：`tls.enabled` + `tls.clientCAFile`，适用于零信任环境

### 部署拓扑

| 拓扑 | TLS 终止 | IP 检查对象 | 说明 |
|---|---|---|---|
| 直连 | webhookd 或裸 HTTP | RemoteAddr | 最简单 |
| 反向代理 | Nginx/HAProxy | XFF 提取的 AM IP | webhookd 配 trustedProxies |
| 反代 + mTLS | 代理↔webhookd 加密 | XFF 提取的 AM IP | 最严格 |

## Metrics

| 指标 | 类型 | 说明 |
|---|---|---|
| `alertmanager_received_total` | counter | 累计接收 alert 数 |
| `alertmanager_forwarded_total` | counter | 转发成功数 |
| `alertmanager_forward_failed_total` | counter | 转发失败数 |
| `alertmanager_dropped_total` | counter | 丢弃数（队列满） |
| `alertmanager_auth_rejected_total` | counter | 安全层拒绝数 |
| `alertmanager_queue_depth` | gauge | 当前队列深度 |
| `alertmanager_throughput_5m` | gauge | 5 分钟吞吐量 |

## Alertmanager 对接

```yaml
# alertmanager.yml
receivers:
  - name: cep-webhook
    webhook_configs:
      - url: "http://prometheus-webhook:9093/webhook"
        send_resolved: true    # 关键：发送 resolved 通知以触发自动恢复
        http_config:
          authorization:
            type: Bearer
            credentials: "<AMWH_WEBHOOK_AUTH_TOKEN>"
```

## 环境变量

所有配置项支持 `AMWH_*` 环境变量覆盖，详见 `config.example.yaml` 注释。

## 项目结构

```
prometheus-webhook/
├── cmd/webhookd/main.go          # 入口
├── internal/
│   ├── config/config.go          # 配置加载
│   ├── model/rawevent.go         # RawEvent 构建
│   ├── webhook/server.go         # HTTP server + 安全层
│   ├── forward/                   # 批量队列 + HTTP 转发
│   └── metrics/metrics.go        # Prometheus 指标
├── config.example.yaml
├── deploy/prometheus-webhook.service
├── Makefile
└── DESIGN.md
```

## License

Licensed under the [Apache License, Version 2.0](LICENSE).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Contact

- Email: chenke@dujitech.cn
