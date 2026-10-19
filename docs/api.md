#  API 文档

后端直连：http://127.0.0.1:8080
开发代理：http://127.0.0.1:5173/api/v1/...
Nginx 代理：http://127.0.0.1:8088/api/v1/...

/health 当前仅说明后端直连地址，Nginx 未单独代理该路径。
文档中的统一 JSON 错误结构由 Gin 返回；
Nginx 自身产生的错误可能是 HTML，例如 502 或请求体超限的 413。

JSON 请求使用：

```text
Content-Type: application/json
```

受保护请求还需：

```text
Authorization: Bearer <access_token>
```

## 接口总览

| 方法 | 路径 | 访问要求 | 成功状态 |
|---|---|---|---|
| GET | /health | 公开 | 200 |
| POST | /api/v1/auth/register | 公开 | 201 |
| POST | /api/v1/auth/login | 公开 | 200 |
| GET | /api/v1/me | 已登录 | 200 |
| GET | /api/v1/staff/me | support 或 admin | 200 |
| GET | /api/v1/admin/me | admin | 200 |
| POST | /api/v1/conversations | 已登录 | 201 |
| GET | /api/v1/conversations/:id | 会话所有者 | 200 |
| POST | /api/v1/conversations/:id/messages | 会话所有者 | 201 |
| GET | /api/v1/conversations/:id/messages | 会话所有者 | 200 |
| GET | /api/v1/conversations | 已登录，仅自己的会话 | 200 |

所有受保护接口共享当前用户的限流额度：
从首次计数开始的 60 秒窗口内允许 60 次请求。
认证通过但业务失败的请求也计入额度。
注册、登录和健康检查不经过这个用户 ID 限流器。
注册和登录使用独立的 IP 限流器：
同一客户端 IP 在首次计数开始的 60 秒窗口内，共享 10 次请求额度。
进入认证限流器的请求即参与计数，不以注册或登录是否成功为条件。
超过额度返回 429，并附带 Retry-After 响应头。
健康检查不经过认证 IP 限流器。

`/api/v1` 请求体读取上限为 64 KiB。
声明的请求体长度超过上限，或 JSON 绑定读取触发大小限制时，返回 413。
该限制针对请求体字节数，与标题、消息内容的字符数限制分别校验。

API 请求 Context 设置 40 秒截止时间。
业务层返回的截止时间错误映射为 504；
限流检查错误返回 503，缓存快照更新失败不改变已成功保存消息的响应。

## 注册

POST /api/v1/auth/register

```json
{
  "username": "demo_user",
  "password": "DemoOnly_123!"
}
```

规则：

- 用户名去除首尾空白后为 3～64 个字符。
- 密码为 8～72 个字节，不清理首尾空白。
- 服务端使用 bcrypt 保存哈希。
- 注册角色固定为 user，客户端不能指定权限角色。

成功响应示例：

```json
{
  "id": 7,
  "username": "demo_user",
  "role": "user"
}
```

用户名重复返回 409，参数错误返回 400。

## 登录

POST /api/v1/auth/login

请求字段同注册。

成功响应示例：

```json
{
  "access_token": "<JWT>",
  "token_type": "Bearer"
}
```

响应带有 `Cache-Control: no-store`。
用户名不存在和密码错误统一返回 401、invalid_credentials。
数据库异常返回 500。

JWT 使用 HS256，包含 sub、iss、iat、exp。
sub 保存用户 ID，默认有效期为 2 小时。
JWT 不包含密码、密码哈希或角色。

## 当前用户及角色验证

GET /api/v1/me

成功响应：

```json
{
  "id": 7,
  "username": "demo_user",
  "role": "user"
}
```

身份取自验证后的 JWT，用户信息取自 MySQL。
URL 中的 user_id 参数不能改变查询身份。
用户已不存在返回 401。

/staff/me 和 /admin/me 返回同样的公开字段，但增加角色检查：

- /staff/me：support、admin。
- /admin/me：admin。
- 角色不匹配返回 403。
- 角色从数据库读取，不依赖 Token 中的旧权限。

这些是只读角色验证入口，不提供跨用户查询功能。

## 创建会话

POST /api/v1/conversations

```json
{"title":"账号申诉咨询"}
```

标题去除首尾空白后为 1～200 个字符。
归属使用 JWT 用户 ID；额外提交 user_id 不会改变归属。

成功返回会话对象，包括：
id、user_id、title、created_at、updated_at。

当前实现中，如果对应用户已不存在，创建会话返回 404、user_not_found。

## 查询会话列表

