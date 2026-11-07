# 容器与 Kubernetes 部署

本说明面向已有配置与依赖的学习环境。应用不会自动下载模型、创建管理员或录入官方规则。命令是操作步骤，不代表已在你的目标环境执行。

## Compose 应用组合

根目录 `.env.example` 是变量说明；在本机受控环境配置实际密码、JWT 密钥、独立确认签名密钥、模型端点及 Key、Embedding 身份与源数据库 UUID。Go 进程本身不会自动读取 `.env`。不要输出展开后的 Compose 配置或将凭据放入前端变量。

以下命令在项目根目录、同一个 PowerShell 中执行。`bili-support-app` 是示例项目名；选择后保持一致，先核对现有容器及宿主端口，不能与已有环境争用 3307、6379、8088 等端口。

```powershell
$composeArgs = @('-p','bili-support-app',
  '-f','compose.yaml','-f','compose.elasticsearch.yaml',
  '-f','compose.milvus.yaml','-f','compose.kafka.yaml',
  '-f','compose.app.yaml')
docker compose @composeArgs --profile app --profile ops build api worker nginx migrate
docker compose @composeArgs up -d mysql redis elasticsearch etcd minio standalone kafka kafka-init
docker compose @composeArgs ps -a
```

确认依赖健康且 kafka-init 成功退出，再针对确认过的数据库执行初始化。首次初始化前读取该 MySQL 的 `@@server_uuid`，将其配置为 `KNOWLEDGE_SOURCE_SERVER_UUID`；不得随意生成或复制其他实例的 UUID。迁移与 seed 均失败即停止，不继续启动应用。

```powershell
docker compose @composeArgs --profile ops run --rm migrate
docker compose @composeArgs --profile ops run --rm seed
docker compose @composeArgs --profile app up -d api worker nginx
docker compose @composeArgs ps
docker compose @composeArgs exec nginx nginx -t
```

seed 只初始化分类，不创建管理员或规则。migrate 使用 AutoMigrate，不能替代旧库数据迁移方案。已有数据环境不能为了演示反复初始化或删除卷。

应用内部地址使用 mysql:3306、redis:6379、elasticsearch:9200、standalone:19530、kafka:9092。Embedding 使用 `CONTAINER_EMBEDDING_URL`，LLM 使用 `CONTAINER_LLM_BASE_URL`；容器的 localhost 不是宿主机。Kafka advertised listeners 必须向调用方返回可达地址。

浏览器访问 http://127.0.0.1:8088。Nginx 镜像内含静态资源，代理 api:8080，关闭 SSE 缓冲、缓存及上游自动重试。修改前端或 Nginx 配置后使用相同文件组合重新 build nginx、up -d nginx；API/Worker 修改后分别重建。

## Kubernetes 前置条件

- Chart 声明 Kubernetes >=1.26；实际隔离验证使用 k3s 1.31.5，不表示所有声明版本均完成测试。
- 使用支持 NetworkPolicy 的 CNI；启用 HPA 还需 metrics-server，Prometheus 不能替代资源指标 API。
- 将四类镜像发布到节点可访问的仓库，或显式导入测试节点；开发机存在镜像不代表节点可用。
- MySQL、Redis、Kafka、ES、Milvus、Embedding、LLM 必须从 Pod 网络可达，Kafka 返回的 broker 地址也必须可达。Chart 不安装这些依赖。
- 一个 namespace 仅安装一个应用 release：API Service 固定名 api，与 Nginx 上游一致。
- Namespace 和 Secret 独立管理，Helm 不负责创建真实凭据或删除它们。

复制 `deploy/helm/video-support-agent/values.yaml` 到本地受控的非敏感 values 文件，填写空地址、Embedding digest、实际数据库 UUID、LLM 型号及四类镜像仓库/版本。默认空值是待配置项，不能直接安装默认 values。

按 `deploy/kubernetes/secrets.example.yaml` 的键结构，通过受控 Secret 管理流程提供 DB_PASSWORD、JWT_SECRET、APPEAL_CONFIRMATION_KEY_BASE64、LLM_API_KEY、REDIS_PASSWORD。空 Secret 示例不能直接应用；真实 Secret 文件放在仓库之外。Worker 不注入 API 专用密钥。

下面环境变量由操作者设置为已经确认的文件路径：`KUBECONFIG`、`AGENT_VALUES_FILE`、`AGENT_SECRET_FILE`。命令不修改全局 context，必须先检查输出身份。

```powershell
kubectl --kubeconfig $env:KUBECONFIG config current-context
kubectl --kubeconfig $env:KUBECONFIG get nodes
kubectl --kubeconfig $env:KUBECONFIG apply -f deploy/kubernetes/namespace.yaml
kubectl --kubeconfig $env:KUBECONFIG apply -f $env:AGENT_SECRET_FILE
$helmArgs = @('upgrade','--install','support','deploy/helm/video-support-agent',
  '--kubeconfig',$env:KUBECONFIG,'-n','video-support-agent','-f',$env:AGENT_VALUES_FILE)
helm lint deploy/helm/video-support-agent -f $env:AGENT_VALUES_FILE --strict
```

## 首次初始化与应用启动

只对已确认的新环境按顺序执行，每一步退出成功后才继续。以下 m01/s01 是本次 Job 标识；重新执行时使用新的 1–12 位小写字母、数字或中间连字符标识，并同步修改 wait 中的名称。

