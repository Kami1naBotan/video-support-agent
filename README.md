# Bilibili 官方规则智能客服 AI Agent

这是一个基于 Go 构建的 Bilibili 官方规则智能客服 AI Agent 项目。

项目计划使用 Bilibili 官方社区规范、投稿规则、版权规则、账号处罚规则、弹幕规范和申诉流程等资料，构建可检索、可引用、可追溯的规则知识库，为用户提供规则咨询、申诉指引、工单查询和人工客服转接能力。

## 技术栈

当前已实现：

- Go
- Gin
- Go Modules
- Zap
- GORM
- MySQL 8.4
- Redis（规划中）
- Elasticsearch（规划中）
- Milvus（规划中）
- Kafka（规划中）
- Vue 3（规划中）
- Nginx（规划中）
- Docker Compose
- Docker
- Kubernetes（规划中）

## 当前完成内容

- 初始化 Go Module；
- 创建 Go 后端目录结构；
- 创建 Gin API 服务；
- 添加 `/` 根路径；
- 添加 `/health` 健康检查接口；
- 添加基础环境配置；
- 添加 Zap 结构化日志；
- 添加路由和处理器分层；
- 添加基础路由测试；
- 验证格式化、测试和编译流程。
- Docker Compose 启动 MySQL；
- 环境变量数据库配置与连接池；
- GORM 模型和 `AutoMigrate` 迁移；
- User、Conversation、Message、KnowledgeDocument、KnowledgeChunk 模型；
- Repository、Service、Handler 分层；
- 会话和消息创建、查询接口；
- 统一错误响应、参数校验；
- Service、Handler 单元/接口测试；
- MySQL Repository 集成测试。

## 本地启动

复制 `.env.example` 为 `.env`，填写本地密码，然后启动 MySQL：

```powershell
docker compose up -d mysql
```

GoLand 的 API 运行配置需要设置：

```text
DB_HOST=127.0.0.1
DB_PORT=3307
DB_USER=bili_app
DB_PASSWORD=应用密码
DB_NAME=bili_support
```

首次创建表结构：

```powershell
go run ./cmd/migrate
```

启动 API：

```powershell
go run ./cmd/api
```

健康检查：

```text
GET http://localhost:8080/health
```

## API

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/conversations` | 创建会话 |
| GET | `/api/v1/conversations/:id` | 查询会话 |
| POST | `/api/v1/conversations/:id/messages` | 创建用户消息 |
| GET | `/api/v1/conversations/:id/messages` | 按 ID 顺序查询消息历史 |

创建会话示例：

```json
{"user_id":1,"title":"账号申诉咨询"}
```

创建消息示例：

```json
{"content":"账号被封禁后如何申诉？"}
```

## 数据库设计

| 表 | 用途 |
|---|---|
| `users` | 项目用户 |
| `conversations` | 用户客服会话，外键关联 `users` |
| `messages` | 会话中的完整消息，按 `id ASC` 查询 |
| `knowledge_documents` | 官方规则原文及版本 |
| `knowledge_chunks` | 规则文档切分片段，关联具体文档版本 |

完整消息保存在 MySQL，保证历史可持久化、可分页查询并参与事务；Redis 后续只用于短期上下文和缓存。

## 测试

普通测试不依赖 MySQL：

```powershell
go test ./...
```

MySQL 集成测试使用独立数据库 `bili_support_test`，并通过事务回滚清理测试数据。设置环境变量后执行：

```powershell
$env:RUN_MYSQL_TESTS="1"
$env:DB_HOST="127.0.0.1"
$env:DB_PORT="3307"
$env:DB_USER="bili_app"
$env:DB_PASSWORD="应用密码"
$env:DB_NAME="bili_support_test"
go test ./internal/repository -run TestMessageRepositoryMySQL -v
```

## 当前项目结构

```text
.
├── cmd
│   └── api
│       └── main.go
├── configs
├── docs
├── internal
│   ├── config
│   ├── database
│   ├── handler
│   ├── logger
│   ├── middleware
│   ├── model
│   ├── repository
│   ├── router
│   └── service
├── scripts
├── go.mod
├── go.sum
└── README.md
