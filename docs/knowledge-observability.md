# 知识库异步状态、观测与故障处理

本文按当前API、Worker和仓储实现编写。结构化状态日志、Prometheus指标与OTLP链路各自提供不同证据；已有Grafana面板，没有自动DLQ重放接口。下列代码时间预算不是性能测量；文末单独记录本地知识读取样本，不提供生产容量结论。

## 状态分层

|层次|状态或结果|解释|
|---|---|---|
|API规则事务|创建/编辑/发布/下线成功|业务状态与源Outbox已提交，不等于消息或索引完成|
|源或派生Outbox|pending / sending / sent / blocked|待发送/已认领发送/发送确认已记录/投递阻塞；attempts为投递次数|
|inbox|received / waiting|合法事件已接收或等待处理|
|inbox终态|applied / ignored_draft / superseded / dead_lettered|已应用/草稿忽略/已被新状态取代/死信决定；不能把全部终态都当索引成功|
|事件attempt|scheduled / running / succeeded / failed / obsolete|单个事件的尝试状态；与全量任务build_attempt不同|
|全量任务|pending / building / retry_wait / completed / obsolete / blocked|epoch构建进度；blocked需结合safe_error_code判断容量或失败预算|
|恢复记录|pending / completed|失败决定的分批成员恢复进度，不等于索引发布|
|代际|verified与published|完整构建验证和权威发布不同；查询选择以控制行active为准|

API /deliveries仅列knowledge.document.events源Outbox，不展示Retry/DLQ派生消息或Worker最终状态。Worker提交源offset表示已持久化inbox/任务，提交Retry offset表示重试调度已持久化，均不表示目标已构建。发布事务可结算一批事件，剩余事件由后续分批追平，不承诺所有事件终态与active切换同一时刻出现。

## 观测日志字段

|日志|字段|定义/单位|限制|
|---|---|---|---|
|knowledge receive|topic / partition / offset|收到的Kafka位置|未知topic映射unexpected|
|knowledge receive|event_id / trace_id|校验后的事件关联|坏消息可能没有这些字段|
|knowledge receive|event_age_seconds|本机接收时刻减source occurred_at，秒|含发布到接收前等待；不是构建耗时，时钟超前保留负值并置event_clock_ahead|
|knowledge receive|receive_duration_seconds / code|包装的接收处理调用耗时与安全分类|不包含后续offset提交或索引构建；accepted不表示applied|
|knowledge build attempt|generation_id / source_epoch / build_attempt / code|全量构建身份、次序及结果分类|不直接提供每个参与event的身份|
|knowledge build attempt|build_duration_seconds|builder.Build调用耗时，秒|不包含随后的Publish事务；verified日志不等于published|
|generation published|generation_id / source_epoch|Publish成功返回|提交未知时另有publication_unconfirmed，需查权威状态|
|knowledge observation，database|sample_at / desired_epoch / active_epoch / epoch_gap|MySQL只读RR快照；gap=desired-active|gap不是消息数或秒数，多个事件可以合并构建|
|同上|state_counts|按scope/state、Outbox再按topic统计当前行数|当前数量不是累计计数器，不能直接当处理速率|
|state_counts元素|oldest_created_age_seconds|该状态中最早created_at距采样时间的秒数|不是进入该状态后的时长；空桶count=0且年龄null|
|同上|oldest_due_wait_seconds|已到期scheduled attempt中最早not_before距采样时刻的秒数|无到期项为null，不伪装成零|
|同上|poison_count|隔离记录当前总数|不能凭重复采样推导新毒消息数|
|knowledge attempt sample|event_id / trace_id / event_attempt / generation_id / state|最近更新的最多32条attempt关联样本|不是全量审计流；重复出现不重复计数|
|同上|schedule_delay_seconds|started_at-not_before，秒|仅有started_at时输出；负值附schedule_time_inconsistent|
|同上|attempt_elapsed_seconds|completed_at-started_at，秒|仅两时间均存在时输出；负值附attempt_time_inconsistent；不等于端到端延迟|
|knowledge observation，kafka|topic / partitions|Source和Retry两个消费组对应Topic的位置样本|组来自运行配置；该日志本身不输出group字段，跨部署汇总需补部署/组标签|