GET /api/v1/conversations

用户身份来自 JWT，不接受客户端指定查询归属。
返回当前用户的会话数组，按 id 倒序排列。
无会话时返回 []，当前未实现分页。

每项包含：
id、user_id、title、created_at、updated_at。

## 查询会话

GET /api/v1/conversations/:id

只允许访问自己的会话。
会话不存在和属于其他用户时均返回 404、conversation_not_found。
非法 ID 返回 400。

## 写入消息

POST /api/v1/conversations/:id/messages

```json
{"content":"账号被封禁后如何申诉？"}
```

内容去除首尾空白后为 1～10000 个字符。
消息来源固定为 user，不能由请求指定。
必须通过会话所有权检查。

通过会话所有权和输入校验后：

1. 获取当前实例内的会话占用；同一会话已在处理中时返回
   HTTP 409、conversation_busy。
2. 读取 Redis 历史。非空缓存需要校验 MySQL 最新消息 ID；
   缓存为空、读取失败或尾部 ID 不一致时，查询最近 20 条消息。
   数据库校验失败时返回错误。
3. 构造系统 Prompt、历史和当前问题。文本总量超过 32 KiB 时，
   从最旧一轮开始移除历史；系统 Prompt 和当前问题不截断。
   仅这两项就超限时返回 HTTP 400、model_input_too_large。
4. 调用 Eino 模型，设置 max_tokens=1024。
5. 模型成功返回非空回答后，通过同一个 MySQL 事务保存用户消息和模型回答。
6. 提交后读取最近消息，整体替换 Redis 快照。处理结束时释放会话占用。

预算限制是 UTF-8 正文字节数，不是精确 Token 数量或 HTTP 请求体大小。
历史裁剪不删除已保存消息。

预算检查失败、模型失败或返回空回答时，不保存本轮消息。
数据库事务失败时，不更新缓存。
提交后的缓存刷新失败不改变消息保存成功的结果。

成功返回 HTTP 201，响应体仍是用户消息对象，包含：
id、conversation_id、role、content、created_at。

客户端随后通过 GET 消息历史获取模型回答。
当前为非流式响应，不返回 SSE 事件。

当前没有请求幂等机制。网络超时不能直接证明保存失败，
客户端应先查询历史，避免自动重试造成重复消息。
会话占用仅保护当前实例，不支持多副本协调。

## 查询消息历史

GET /api/v1/conversations/:id/messages

只允许访问自己的会话，从 MySQL 按消息 ID 升序读取。
无消息时返回 `[]`。
当前返回完整历史，尚未实现分页。
Redis 缓存缺失不触发历史消息删除或自动缓存回填。

## 错误响应

统一结构：

```json
{
  "code": "unauthorized",
  "message": "authentication required"
}
```

具体 message 可能因触发位置不同而变化，客户端应依据状态码和 code 判断。

| HTTP 状态 | code | 含义 |
|---|---|---|
| 400 | invalid_argument | JSON、字段或路径参数非法 |
| 400 | model_input_too_large | 系统 Prompt 与当前问题超过模型输入文本预算 |
| 401 | invalid_credentials | 用户名或密码错误 |
| 401 | unauthorized | 缺少身份、Token 无效或身份失效 |
| 403 | forbidden | 角色不满足要求 |
| 404 | user_not_found | 创建会话时用户不存在 |
| 404 | conversation_not_found | 会话不存在或不属于当前用户 |
| 409 | username_exists | 用户名重复 |
| 409 | conversation_busy | 当前实例中该会话正在处理上一条消息 |
| 413 | request_too_large | 请求体超过大小限制 |
| 429 | rate_limited | 超出用户 ID 或认证 IP 的限流额度 |
| 500 | internal_error | 内部异常 |
| 502 | model_unavailable | 模型调用失败 |
| 502 | model_empty_reply | 模型未返回有效文本 |
| 503 | rate_limit_unavailable | 无法完成 Redis 限流检查 |
| 504 | model_timeout | 模型调用超时 |
| 504 | request_timeout | 业务处理返回请求截止时间错误 |

429 响应包含整数秒的 Retry-After。
503 不表示已经执行业务：限流失败会阻止后续 Handler。

## 存储与权限边界

- 完整消息历史：MySQL。
- 短期上下文：Redis，最多 20 条，成功替换快照后设置 30 分钟 TTL。
- 缓存和数据库不保证强一致。
- user、support、admin 均不能通过现有会话接口访问他人会话。
- 注册不允许自选高权限角色，当前没有角色修改 API。