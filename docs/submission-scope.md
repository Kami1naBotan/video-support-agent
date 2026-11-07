# 本地测试版提交目录建议

2026-11-07用户确认：Git初始化、提交、远程推送、历史整理均不属于本会话范围；只提供目录建议。未执行任何Git写操作。项目根目录为C:/Users/ROG/Documents/Codex/2026-09-21/video-support-agent。

## 建议纳入的工程文件

- cmd/：API、Worker、迁移、seed、评测和重建命令及测试。
- internal/：业务实现、存储、检索、工具、运行合同及测试。
- frontend/src/、frontend/public/：源码、页面测试及项目静态资产。
- frontend/package.json、package-lock.json：必须一起提交，保留依赖修复的限定overrides及可重现锁文件。
- frontend/index.html、tsconfig*.json、vite.config.ts、vitest.config.ts、.gitignore、README.md。
- deploy/：Dockerfile、Helm、Kubernetes、Nginx、ES映射与监控示例；只含模板，不包含展开后的真实Secret。
- docs/：API/部署/架构决策/HTTP请求说明；docs/knowledge内官方HTML快照和网页资源单独评估是否有必要全量纳入，可保留来源URL、取得时间、SHA256与所需快照。请求示例只能含占位变量。
- 根目录go.mod、go.sum、compose*.yaml、.dockerignore、.gitignore、README.md、GLOSSARY.md、AGENTS.md、.env.example（确认只有示例/空值）。
- configs/、scripts/若将来有实际非敏感内容再纳入；当前为空，不制造占位提交。

## 默认排除

.env及真实环境变体、key.txt、真实Secret/kubeconfig、令牌/数据库导出、.idea/、node_modules/、dist/、work/、outputs/、api.exe及其他构建产物、日志和缓存。不要整体加入.scratch/：其中包含运行时环境与大量证据，应只将确需共享且脱敏的脚本/报告另行挑选。

.scratch/tools/script/test-frontend-dependency-compatibility.cjs是本次有用的依赖兼容性测试；如需长期保留，可在后续提交准备中放入正式测试脚本目录并调整引用。本轮未搬动文件。原始验收证据作为本地归档保存，不默认上传。

根.gitignore尚未全面覆盖.scratch/outputs/api.exe；frontend自己的.gitignore覆盖node_modules/dist。提交前由提交操作者完善忽略规则并逐项检查暂存清单，不使用git add .作为本建议的执行方式。无原仓库历史时只能如实建立当前快照，不能倒填学习阶段历史。
