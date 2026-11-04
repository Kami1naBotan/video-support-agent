<script setup lang="ts">
import {
  computed,
  onBeforeUnmount,
  onMounted,
  ref,
  watch,
} from 'vue'
import {
  canConfirm,
  canRetryConfirmation,
  type AgentUIState,
} from '../chat/agent-state'

const props = defineProps<{
  state: AgentUIState
  disabled: boolean
}>()

const emit = defineEmits<{
  confirm: []
  dismiss: []
}>()

const acknowledged = ref(false)
const now = ref(Date.now())
let timer: ReturnType<typeof setInterval> | undefined

const operation = computed(() => props.state.operation)
const proposal = computed(() => operation.value?.proposal)

const retryAvailable = computed(() =>
    canRetryConfirmation(props.state),
)

const showConfirmationForm = computed(() =>
    operation.value?.phase === 'ready' || retryAvailable.value,
)

const expired = computed(() =>
    proposal.value !== undefined &&
    Date.parse(proposal.value.expires_at) <= now.value,
)

const confirmEnabled = computed(() =>
    acknowledged.value &&
    !props.disabled &&
    canConfirm(props.state, now.value),
)

const toolNames: Record<string, string> = {
  search_rule: '搜索规则',
  get_rule_detail: '读取规则条款',
  check_appeal_eligibility: '检查申诉条件',
  create_appeal_ticket: '创建本地工单',
  query_ticket_status: '查询工单状态',
}

const statuses: Record<string, string> = {
  running: '执行中',
  started: '执行中',
  completed: '已完成',
  succeeded: '成功',
  rejected: '未获准执行',
  failed: '失败',
  cancelled: '已取消执行',
  timed_out: '执行超时',
  unknown: '结果待核对',
}

const effects: Record<string, string> = {
  none: '尚未报告工单提交',
  committed: '工单已提交',
  unknown: '工单提交结果待核对',
}

const saves: Record<string, string> = {
  not_attempted: '尚未确认保存',
  saved: '消息已保存',
  failed: '消息保存失败',
  unknown: '消息保存结果待核对',
}

const phases: Record<string, string> = {
  ready: '等待你核对确认',
  submitting: '正在提交确认',
  uncertain: '本次确认结果待核对，请勿重复提交',
  submitted: '已收到工单提交结果',
  dismissed: '你已放弃这张确认卡片',
}

const executionText = computed(() => {
  const status =
      props.state.recovery?.stored.execution_status ??
      props.state.result?.execution_status
  if (status !== undefined) return statuses[status] ?? status

  return props.state.busy
      ? '执行中'
      : '执行结果待核对'
})

function timeText(value: string): string {
  return new Date(value).toLocaleString()
}

watch(
    [
      () => proposal.value?.confirmed_action.idempotency_key,
      () => operation.value?.phase,
    ],
    () => {
      acknowledged.value = false
    },
)

onMounted(() => {
  timer = setInterval(() => {
    now.value = Date.now()
  }, 1000)
})

onBeforeUnmount(() => {
  if (timer !== undefined) clearInterval(timer)
})
</script>

