# 前端依赖修复说明

2026-11-07，本地测试版收尾。此前官方npm审计为6项受影响包（1 high、5 moderate），本次直接复现并修复。用户明确要求解决该遗留项，授权修改依赖清单及锁文件；没有改业务Go/TS/Vue。

- follow-redirects：1.16.1→1.16.2，满足axios原^1.16.0范围，修复重定向敏感头问题。
- js-beautify的glob：13.0.6→14.0.2，修复取消信号下继续遍历问题；使用限定于js-beautify的override。
- js-beautify→editorconfig的minimatch：10.2.6→11.0.1，修复globparts资源放大；使用限定于该父链的override。
- glob自带minimatch解析为11.0.5，path-scurry升至3.0.0，增加@isaacs/nonrecursive-glob-match 1.1.0；完整差异见证据lock-changes.json。

保留@vue/test-utils 2.5.1及其他直接依赖版本，不使用npm audit fix --force。glob/minimatch跨主版本，核对CommonJS globSync/hasMagic、Minimatch/escape调用及Node24兼容，并增加文件遍历、预先取消拒绝、EditorConfig匹配优先级、字面量转义和HTML格式化检查。未来上游依赖范围涵盖修复版本时，可移除overrides后重新审计和回归，不应长期无检查地保留。

结果：npm ci、115项测试/20文件、vue-tsc与Vite构建、Docker前端镜像构建均退出0；最终官方registry审计0漏洞。首次兼容性脚本误将Windows路径期待为正斜杠，改为path.join后全部通过，原失败保留。未降低测试断言或审计等级。

证据：.scratch/tools/evidence/dependency-closeout-20261107。新镜像video-support-agent-frontend:local-closeout-20261107。旧隔离集群未自动升级；源码/锁文件及新本地镜像已修复，旧镜像不借用新审计结论。0项是当前npm已知漏洞数据库下的结果，不是所有安全风险均不存在。