一次观测依次采集数据库和两个Kafka目标，完成后等待15秒，因此不是严格每15秒固定采样。数据库采样3秒预算，各Kafka采样3秒预算。数据库内部RR一致；数据库与Kafka采样之间不构成统一快照，Kafka的提交/start/end读取也不是原子事务。

## Kafka lag判读

每个分区输出partition、start_offset、end_offset、committed_offset、lag和status。

- available：committed位于[start,end]，lag=end-committed。单位为offset位置差，不是耗时或已验证的业务事件数。
- uncommitted：无有效提交位置，committed_offset和lag为null。
- offset_out_of_range：提交位置落在保留范围外，保留committed值，lag为null。检查保留策略和组状态，不直接修改offset掩盖问题。
- 整次采样失败：available=false，code=lag_sample_unavailable；数据库失败为database_sample_unavailable，不输出假正常零快照。

lag=0仍可能有长时间building、retry_wait或blocked任务，因为接收持久化先于构建完成。持续epoch_gap、已到期等待、Outbox积压和分区lag应结合观察，不从一个值推断全部链路健康。

## 关联与脱敏

API生成X-Trace-ID，源事件携带trace_id及event_id。用event_id关联inbox与attempt，再用generation_id关联构建、发布及恢复；多个事件可以对应同一个全量构建。Retry接收另记录failed_generation_id和下一event_attempt。非法关联字段输出correlation_invalid，不原样回显。坏消息按(topic,partition,offset)隔离，持久化摘要/长度等安全信息，不保存原文为诊断日志。

不要开启GORM原始SQL、打印凭据、消息正文或原始供应商错误作为日常排障办法。跨进程应记录部署身份和日志时间；不要将event_id/generation_id直接设计为无限增长的指标标签。Prometheus指标与OTLP的配置和语义见下文。

## 排查顺序

|现象|先查|处理边界|
|---|---|---|
|API启动失败|安全启动码、DB身份、必要表列、控制行/bootstrap、模型配置|确认目标再运行必要迁移；不自动补控制行、不把损坏当未就绪|
|搜索503 search_index_not_ready|是否尚无active、Worker bootstrap/task状态|允许API先启动；等待或修复Worker，不手动改active|
|search_generation_unavailable|数据库访问和active解析|恢复依赖，不回退旧alias|
|search_generation_invalid|控制行、published代际、双目标/摘要合同|保留现场，查修复路径；不可将半成品强制设为active|
|发布成功但搜索没有新内容|源Outbox、Source lag、inbox/task、epoch_gap、构建与发布日志|依次定位，不盲目重复发布；权威复核可能过滤旧候选|
|Outbox pending/sending持续|broker/Topic合同、relay运行与租约|发送未知可能已成功；由现有幂等重放接住|
|retry_wait|原not_before、Retry Outbox/receipt、到期attempt|按持久时间等待，不改时间戳加速真实业务|
|blocked/resource_capacity|代际预留数及持久上限|当前无自动GC；不删除active或未知半成品解围，不只改环境变量绕过合同|
|预算耗尽或永久失败|failure_code、reason_code、事件attempt与构建次数|dead_lettered不保证DLQ已送达；无自动重放接口，先修复根因再设计受控恢复|
|publication_unconfirmed|控制行与目标代际published状态|不能先认定失败，再盲目切换active|
|下线后搜索为空|MySQL当前状态与新代际状态|过滤即可使查询为空；只有全局无有效规则并发布empty才证明空代际|

/health只是HTTP存活，active_generation_ready只是启动快照。本文不提供删除数据、改offset、重置预算或修改控制行的快捷命令。真实故障演练在归属明确的隔离环境执行。
## Prometheus 指标与 OTLP 链路