```powershell
helm @helmArgs --set workloadsEnabled=false --set operations.migrate=true --set operations.seed=false --set operations.runId=m01
kubectl --kubeconfig $env:KUBECONFIG wait -n video-support-agent --for=condition=complete job/support-migrate-m01 --timeout=300s
helm @helmArgs --set workloadsEnabled=false --set operations.migrate=false --set operations.seed=true --set operations.runId=s01
kubectl --kubeconfig $env:KUBECONFIG wait -n video-support-agent --for=condition=complete job/support-seed-s01 --timeout=300s
helm @helmArgs --set workloadsEnabled=true --set operations.migrate=false --set operations.seed=false
kubectl --kubeconfig $env:KUBECONFIG rollout status -n video-support-agent deployment/support-api --timeout=120s
kubectl --kubeconfig $env:KUBECONFIG rollout status -n video-support-agent deployment/support-worker --timeout=120s
kubectl --kubeconfig $env:KUBECONFIG rollout status -n video-support-agent deployment/support-frontend --timeout=120s
```

迁移与 seed 互斥，且要求 workloadsEnabled=false。**不要在运行中的 release 上照搬首次初始化命令**，它会移除应用 Deployment。升级已有环境必须先制定迁移和停机方案。切换 ops 阶段前保留 Job 状态与脱敏日志；普通 Helm 管理的上一个 Job 可能在升级时被删除。

API 启动不等于规则已发布。管理员需按来源核验流程创建、核验并发布规则，Worker 通过 Kafka 构建并校验两个索引后才推进活动代际。分别核对源 Outbox sent、Inbox applied、active_epoch=desired_epoch、双索引 manifest 与查询结果；sent 不代表索引可查询。

## 入口、探针、指标与扩缩容

未启用 Ingress 时，可在本地使用：

```powershell
kubectl --kubeconfig $env:KUBECONFIG port-forward -n video-support-agent service/support-frontend 8088:80 --address 127.0.0.1
```

Ingress 默认关闭。启用时配置实际 controller class、域名、TLS Secret，以及 networkPolicy.ingressNamespace/ingressPodLabels；NetworkPolicy 同时匹配 namespace 与 Pod 标签。controller 及证书需另行准备，Chart 内的 Nginx 注解不能保证任意 controller 的 SSE 行为。

API /livez 不查询依赖；/readyz 在共享 2 秒预算内检查 MySQL/Redis，退出时拒绝就绪，不以 ES/Milvus/模型暂时故障摘除全部 API。Worker readiness 表示进入工作单元监督阶段，不保证依赖始终可达、已分配 Kafka 分区或索引完成，也不控制消费摘流。Worker 停止消费依赖 context 取消。

API 退出先撤销就绪并等待 5 秒，再进行最多 60 秒 HTTP Shutdown；Chart 终止预算为 90 秒。强制终止、外部代理或未响应取消的操作仍可能中断请求。

OBS_ENABLED 默认 false。开启时设置实际容器可达的 OBS_OTLP_ENDPOINT，以及 monitoring.namespace/podLabels。API/Worker 各自监听 9464；按两个 headless metrics Service 发现全部 Pod，不能只抓取负载均衡 Service 的一个随机副本。现有 host 网络 compose.observability.yaml 不会自动连接此集群；Grafana/Prometheus/OTLP 接通需单独验证。

API/Worker HPA 独立开启，默认 CPU 目标为 requests 的 70%，缩容稳定窗口为 300 秒。窗口取近期建议的最大副本数，不能简单从停止负载时计时。启用后用 get hpa、top pods、Deployment 和 Pod 状态确认实际行为；低 CPU 不保证立刻缩容。

Kafka 消费并行度受分区及消费者组约束，构建还受数据库协调、Embedding 和索引写入能力限制。增加 Worker 不保证吞吐线性增长。

隔离环境已验证独立扩容到 API 2/Worker 2、四 Pod 指标、自动缩容到各 1、缩容后搜索成功，以及发布事件与双索引一致。该部署验证使用合成 CPU 压力和模型协议夹具，不代表真实模型质量、生产吞吐或完整监控面板已验证。

## 隔离部署演示补充（2026-11-07）

隔离 Kubernetes 已通过真实模型服务完成官方英文用户协议第2.7条的搜索、详情读取和带引用 SSE 回答，并在浏览器查看保存结果及旧版本被替代后的条款原文。来源网页当日获取并与本地快照核对，仅适用于中国大陆以外用户；entry-001/entry-002是项目录入版本，日期零点UTC+8是项目转换约定。Embedding仍为常量向量协议夹具，本次不证明语义检索质量。

浏览器确认后创建本地人工复核工单，查询状态为pending_review；NoAppealPolicies没有被替换，不代表平台受理或真实申诉资格通过。管理员通过HTTP发布录入修订，Kafka/Worker完成双索引切换。Grafana曾显示API/Worker两条up=1曲线，证明当时抓取可用，不证明全部业务面板或告警有效。

演示后临时真实模型代理已停止，API恢复原隔离夹具配置。保留的客服页面可查看历史结果，新提问不再调用真实模型。原隔离集群和依赖保留，不能按生产部署理解。无新增性能容量结论。
