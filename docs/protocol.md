# BinancePayTool 接入协议 v1

> 签名头与错误码沿用 `X-BPG-` / `BPG` 前缀，它们是协议稳定标识，与仓库名无关。

所有接口均为 HTTP + JSON（UTF-8）。金额一律为**字符串**十进制（如 `"10.0037"`），最多 8 位小数。
时间戳一律为**毫秒** Unix 时间。

## 1. 签名

商户与网关共享一个密钥 `API_AUTH_KEY`（下称 `secret`），同时用于「商户调用网关」与「网关回调商户」。

### 1.1 商户 → 网关（API 请求签名）

请求头：

| Header | 说明 |
|---|---|
| `X-BPG-Timestamp` | 当前毫秒时间戳；与服务器偏差超过 300 秒拒绝 |
| `X-BPG-Nonce` | 随机串（≥16 字符）；5 分钟内不得重复 |
| `X-BPG-Signature` | 小写十六进制签名 |

签名串（`\n` 为换行符；`path` 只含路径不含查询串与域名；GET/无 body 时 body 视为空串）：

```
string_to_sign = timestamp + "\n" + nonce + "\n" + METHOD + "\n" + path + "\n" + SHA256_HEX(body)
signature      = HMAC_SHA256_HEX(secret, string_to_sign)
```

### 1.2 网关 → 商户（回调签名）

回调为 `POST callback_url`，`Content-Type: application/json`，请求头同上三个，签名串少 METHOD 与 path：

```
string_to_sign = timestamp + "\n" + nonce + "\n" + SHA256_HEX(body)
```

商户校验：时间戳偏差 ≤300 秒、签名一致（常数时间比较）。校验通过返回 HTTP 2xx 即视为成功；
否则网关按 `0s, 15s, 60s, 5m, 15m, 1h, 3h` 重试 7 次后放弃。**回调可能重复投递，商户须按
`order_id` 幂等处理。**

## 2. 接口

### 2.1 创建订单 `POST /api/v1/orders`

请求体：

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `merchant_order_id` | string | 是 | 商户侧唯一订单号（≤64 字符）；重复返回 `ERR_DUPLICATE` |
| `account_id` | string | 否 | 收款账号 ID（多账号模式，见 §2.6）；缺省为网关 `config.env` 里的默认账号 `default` |
| `currency` | string | 是 | 币种，须在网关 `CURRENCIES` 白名单内（如 `USDT`）|
| `amount` | string | 是 | 基础金额，>0，小数位 ≤ 网关 `AMOUNT_DECIMALS` |
| `callback_url` | string | 否 | 支付结果回调地址 |
| `return_url` | string | 否 | 支付成功后收银页跳转地址（自动附加 `?order_id=&status=`）|
| `timeout` | int | 否 | 有效期秒数，120–86400，缺省用网关 `ORDER_TTL` |

响应 `data`：

| 字段 | 说明 |
|---|---|
| `order_id` | 网关订单号 |
| `account_id` | 收款账号 ID（默认账号为 `default`）|
| `merchant_order_id` | 原样返回 |
| `status` | `pending` |
| `currency` / `base_amount` / `pay_amount` | `pay_amount` 为**实际应付的唯一金额**，付款方必须严格按它支付 |
| `note_code` | 6 位备注码；付款方转账备注里带上它可加速/兜底确认（可选）|
| `pay_url` | 网关托管收银页地址，直接跳转即可 |
| `receive_uid` | 收款方币安 UID（即该订单所属账号的 UID）|
| `receive_link` / `receive_email` | 该账号配置的收款链接（二维码内容，可自建收银页：生成二维码、手机端唤起 App）与收款邮箱，未配置为空 |
| `created_at` / `expires_at` | 毫秒时间戳 |

### 2.2 查询订单 `GET /api/v1/orders/{order_id}`

响应 `data` 在 2.1 基础上增加：`actual_amount`、`paid_at`、`matched_by`（`amount`/`note`/`claim`）、
`binance_order_id`（币安 18 位订单编号）、`payer_id`、`payer_binance_id`、`counterparty_id`（付款方标识，见下）、`overpaid`（bool）。

付款方标识三个字段：`counterparty_id` 来自流水的 `counterpartyId`，每条流水都有，是**付款方的稳定标识**，做身份锚定 / 风控请用它；`payer_binance_id` 来自 `payerInfo.binanceId`，部分流水缺失，为空串；`payer_id` 是兼容字段（`binanceId` 优先、缺失时退回 `counterpartyId`，两者命名空间不同，同一付款人可能得到两个值），老接入方可继续使用。
也可用 `GET /api/v1/orders/by-merchant/{merchant_order_id}` 按商户单号查询。

### 2.3 关闭订单 `POST /api/v1/orders/{order_id}/close`

仅 `pending` 可关闭；成功后状态为 `closed`，唯一金额进入冷却。

### 2.4 收银页（免签名，公开）

- `GET /pay/{token}`：托管收银页（token 含在 `pay_url` 中）
- `GET /pay/{token}/status`：`{status, paid_at, actual_amount, redirect}`，页面轮询用
- `POST /pay/{token}/claim`：`{"binance_order_id":"币安订单编号"}`——「我已付款」回填入口，接受 6–20 位数字，按订单 5 次/分钟、按 IP 20 次/分钟限流。返回外层 `code=OK`，`data` 为 `{code, status}`：

| `data.code` | 含义 |
|---|---|
| `OK` | 订单已支付 |
| `NOT_FOUND` / `CONSUMED` / `CURRENCY` | 未找到入账 / 已用于其他核销 / 币种不符 |
| `MISMATCH` | 流水无法唯一绑定到该订单，或唯一金额属于另一订单，需联系商家核对 |
| `TIME_WINDOW` | 付款时间早于订单创建、超出到期宽限或时间异常 |
| `UNDERPAID` | 备注绑定成功但少付；订单状态不变，需联系商家核对 |
| `STATE` | 订单状态不支持回填，或已超过创建后 7 天的回填期限 |

