# prometheus-webhook

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.22-00ADD8.svg?logo=go&logoColor=white)](go.mod)
[![Release](https://img.shields.io/github/v/release/chenke-duji/prometheus-webhook)](https://github.com/chenke-duji/prometheus-webhook/releases)

> Alertmanager Webhook Receiver for CEP (Complex Event Processing) Engine

> **Bearer + CIDR + mTLS 三层安全 · firing/resolved 自动配对 · >40,000 alert/s 接收 · Prometheus 自监控**

`prometheus-webhook` 是一个 HTTP webhook 守护进程，接收 Prometheus Alertmanager 推送的告警通知，将每条告警拆分为独立的 RawEvent，批量转发给 cep-engine。与 `trap-daemon`（SNMP Trap）、`syslog-daemon`（Syslog）同级，共享相同的下游管道。

```mermaid
flowchart LR
    A[Prometheus Alertmanager] -->|POST /webhook<br/>Bearer/CIDR/mTLS| B[prometheus-webhook]
    B -->|拆分 alerts[]| C{有界队列<br/>攒批}
    C -->|REST 批量转发| D[cep-engine]
    D --> E[(MongoDB)]
    B -.->|自监控| F[/metrics/]
```

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

## 性能（压测）

> 压测环境：WSL AlmaLinux-8（16 核 / 15.7G），后端 cep-engine `-Xmx4g -XX:+UseG1GC`，
> MongoDB 6.0 单机（端口 27015）。压测器：`perf-test/loadgen` 的 `webhookload`
> （模拟 Alertmanager 带 Bearer token 的 POST /webhook，随机 fingerprint/instance/startsAt，
> 每请求 100 条告警，8 worker，令牌桶精确控速）。

### 梯度压测结果

| 目标速率(alert/s) | 实际吞吐(alert/s) | 接收(20s) | 转发(20s) | 丢弃(20s) | 丢弃率 | P50 | P99 |
|------------------|------------------|----------|----------|----------|--------|-----|-----|
| 5000 | 5249 | 105002 | 104900 | 0 | 0.0% | 332µs | 8.7ms |
| 10000 | 10499 | 210000 | 209850 | 234 | 0.1% | 528µs | 8.5ms |
| 15000 | 15748 | 314996 | 214850 | 90164 | 28.6% | 723µs | 9.2ms |
| 20000 | 20991 | 419985 | 205200 | 215072 | 51.2% | 798µs | 9.4ms |
| 30000 | 31494 | 630025 | 210750 | 418538 | 66.4% | 987µs | 9.8ms |
| 40000 | 41990 | 840009 | 188250 | 651259 | 77.5% | 1.07ms | 10.7ms |

### 结论

- **接收能力 > 40000 alert/s**：HTTP 接收 + 拆分 + 入队无压力（实测 41990/s 仍达目标，
  受限于压测器单进程发送能力）。
- **转发瓶颈约 10000–10500 alert/s**：受 cep-engine 单机写入速度限制（与 trap/syslog-daemon
  同量级）。≤10000/s 时丢弃率 <0.1%；超过后队列满按 `drop` 策略丢弃。
- **内存占用极低**：RSS 仅 ~21MB。
- **延迟优秀**：P50 亚毫秒级，P99 < 11ms。

### 调优建议

高吞吐（>10000/s）场景，参照 syslog-daemon 的调优路径：

- `forward.workers` 4→8、`batchSize` 50→100、`queueCapacity` 10000→50000，降低丢弃率；
- 后端 cep-engine 水平扩展（多实例 + nginx 负载均衡）是解除转发瓶颈的根本手段。

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
