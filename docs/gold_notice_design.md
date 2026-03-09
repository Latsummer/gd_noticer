# 黄金报价通知服务落地设计文档

## 1. 文档信息

- **项目名称**: 黄金报价通知服务（个人自用）
- **目标**: 在指定时间窗口内按固定间隔拉取黄金报价，并在满足策略条件时通过 Bark 推送通知
- **适用范围**: 单用户、单进程、轻量部署
- **不在范围**: 多用户系统、复杂规则引擎、历史分析平台（第3阶段）

---

## 2. 需求摘要

### 2.1 核心需求

- 从配置读取起始时间、结束时间、轮询间隔
- 定时调用黄金报价 API 获取数据
- 根据策略判定是否发送通知
- 通过 Bark API 向苹果设备推送消息

### 2.2 已知外部接口

#### 黄金报价 API

- **Method**: `GET`
- **URL**: `https://sapi.k780.com/?app=finance.gold_price&goldid=1051&appkey=APPKEY&sign=SIGN&format=json`
- **成功返回**: `success = "1"`，结果在 `result.dtList` 中
- **失败返回**: `success = "0"`，包含 `msgid`、`msg`

#### Bark 推送 API

- **Method**: `POST`
- **URL**: `https://api.day.app/push`
- **Content-Type**: `application/json; charset=utf-8`
- **核心字段**:
  - `title`
  - `body`
  - `group`
  - `device_key`

---

## 3. 设计目标与原则

### 3.1 设计目标

- **稳定**: 异常可重试，失败可观测
- **低打扰**: 避免重复推送
- **简单可维护**: 无数据库依赖，本地状态可恢复
- **可扩展**: 后续可平滑增加更多品种或策略

### 3.2 关键原则

- 配置可校验，错误尽早失败
- 请求必须有超时与重试
- 任务执行必须串行，避免并发重入
- 状态持久化优先保证“去重能力”

---

## 4. 功能范围（第1、2阶段）

### 4.1 必做（MVP）

1. 定时调度与时间窗口控制
2. 黄金报价拉取与解析
3. 去重与降噪策略
4. Bark 推送
5. 错误处理、重试、日志
6. 配置加载与启动校验

### 4.2 增强（建议）

1. 本地状态持久化（`state.json`）
2. 连续失败告警（达到阈值后单独通知）
3. 健康检查接口（`/healthz`）
4. 手动触发接口（`/trigger`，建议仅本机）

---

## 5. 系统架构

单进程模块化架构：

1. `config`：加载并校验配置
2. `scheduler`：定时触发任务，保证同一时刻仅一个任务执行
3. `gold_client`：调用黄金 API，完成响应解析与错误归类
4. `strategy`：根据最新行情和历史状态判断是否通知
5. `notifier_bark`：发送 Bark 消息
6. `state_store`：读写本地状态文件
7. `health`：暴露运行健康状态

---

## 6. 配置设计

建议使用 `config.yaml` + 环境变量覆盖敏感字段。

### 6.1 配置项清单

```yaml
service:
  timezone: "Asia/Shanghai"
  poll_interval_seconds: 60
  window_start: "09:00:00"
  window_end: "23:30:00"

gold_api:
  base_url: "https://sapi.k780.com/"
  gold_id: "1051"
  app_key: "${GOLD_APP_KEY}"
  sign: "${GOLD_SIGN}"

notify:
  bark_endpoint: "https://api.day.app/push"
  device_key: "${BARK_DEVICE_KEY}"
  group: "gold-watch"
  title_prefix: "黄金提醒"

strategy:
  dedup_by_uptime: true
  min_change_percent: 0
  max_silent_minutes: 120

reliability:
  http_timeout_seconds: 10
  max_retries: 3
  retry_backoff_seconds: 2
  failure_alert_threshold: 3

state:
  file_path: "./data/state.json"

http_server:
  enable: true
  listen_addr: "127.0.0.1:18080"
```

### 6.2 配置校验规则

- `poll_interval_seconds >= 30`
- `window_start`、`window_end` 格式必须是 `HH:MM:SS`
- `app_key`、`sign`、`device_key` 不能为空
- `http_timeout_seconds`、`max_retries` 必须为正整数
- `max_silent_minutes >= 0`

---

## 7. 数据模型与状态持久化

### 7.1 行情数据模型（逻辑）

- `goldID`
- `variety`
- `varietyName`
- `lastPrice`
- `changePrice`
- `changeMargin`
- `uptime`

