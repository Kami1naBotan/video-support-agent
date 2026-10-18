# 规则智能客服前端

使用 Vue 3、TypeScript、Vite、Vue Router、Pinia、
Axios 和 Element Plus。

完整启动、环境配置和 Nginx 联调说明见
[项目 README](../README.md)。

在项目根目录运行：

- 安装锁定依赖：npm --prefix frontend ci
- 开发启动：npm --prefix frontend run dev
- 单元测试：npm --prefix frontend test
- 类型检查及构建：npm --prefix frontend run build

开发入口：http://127.0.0.1:5173
Nginx 验收入口：http://127.0.0.1:8088

两个入口属于不同来源，登录状态不共享。