公开回填沿用自动匹配的优先级：唯一金额精确匹配优先，其次是本订单备注码。
匹配唯一金额时允许 `SUFFIX_MODE=sub` 的正常尾数；按备注认领时实付须 ≥ `base_amount`。
无订单绑定、少付、时间异常等失败认领不会修改订单、消费流水或发送支付回调。
付款时间须在 `[created_at, expires_at + EXPIRED_GRACE)` 内；即使来自缓存也须校验。
在创建后 7 天内，可回填恢复上述时间范围内的付款；超出自动轮询宽限后的新付款不被自动认领。
无法确认归属的付款需商家核对处理，客户端不应引导付款方重复支付。

### 2.5 响应外层

```json
{"code":"OK","data":{...}}
{"code":"ERR_AUTH","message":"..."}
```

`code` 取值：`OK`、`ERR_AUTH`、`ERR_PARAM`、`ERR_DUPLICATE`、`ERR_NOT_FOUND`、`ERR_STATE`、
`ERR_RATE_LIMIT`、`ERR_ALLOC`（唯一金额耗尽）、`ERR_BINANCE`（账号接口：币安校验失败）、`ERR_INTERNAL`。

### 2.6 收款账号（多账号模式）

网关默认只用 `config.env` 里的一个币安账号收款。以下接口可**动态添加更多收款账号**（如平台型商户
让每个入驻用户用自己的币安账户收款）：每个账号提供自己的**只读** API Key/Secret 与 UID，网关按账号
分别轮询流水，订单通过 `account_id` 指定收款账号，不同账号的订单与流水严格隔离（唯一金额也按账号分配）。

| 接口 | 说明 |
|---|---|
| `POST /api/v1/accounts` | 添加账号。请求体：`api_key`、`api_secret`、`uid`（必填），`label`、`receive_link`（https 收款链接）、`receive_email`（可选）。网关会先用该 Key 试拉一条流水，失败返回 `ERR_BINANCE`（消息含币安原因，如 Key 无效 / IP 未白名单 / 地区限制）。同一 `api_key` 重复添加视为更新并重新启用 |
| `GET /api/v1/accounts/{id}` | 查询账号（`default` 可查） |
| `POST /api/v1/accounts/{id}/verify` | 立即校验该账号的 Key；失败返回 `ERR_BINANCE` |
| `POST /api/v1/accounts/{id}/disable` / `/enable` | 停用 / 启用；停用后不能再用它下单（`default` 不可停用）|

账号响应 `data`：`account_id`、`label`、`api_key_masked`、`uid`、`receive_link`、`receive_email`、
`status`（`active`/`disabled`）、`last_ok`（最近一次轮询成功时间，毫秒）、`last_err`（最近轮询错误）、`created_at`。
**永不返回** Key 与 Secret。

安全提示：Secret 以 AES-256-GCM 加密存储，密钥由 `API_AUTH_KEY` 派生——更换 `API_AUTH_KEY` 后已存账号需重新添加。
各账号的 Key 只需「允许读取」权限，IP 白名单填网关服务器出口 IP。

## 3. 回调体

```json
{
  "event": "paid",                    // paid | underpaid | expired
  "order_id": "…",
  "account_id": "default",            // 收款账号 ID
  "merchant_order_id": "…",
  "status": "paid",
  "currency": "USDT",
  "base_amount": "10",
  "pay_amount": "10.0037",
  "actual_amount": "10.0037",
  "overpaid": false,
  "matched_by": "amount",
  "binance_order_id": "452021922068888888",
  "binance_txn_id": "P_EXAMPLE1234567890",
  "payer_id": "1160000000",
  "payer_binance_id": "1160000000",  // 可能为空
  "counterparty_id": "1163000000",   // 付款方稳定标识
  "paid_at": 1788325910559,
  "timestamp": 1788325912000
}
```

`expired`：超时未支付。`underpaid` 保留用于兼容历史少付终态订单；新版本不再因自动匹配或公开回填的少付流水进入该终态，也不生成新的 `underpaid` 回调。
少付不会被累计抵扣；如已发生少付，商户应核对实际流水并人工决定处理方式，勿要求买家盲目重复付款。

## 4. 订单状态机

```
pending ──精确金额/备注码/回填命中──▶ paid
pending ──到期──▶ expired ──宽限期内精确金额到账/回填──▶ paid
pending ──商户关闭──▶ closed
```

新版本中，少付或无绑定回填保持原状态；历史 `underpaid` 订单仍需商户人工处理。
已在付款窗口内到账的流水，可在订单创建后 7 天内通过安全回填恢复为 `paid`。

## 5. 匹配规则（网关内部，供理解）

对每条**入账**流水（金额>0、币种在白名单、币安 `orderId` 未被消费过）：

1. 流水时间必须在该订单创建后、到期宽限结束前；允许币安相对网关最多 120 秒的向前时钟偏差，但不接受订单创建前的流水；
2. 金额与某订单的 `pay_amount` 完全相等且能唯一匹配 → 自动确认；
3. 否则备注含某订单 `note_code`（不区分大小写）且能唯一匹配 → 实付 ≥ 基础金额则确认，否则保留原状态；
4. 回填使用相同金额/备注绑定与时间规则，不能仅凭未使用的币安订单编号确认付款。模糊、错额或缺少订单绑定的付款需商家核对。

同一笔币安流水只能核销一个订单（以币安 `orderId` 幂等）。
收银页 token 是访问凭据，应避免泄露；真实付款的付款方字段不能单独证明网站账户持有人身份。