观测默认关闭（OBS_ENABLED=false）。启用后，API与Worker各自持有独立Registry和TracerProvider；业务HTTP端口不承载/metrics。Prometheus抓取指标，OTLP/HTTP将trace送往Jaeger，Grafana查询Prometheus。Kafka lag同时提供下述Prometheus Gauge；epoch和状态行数仍按上文输出结构化日志，未自动转成指标。

|配置|默认值/使用要求|
|---|---|
|OBS_ENABLED|false；设为true启用|
|OBS_SERVICE_NAME|video-support-agent；实际trace服务名追加-api或-worker|
|OBS_OTLP_ENDPOINT|http://127.0.0.1:4318；只填根地址，导出器使用/v1/traces|
|OBS_SAMPLE_RATIO|1；0～1，ParentBased根trace比例采样，不决定指标是否计数|
|OBS_METRICS_ADDR|127.0.0.1:9464；只允许回环IP和有效端口。Worker需显式设为127.0.0.1:9465，默认不会自动区分进程|

在各自启动API/Worker的终端设置环境变量；先确保业务依赖配置完整，再按已有启动说明运行进程。修改变量不改变已经运行的进程。

```powershell
$env:OBS_ENABLED='true'
$env:OBS_SERVICE_NAME='video-support-agent'
$env:OBS_OTLP_ENDPOINT='http://127.0.0.1:4318'
$env:OBS_SAMPLE_RATIO='1'
$env:OBS_METRICS_ADDR='127.0.0.1:9464' # API终端；Worker终端改为9465
```

本地观测栈由compose.observability.yaml提供：Prometheus v3.2.1（127.0.0.1:9090）、Grafana 11.6.0（127.0.0.1:3000）、Jaeger 1.62.0（UI 127.0.0.1:16686，OTLP HTTP 4318）。运行 `docker compose -f compose.observability.yaml up -d` 启动；Docker Desktop需启用host networking。Grafana使用本地匿名Viewer，预置overview面板；此配置不应作为公网部署配置。Jaeger使用内存存储，重启丢失trace；Prometheus保留3天数据。

Prometheus每15秒抓取、3秒超时，使用process_role区分api/worker；LLM自己的role仍为planner/answer，不能覆盖它。Worker暴露端点不代表其Registry拥有API的全部指标。未发生对应操作时，指标序列可能尚未产生；No data不能直接当作零错误。

|指标|类型/单位|标签|测量边界|
|---|---|---|---|
|http_server_request_duration_seconds|Histogram，秒|method、route、status_class|Gin中间件包围的处理耗时；SSE包含流式处理全程，不是首段延迟|
|llm_call_duration_seconds|Histogram，秒|role、outcome|HTTP传输开始到响应体EOF、Close或取消；不是语义正确率|
|es_query_duration_seconds|Histogram，秒|outcome|ES搜索操作及响应校验边界，不代表整个混合RAG|
|milvus_query_duration_seconds|Histogram，秒|outcome|Milvus搜索操作及响应校验边界，不代表整个混合RAG|
|kafka_consumer_lag|Gauge，offset位置差|group、topic、partition|有效且新鲜样本的end-committed；不是索引完成度|
|kafka_consumer_lag_available|Gauge，0/1|group、topic、partition|无提交、越界、采样失败或样本超过45秒时为0，并不输出旧lag|
|kafka_consumer_lag_last_success_timestamp_seconds|Gauge，Unix秒|group、topic、partition|保留最近有效采样时间；从未成功时为0，不是延迟|
|tool_calls_total|Counter，次|tool、phase、outcome|实际工具调用结束时计数，不是Planner提出的调用数|
|agent_failures_total|Counter，次|phase、reason|失败、超时或未知的Agent执行；不是按HTTP状态推算|
|agent_cancellations_total|Counter，次|phase|单独计取消，不能再次并入失败计数|

