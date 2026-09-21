# Bilibili 官方规则智能客服 AI Agent

这是一个基于 Go 构建的 Bilibili 官方规则智能客服 AI Agent 项目。

项目计划使用 Bilibili 官方社区规范、投稿规则、版权规则、账号处罚规则、弹幕规范和申诉流程等资料，构建可检索、可引用、可追溯的规则知识库，为用户提供规则咨询、申诉指引、工单查询和人工客服转接能力。

## 技术栈

当前基础骨架：

- Go
- Gin
- Go Modules
- Zap
- GORM（规划中）
- MySQL（规划中）
- Redis（规划中）
- Elasticsearch（规划中）
- Milvus（规划中）
- Kafka（规划中）
- Vue 3（规划中）
- Nginx（规划中）
- Docker（规划中）
- Kubernetes（规划中）

## 当前完成内容

第 1 天完成：

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