### 7.2 本地状态文件（`state.json`）

建议字段：

- `last_success_uptime`: 最近一次成功拉取的行情更新时间
- `last_success_price`: 最近一次成功拉取价格
- `last_notify_at`: 最近一次通知时间
- `last_notify_digest`: 最近一次通知摘要（用于辅助去重）
- `consecutive_failures`: 连续失败次数
- `last_error`: 最近一次错误信息
- `last_fetch_at`: 最近一次拉取时间

### 7.3 状态文件策略

- 启动时读取；文件不存在则初始化默认状态
- 每次任务结束后落盘（原子写入，防止半写入）
- 写入失败仅记录错误，不阻塞主流程

---

## 8. 核心流程设计

### 8.1 定时任务流程

1. 到达调度时间
2. 判断是否在时间窗口内
   - 若否：记录日志并跳过
3. 获取执行锁（防并发重入）
4. 调用黄金 API（含超时、重试）
5. 解析响应
   - `success != "1"`：记为失败
6. 交给策略模块判断是否通知
7. 若需通知，调用 Bark API 推送
8. 更新状态文件
9. 更新健康状态
10. 释放执行锁

### 8.2 失败处理流程

- 失败一次：`consecutive_failures + 1`
- 连续失败达到阈值（如3次）：发送“服务异常告警”通知
- 下次成功后：`consecutive_failures` 清零

---

## 9. 通知策略设计

### 9.1 默认策略（推荐）

- 主去重维度：`uptime`
- 辅助判断：价格是否变化
- 阈值策略：当 `abs(change_percent) >= min_change_percent` 才通知（若阈值为0则不过滤）
- 静默保底：超过 `max_silent_minutes` 强制发送一条行情心跳

### 9.2 通知内容模板（建议）

- `title`: `{{title_prefix}} {{varietynm}}`
- `body` 示例：
  - `现价: 403.10`
  - `涨跌: +5.97 (1.50%)`
  - `更新时间: 2022-06-13 16:58:39`

---

## 10. HTTP 接口设计（可选增强）

### 10.1 `GET /healthz`

返回示例：

```json
{
  "status": "ok",
  "last_fetch_at": "2026-03-09T10:00:00+08:00",
  "last_success_uptime": "2026-03-09 09:59:37",
  "consecutive_failures": 0,
  "in_window": true
}
```

### 10.2 `POST /trigger`

- 行为：立即执行一次完整拉取-判定-推送流程
- 安全建议：仅监听 `127.0.0.1`，或增加简单 token 校验

---

## 11. 日志与可观测性

### 11.1 日志建议字段

- `event`: `fetch` / `decide` / `notify` / `health`
- `gold_id`
- `last_price`
- `uptime`
- `should_notify`
- `consecutive_failures`
- `error`

### 11.2 日志建议

- 使用结构化日志（JSON 或 key-value）
- 按天滚动切分日志
- 避免记录密钥和完整敏感参数

---

## 12. 安全与可靠性要求

1. 严禁硬编码 `app_key`、`sign`、`device_key`
2. 所有外部请求必须设置超时
3. 重试需有上限与退避间隔
4. 仅信任 HTTPS 接口
5. 手动触发接口默认仅本机可访问

---

## 13. 验收标准

1. 在窗口内按间隔稳定拉取，窗口外不拉取
2. 同一行情不重复推送
3. API 临时失败可自动重试
4. 连续失败达到阈值触发异常通知
5. 服务重启后去重能力仍有效
6. `healthz` 能反映最近状态

---

## 14. 实施里程碑

### 里程碑 M1（1天）

- 配置模块 + 调度模块 + 黄金 API 拉取
- Bark 推送最小链路打通

### 里程碑 M2（1天）

- 策略模块（去重/阈值/静默）
- 本地状态持久化

### 里程碑 M3（0.5天）

- 连续失败告警
- `/healthz` 与 `/trigger`
- 日志字段完善与回归验证

---

## 15. 后续可扩展（预留，不实现）

- 支持多个 `gold_id`
- 支持多个通知分组
- 增加简单 Web 面板查看状态

---

## 16. 结论

该方案面向个人使用场景，优先保证稳定、低打扰和易维护：

- 不引入数据库，使用本地状态文件已满足当前需求
- 具备完整的失败恢复、去重、防噪与健康可观测能力
- 后续可在不破坏现有架构的前提下继续扩展