<template>
  <section class="agent-panel" aria-label="本轮工具执行与确认">
    <strong>本轮执行</strong>

    <div class="result-summary" aria-live="polite">
      <span>执行：{{ executionText }}</span>
      <span>业务：{{ effects[state.businessEffect] }}</span>
      <span>消息：{{ saves[state.messageSaveStatus] }}</span>
    </div>

    <div v-if="state.recovery" class="notice">
      <p>
        执行记录中的业务状态：
        {{ effects[state.recovery.stored.business_effect] }}
      </p>
      <p>
        本次恢复观察：
        {{ effects[state.recovery.business_effect] }}
        · {{ saves[state.recovery.message_save_status] }}
      </p>
      <p v-if="!state.recoveryResolved">
        恢复核对尚未完成，请稍后再次刷新；不会自动重新提交。
      </p>
      <p
          v-else-if="
        !operation &&
        state.recovery.stored.decision === 'confirmation_required'
      "
      >
        历史中不包含确认凭据。如需继续申请，请重新咨询并核对新的确认卡片。
      </p>
    </div>

    <p v-if="state.ticketID">
      本地工单号：{{ state.ticketID }}
    </p>

    <p v-if="state.result?.ticket?.replayed">
      本次返回的是原操作对应的已有工单。
    </p>

    <p v-if="state.result?.ticket_status">
      查询工单 {{ state.result.ticket_status.ticket_id }}：
      {{
        state.result.ticket_status.status === 'pending_review'
            ? '待人工复核'
            : '已提交'
      }}
    </p>

    <p v-if="state.result?.audit_pending" class="notice">
      执行记录尚待核对；已显示的工单提交事实仍然有效。
    </p>

    <p v-if="state.interrupted" class="notice" role="status">
      连接未完整结束。停止接收不会撤销已提交工单，请先核对恢复结果。
    </p>

    <ol v-if="state.tools.length" class="tool-list">
      <li v-for="tool in state.tools" :key="tool.call_id">
        <strong>{{ toolNames[tool.tool_name] ?? tool.tool_name }}</strong>
        <span>：{{ statuses[tool.status] ?? tool.status }}</span>

        <span v-if="tool.duration_ms !== undefined">
          · {{ tool.duration_ms }} ms
        </span>

        <span v-if="tool.business_effect === 'committed'">
          · 工单已提交
        </span>
        <span v-else-if="tool.business_effect === 'unknown'">
          · 提交结果待核对
        </span>

        <span v-if="tool.audit_pending"> · 执行记录待核对</span>

        <small v-if="tool.error_code !== 'none'">
          原因码：{{ tool.error_code }}
        </small>
      </li>
    </ol>

    <div v-if="operation && proposal" class="confirmation">
      <strong>确认本地工单申请</strong>
      <p role="status">
        {{
          retryAvailable
              ? '已核对上一次未提交，可使用原申请和原操作重试'
              : phases[operation.phase]
        }}
      </p>

      <template v-if="showConfirmationForm">
        <dl>
          <dt>申请对象</dt>
          <dd>
            {{
              proposal.confirmed_action.subject_type === 'account'
                  ? '账号'
                  : proposal.confirmed_action.subject_type === 'content'
                      ? '内容'
                      : '其他'
            }}
            · {{ proposal.confirmed_action.subject_ref }}
          </dd>

          <dt>申请原因</dt>
          <dd class="reason">{{ proposal.confirmed_action.reason }}</dd>

          <dt>受理方式</dt>
          <dd>
            {{
              proposal.confirmed_action.intake_mode === 'manual_review'
                  ? '本地人工复核'
                  : '本地标准受理'
            }}
          </dd>

          <dt>确认有效期</dt>
          <dd>{{ timeText(proposal.expires_at) }}</dd>
        </dl>

        <p class="notice">
          {{
            proposal.confirmed_action.intake_mode === 'manual_review'
                ? '当前条件尚不能确认符合申诉要求。确认后仅创建本地待人工复核工单，不代表平台已受理或申诉通过。'
                : '确认后创建本地工单，不代表平台已受理或申诉通过。'
          }}
        </p>

        <p v-if="expired" class="notice">
          这张确认卡片已过期，请放弃后重新咨询。
        </p>

        <label class="acknowledgement">
          <input
              v-model="acknowledged"
              type="checkbox"
              :disabled="disabled || state.busy || expired"
          />
          我已核对申请内容，并理解上述受理范围。
        </label>

        <div class="actions">
          <el-button
              type="primary"
              :disabled="!confirmEnabled"
              @click="emit('confirm')"
          >
            {{
              retryAvailable
                  ? '使用原申请重试'
                  : proposal.confirmed_action.intake_mode === 'manual_review'
                      ? '确认提交人工复核'
                      : '确认创建本地工单'
            }}
          </el-button>

          <el-button
              :disabled="disabled || state.busy"
              @click="emit('dismiss')"
          >
            放弃本次确认
          </el-button>
        </div>
      </template>
    </div>
  </section>
</template>

<style scoped>
.agent-panel {
  margin: 16px 0;
  padding: 16px;
  border: 1px solid #dcdfe6;
  border-radius: 8px;
  overflow-wrap: anywhere;
}

.result-summary {
  display: flex;
  flex-wrap: wrap;
  gap: 8px 20px;
  margin: 12px 0;
}

.tool-list {
  padding-left: 24px;
}

.tool-list li {
  margin: 8px 0;
}

.tool-list small {
  display: block;
  color: #606266;
}

.confirmation {
  margin-top: 16px;
  padding-top: 16px;
  border-top: 1px solid #dcdfe6;
}

.confirmation dt {
  margin-top: 10px;
  font-weight: 600;
}

.confirmation dd {
  margin: 4px 0 0;
}

.reason {
  white-space: pre-wrap;
}

.notice {
  color: #8a5700;
}

.acknowledgement {
  display: flex;
  align-items: flex-start;
  gap: 8px;
}

.actions {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 16px;
}
</style>