Kafka标签限定于配置的Source/Retry两个目标及各3个分区。判读lag应同时检查available与Prometheus up；最近成功时间不代表当前采样可用。

HTTP使用路由模板，未匹配为unmatched；标签采用有限分类，不含用户ID、文档ID、原始URL、问题或Token。generation_id只作为span属性用于关联，不作为指标标签。

LLM outcome=success要求读到成功HTTP响应的EOF；SDK可能已解码出合法模型结果就Close，此时为closed，且当前span会标记Error。closed不能直接解释为模型回答失败，更不能据此计算业务成功率。需结合Agent结果和保存状态判断。

Histogram派生_bucket、_sum、_count；例如最近5分钟按route估算服务端P95：

```promql
histogram_quantile(0.95,
  sum by (le, route) (rate(http_server_request_duration_seconds_bucket[5m])))
```

这是桶插值估计，取决于流量和窗口；不能与k6原始请求样本分位混为同一统计，也不能平均多个P95。HTTP 2xx可能含业务降级，业务正确率需独立校验。4xx、5xx按实际问题分别解释，不把所有非成功传输都称为模型错误。

API上下文经持久化trace_parent、Kafka header传播到publish/receive。knowledge.build使用独立根trace，并以最多128个去重link关联实际claim成员；app.generation_id精确关联代际，app.outcome=published表示该构建调用观察到发布成功。来源读取可用性/截断由app.source_links_available、app.source_links_truncated表示。API事务先调度构建、Kafka稍后接收时，claim可能没有来源成员，此时无receive link合法；不能伪造未来的因果关系。结构化日志build_duration_seconds只包围builder.Build，与knowledge.build span边界不同。

导出采用有界队列2048、批次256、1秒批处理，导出超时3秒且不重试；关闭观测最长预算5秒。后端恢复可接收后续span，但不保证故障期数据补发。trace和指标不记录正文、凭据、原始供应商错误；安全日志与业务审计仍有各自职责，trace不能替代持久业务记录。

## 本地知识读取负载样本

2026-11-07，Windows amd64/Go1.27.1、32逻辑CPU，MySQL 8.4.11、Redis 7.4.7、k6 0.57.0。固定到达率5请求/秒持续60秒，预分配5VU/最大10VU；10个合成用户均匀轮转，保留生产60次/分钟/用户限流。另1个用户预热10次并全部通过，预热不进入正式统计。

路径GET /api/v1/knowledge/documents/:id/current，生产router/JWT/角色/Redis限流/handler/service/repository，独立MySQL中的1文档/1版本/1短条款。API以原生httptest进程运行，k6通过Docker host networking请求回环端口；不含登录、cmd/api启动、发布、LLM、SSE或搜索。观测导出关闭、访问日志为nop，未用race插桩；这些开销不包含在结果中。

|指标|实测|
|---|---|
|请求/内容身份校验成功|301/301|
|HTTP失败率/业务失败率|0%/0%|
|dropped_iterations|0|
|成功请求平均/P50|8.2848 / 8.0744 ms|
|成功请求P95/P99|10.6901 / 12.1915 ms|
|成功请求最大|18.7351 ms|

每次核对文档、版本、条款ID及完整合成内容。成功延迟为k6 response.timings.duration，即发送+等待+接收，不含建连时间，不是完整迭代耗时。本次全部请求成功，成功样本与全部HTTP样本延迟相同；失败场景应分别报告，不能删掉失败后美化整体延迟。k6失败率中的passes/fails指布尔真/假计数，rate=0且fails=301表示301个false（未失败），并非301次失败。

301为运行器实际样本数，不将5×60的理论近似替代实际计数。P95/P99只描述本样本，尾部样本少，未测饱和容量或生产SLO。连接池最大10、空闲5，本次累计等待次数/时间均为0；这不足以定位CPU、数据库或其他瓶颈。压测数据不能外推LLM、聊天、SSE、搜索或大语料性能。Grafana合成展示曲线与这次k6实测是不同来源的数据。


