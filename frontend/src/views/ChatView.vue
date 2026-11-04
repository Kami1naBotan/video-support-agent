<script setup lang="ts">
import {
  computed,
  onBeforeUnmount,
  onMounted,
  ref,
  watch,
} from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'

import { useAuthStore } from '../stores/auth'
import {
  createConversationAPI,
  listConversationsAPI,
} from '../api/conversation'
import type { Conversation } from '../api/conversation'
import {
  listMessagesAPI,
  recoverAgentRunAPI,
} from '../api/message'
import { streamAgentMessageAPI } from '../api/chat-stream'
import type { ChatMessage } from '../api/message'
import {
  errorMessage,
  isCanceled,
  isUnauthorized,
} from '../api/errors'

import RuleCitations from '../components/RuleCitations.vue'
import type { RuleCitation } from '../api/rule-citation'

import AgentPanel from '../components/AgentPanel.vue'
import { ChatStreamRequestError } from '../api/chat-stream-error'
import type { AgentMessageRequest } from '../api/agent-protocol'
import {
  applyAgentError,
  applyAgentEvent,
  beginConfirmation,
  blocksNewMessage,
  createAgentState,
  dismissConfirmation,
  finishAgentStream,
  interruptAgentStream,
  applyAgentRecovery,
  type AgentUIState,
} from '../chat/agent-state'

import {
  readRecoveryCheckpoint,
  writeRecoveryCheckpoint,
} from '../chat/recovery-checkpoint'

const router = useRouter()
const auth = useAuthStore()
const pageRevision = auth.revision

const pageUserID = auth.user?.id ?? 0
const historyReady = ref(false)
const recoveryNotice = ref('')

const conversations = ref<Conversation[]>([])
const selectedID = ref<number | null>(null)
const title = ref('')

const loadingList = ref(false)
const creating = ref(false)
const listError = ref('')

const messages = ref<ChatMessage[]>([])
const draft = ref('')
const loadingMessages = ref(false)
const sending = ref(false)
const messageError = ref('')

type StreamStatus =
    | 'waiting'
    | 'streaming'
    | 'completed'
    | 'stopped'
    | 'failed'

interface StreamTurn {
  conversationID: number
  question: string
  answer: string
  citations: RuleCitation[]
  status: StreamStatus
  error: string
}

const streamTurn = ref<StreamTurn | null>(null)
const agentStates = ref<Record<number, AgentUIState>>({})

const currentAgent = computed(() => {
  const id = selectedID.value
  return id === null ? undefined : agentStates.value[id]
})

const agentBlocksSending = computed(() => {
  const state = currentAgent.value
  return state !== undefined && blocksNewMessage(state)
})

const completionNotice = ref('')

const currentStream = computed(() => {
  const turn = streamTurn.value

  return turn?.conversationID === selectedID.value
      ? turn
      : null
})

const streamStatusText: Record<StreamStatus, string> = {
  waiting: '正在等待服务响应……',
  streaming: '正在查阅规则并准备回答……',
  completed: '回答已完成并保存',
  stopped: '已停止接收，请刷新消息确认保存结果',
  failed: '本轮回答未确认完成',
}

const drafts = new Map<number, string>()
const controllers = new Set<AbortController>()

let messageController: AbortController | null = null
let streamController: AbortController | null = null
let messageRequestID = 0
let disposed = false

const selectedConversation = computed(() =>
    conversations.value.find((item) => item.id === selectedID.value),
)

function isCurrentPage(): boolean {
  return (
      !disposed &&
      pageUserID > 0 &&
      auth.revision === pageRevision &&
      auth.user?.id === pageUserID
  )
}

function newController(): AbortController {
  const controller = new AbortController()
  controllers.add(controller)
  return controller
}

function releaseController(controller: AbortController) {
  controllers.delete(controller)
}

function stopPage() {
  disposed = true
  messageRequestID++

  for (const controller of controllers) {
    controller.abort()
  }

  controllers.clear()
  messageController = null
  streamController = null
  agentStates.value = {}
  drafts.clear()
  draft.value = ''
  messages.value = []
  streamTurn.value = null
  historyReady.value = false
  recoveryNotice.value = ''
}

async function handleRequestError(
    error: unknown,
    fallback: string,
): Promise<string | null> {
  if (!isCurrentPage() || isCanceled(error)) {
    return null
  }

  if (isUnauthorized(error)) {
    const invalidated = auth.invalidateIfCurrent(pageRevision)

    if (invalidated) {
      ElMessage.warning('登录已失效，请重新登录')
      await router.replace('/login')
    }

    return null
  }

  return errorMessage(error, fallback)
}

async function loadConversations() {
  if (!isCurrentPage() || loadingList.value || creating.value) {
    return
  }

  const controller = newController()
  loadingList.value = true
  listError.value = ''

  try {
    const result = await listConversationsAPI(controller.signal)

    if (!isCurrentPage()) return

    conversations.value = result

    const selectionExists = result.some(
        (item) => item.id === selectedID.value,
    )

    if (!selectionExists) {
      selectedID.value = result[0]?.id ?? null
    }
  } catch (error) {
    const text = await handleRequestError(error, '加载会话失败')

    if (text !== null && isCurrentPage()) {
      listError.value = text
    }
  } finally {
    releaseController(controller)

    if (isCurrentPage()) {
      loadingList.value = false
    }
  }
}

async function createConversation() {
  if (!isCurrentPage() || creating.value || loadingList.value) {
    return
  }

  const value = title.value.trim()

  if (!value || [...value].length > 200) {
    ElMessage.warning('请输入 1～200 个字符的会话标题')
    return
  }

  const controller = newController()
  creating.value = true

  try {
    const conversation = await createConversationAPI(
        value,
        controller.signal,
    )

    if (!isCurrentPage()) return

    conversations.value.unshift(conversation)
    selectedID.value = conversation.id
    title.value = ''

    ElMessage.success('会话创建成功')
  } catch (error) {
    const text = await handleRequestError(error, '创建会话失败')

    if (text !== null && isCurrentPage()) {
      ElMessage.error(
          `${text}。如结果不确定，请先刷新会话列表确认。`,
      )
    }
  } finally {
    releaseController(controller)

    if (isCurrentPage()) {
      creating.value = false
    }
  }
}

async function loadMessages() {
  if (!isCurrentPage() || currentAgent.value?.busy) return

  messageController?.abort()

  const requestID = ++messageRequestID
  const conversationID = selectedID.value

  historyReady.value = false
  messageError.value = ''
  recoveryNotice.value = ''

  if (conversationID === null) {
    messages.value = []
    loadingMessages.value = false
    messageController = null
    return
  }

  const controller = newController()
  messageController = controller
  loadingMessages.value = true

  function isCurrentRequest(): boolean {
    return (
        isCurrentPage() &&
        !controller.signal.aborted &&
        requestID === messageRequestID &&
        selectedID.value === conversationID
    )
  }

  async function recoverState(state: AgentUIState): Promise<void> {
    const runID = state.runID

    if (runID === undefined) {
      state.interrupted = true
      state.recoveryResolved = false
      recoveryNotice.value =
          '请求可能已经发送，但尚未取得执行标识。现有接口无法精确恢复这次请求，请先人工核对，不要重复提交。'
      return
    }

    try {
      const recovery = await recoverAgentRunAPI(
          conversationID!,
          runID,
          controller.signal,
      )

      if (!isCurrentRequest()) return

      applyAgentRecovery(state, recovery, true)

      recoveryNotice.value = state.recoveryResolved
          ? '已完成本次执行的恢复核对'
          : '执行、审计或消息保存仍待核对，请稍后再次刷新'
    } catch (error) {
      if (!isCurrentRequest()) return

      state.interrupted = true
      state.recoveryResolved = false

      if (
          error instanceof ChatStreamRequestError &&
          error.details?.recovery !== undefined
      ) {
        applyAgentRecovery(
            state,
            error.details.recovery,
            false,
        )
      }

      const text = await handleRequestError(
          error,
          '暂时无法完成恢复查询',
      )

      if (text !== null && isCurrentRequest()) {
        recoveryNotice.value =
            `${text}。已保留现有结果，不会自动重新提交。`
      }
    }
  }

  try {
    let state = agentStates.value[conversationID]

    if (state === undefined) {
      const checkpoint = readRecoveryCheckpoint(
          pageUserID,
          conversationID,
      )

      if (checkpoint !== undefined) {
        agentStates.value[conversationID] = createAgentState()
        state = agentStates.value[conversationID]

        if (state !== undefined) {
          state.busy = false
          state.interrupted = true
          state.runID = checkpoint.run_id
        }
      }
    }

    // 已有定位信息时，先恢复，再读取历史。
    if (state !== undefined) {
      await recoverState(state)
      if (!isCurrentRequest()) return
    }

    const result = await listMessagesAPI(
        conversationID,
        controller.signal,
    )

    if (!isCurrentRequest()) return

    messages.value = result

    // 没有本地定位信息时，可从本人历史找到最近的 Run。
    // 只恢复状态，不从历史构造确认请求。
    if (state === undefined) {
      let latest: ChatMessage | undefined

      for (const message of result) {
        if (
            message.agent !== undefined &&
            (latest === undefined || message.id > latest.id)
        ) {
          latest = message
        }
      }

      if (latest?.agent !== undefined) {
        agentStates.value[conversationID] = createAgentState()
        state = agentStates.value[conversationID]

        if (state !== undefined) {
          state.busy = false
          state.interrupted = true
          state.runID = latest.agent.run_id

          await recoverState(state)
          if (!isCurrentRequest()) return
        }
      }
    }

    let receiptInHistory = false

    if (state?.receipt !== undefined) {
      const receipt = state.receipt

      const userMessage = result.find(
          (message) =>
              message.id === receipt.user_message_id &&
              message.role === 'user' &&
              (
                  state.runID === undefined ||
                  message.agent_run_id === state.runID
              ),
      )

      const assistantMessage = result.find(
          (message) =>
              message.id === receipt.assistant_message_id &&
              message.role === 'assistant' &&
              (
                  state.runID === undefined ||
                  message.agent_run_id === state.runID
              ),
      )

      receiptInHistory =
          userMessage !== undefined &&
          assistantMessage !== undefined

      if (!receiptInHistory) {
        state.interrupted = true
        state.recoveryResolved = false
        throw new Error('保存回执与当前历史消息尚未对应')
      }
    }

    if (
        currentStream.value !== null &&
        (
            receiptInHistory ||
            state?.recoveryResolved === true
        )
    ) {
      streamTurn.value = null
    }

    historyReady.value = true
  } catch (error) {
    if (!isCurrentRequest()) return

    const text = await handleRequestError(
        error,
        '历史与恢复结果尚未核对完成，请再次刷新',
    )

    if (text !== null && isCurrentRequest()) {
      messageError.value = text
    }
  } finally {
    releaseController(controller)

    if (messageController === controller) {
      messageController = null
    }

    if (isCurrentPage() && requestID === messageRequestID) {
      loadingMessages.value = false
    }
  }
}

async function sendMessage() {
  const conversationID = selectedID.value
  const content = draft.value.trim()

  if (
      !isCurrentPage() ||
      conversationID === null ||
      !historyReady.value ||
      sending.value ||
      loadingMessages.value
  ) {
    return
  }

  const previous = agentStates.value[conversationID]
  if (previous !== undefined && blocksNewMessage(previous)) {
    ElMessage.warning('请先处理确认卡片或核对上一轮执行结果')
    return
  }

  if (currentStream.value !== null) {
    ElMessage.warning('请先刷新消息确认上一轮结果，再发送新消息')
    return
  }

  if (!content || [...content].length > 10000) {
    ElMessage.warning('请输入 1～10000 个字符的消息')
    return
  }

  try {
    writeRecoveryCheckpoint(pageUserID, conversationID)
  } catch {
    ElMessage.error('无法保存恢复定位信息，本次消息尚未发送')
    return
  }

  agentStates.value[conversationID] = createAgentState()
  const state = agentStates.value[conversationID]
  if (state === undefined) return

  await runAgentMessage(
      conversationID,
      { content },
      content,
      state,
      true,
  )
}

async function confirmCurrentAction() {
  const conversationID = selectedID.value
  const previous = currentAgent.value

  if (
      !isCurrentPage() ||
      conversationID === null ||
      !historyReady.value ||
      previous === undefined ||
      sending.value ||
      loadingMessages.value
  ) {
    return
  }

  const previousPhase = previous.operation?.phase
  const confirmed = beginConfirmation(previous)
  if (confirmed === null) {
    ElMessage.warning('当前确认尚不可提交，请检查有效期与执行状态')
    return
  }

  try {
    writeRecoveryCheckpoint(
        pageUserID,
        conversationID,
        undefined,
        confirmed.idempotency_key,
    )
  } catch {
    if (
        previous.operation !== undefined &&
        previousPhase !== undefined
    ) {
      previous.operation.phase = previousPhase
    }

    ElMessage.error('无法保存恢复定位信息，本次确认尚未发送')
    return
  }

  // 新确认执行使用新的 Run 状态，但沿用原确认操作与原 Key。
  agentStates.value[conversationID] = createAgentState(previous.operation)
  const state = agentStates.value[conversationID]
  if (state === undefined) return

  await runAgentMessage(
      conversationID,
      { confirmed_action: confirmed },
      confirmed.intake_mode === 'manual_review'
          ? '确认提交本地人工复核申请'
          : '确认创建本地工单',
      state,
      false,
  )
}

function dismissCurrentAction() {
  if (
      !isCurrentPage() ||
      sending.value ||
      loadingMessages.value
  ) {
    return
  }

  const state = currentAgent.value
  if (state !== undefined) {
    dismissConfirmation(state)
  }
}

async function runAgentMessage(
    conversationID: number,
    request: AgentMessageRequest,
    question: string,
    state: AgentUIState,
    clearDraft: boolean,
) {
  const controller = newController()
  streamController = controller
  sending.value = true
  messageError.value = ''
  completionNotice.value = ''

  streamTurn.value = {
    conversationID,
    question,
    answer: '',
    citations: [],
    status: 'waiting',
    error: '',
  }

  const turn = streamTurn.value

  function isCurrentStream(): boolean {
    return (
        isCurrentPage() &&
        streamController === controller &&
        selectedID.value === conversationID &&
        streamTurn.value === turn &&
        agentStates.value[conversationID] === state
    )
  }

  let persistedRunID: string | undefined

  function persistRunID() {
    if (
        state.runID === undefined ||
        state.runID === persistedRunID
    ) {
      return
    }

    writeRecoveryCheckpoint(
        pageUserID,
        conversationID,
        state.runID,
        request.confirmed_action?.idempotency_key,
    )

    persistedRunID = state.runID
  }

  try {
    await streamAgentMessageAPI(conversationID, request, {
      signal: controller.signal,

      onEvent(event) {
        if (!isCurrentStream() || controller.signal.aborted) return

        try {
          applyAgentEvent(state, event)
        } finally {
          // 即使后续工具状态校验失败，也保留已取得的 Run ID。
          persistRunID()
        }

        switch (event.event) {
          case 'start':
            turn.status = 'streaming'
            break

          case 'token':
            turn.answer += event.data.content
            break

          case 'citation':
            turn.citations = event.data.items
            break

          case 'agent_start':
          case 'tool_start':
          case 'tool_end':
          case 'action_required':
          case 'agent_result':
          case 'error':
          case 'done':
            break
        }
      },
    })

    if (!isCurrentStream()) return

    finishAgentStream(state)
    turn.status = 'completed'

    completionNotice.value =
        state.operation?.phase === 'ready'
            ? '回答已保存，请核对确认卡片'
            : state.businessEffect === 'committed'
                ? '已收到工单提交结果，消息已保存'
                : '回答已完成并保存'

    // 提交确认不会删除用户另行输入的咨询草稿。
    if (clearDraft) {
      drafts.delete(conversationID)
      draft.value = ''
    }

    await loadMessages()
  } catch (error) {
    if (!isCurrentStream()) return

    let displayError: unknown = error

    // 非 2xx HTTP 错误没有经过 onEvent，也要读取其中的业务事实。
    if (
        error instanceof ChatStreamRequestError &&
        error.details !== undefined
    ) {
      try {
        applyAgentError(state, error.details)
      } catch (protocolError) {
        displayError = protocolError
      }
    }

    try {
      persistRunID()
    } catch {
      recoveryNotice.value =
          '恢复标识暂时只能保留在当前页面，请保持页面打开并核对结果'
    }

    interruptAgentStream(state)

    // agent_result 可能已经到达，但 token 尚未发送就断开。
    if (turn.answer === '' && state.result !== undefined) {
      turn.answer = state.result.content
      turn.citations = state.result.citations
    }

    if (isCanceled(error)) {
      turn.status = 'stopped'
      return
    }

    turn.status = 'failed'

    const text = await handleRequestError(
        displayError,
        '回答连接异常，执行结果尚待核对',
    )

    if (text !== null && isCurrentStream()) {
      turn.error = `${text}。请先核对恢复结果，不要重复提交。`
    }
  } finally {
    releaseController(controller)

    if (streamController === controller) {
      streamController = null

      if (isCurrentPage()) {
        sending.value = false

        if (
            state.interrupted &&
            selectedID.value === conversationID
        ) {
          void loadMessages()
        }
      }
    }
  }
}

function stopGenerating() {
  if (!isCurrentPage() || !sending.value) return

  const turn = currentStream.value
  if (
      turn === null ||
      turn.status === 'completed' ||
      turn.status === 'stopped' ||
      turn.status === 'failed'
  ) {
    return
  }

  turn.status = 'stopped'

  const state = currentAgent.value
  if (state !== undefined && state.busy) {
    interruptAgentStream(state)
  }

  streamController?.abort()
}

function selectConversation(id: number) {
  if (!isCurrentPage()) return
  selectedID.value = id
}

async function handleLogout() {
  stopPage()
  auth.logout()
  await router.replace('/login')
}

watch(selectedID, (nextID, previousID) => {
  if (!isCurrentPage()) return

  if (previousID !== null) {
    const previous = agentStates.value[previousID]
    if (previous !== undefined && previous.busy) {
      interruptAgentStream(previous)
    }
  }

  streamController?.abort()
  streamController = null
  sending.value = false
  streamTurn.value = null
  completionNotice.value = ''
  historyReady.value = false
  recoveryNotice.value = ''

  if (previousID !== null) {
    drafts.set(previousID, draft.value)
  }

  draft.value = nextID === null ? '' : drafts.get(nextID) ?? ''
  messages.value = []

  void loadMessages()
})

watch(
    () => auth.revision,
    () => {
      if (auth.revision !== pageRevision) {
        stopPage()
      }
    },
    { flush: 'sync' },
)

onMounted(() => {
  void loadConversations()
})

onBeforeUnmount(stopPage)
</script>

<template>
  <main class="chat-page">
    <header class="chat-header">
      <div>
        <h1>规则智能客服</h1>
        <span>当前用户：{{ auth.user?.username }}</span>
      </div>

      <el-button @click="handleLogout">
        退出登录
      </el-button>
    </header>

    <div class="chat-layout">
      <aside class="sidebar">
        <el-card>
          <template #header>
            <div class="section-header">
              <strong>我的会话</strong>

              <el-button
                  link
                  type="primary"
                  :disabled="loadingList || creating"
                  @click="loadConversations"
              >
                刷新
              </el-button>
            </div>
          </template>

          <el-form @submit.prevent="createConversation">
            <el-input
                v-model="title"
                placeholder="例如：账号申诉咨询"
                :disabled="creating || loadingList"
            />

            <el-button
                class="create-button"
                type="primary"
                native-type="submit"
                :loading="creating"
                :disabled="creating || loadingList"
            >
              新建会话
            </el-button>
          </el-form>

          <p v-if="loadingList" role="status">正在加载会话……</p>

          <el-alert
              v-if="listError"
              :title="listError"
              type="error"
              :closable="false"
              show-icon
          />

          <el-empty
              v-if="!loadingList && !listError && conversations.length === 0"
              description="暂无会话，请先创建"
              :image-size="70"
          />

          <ul class="conversation-list">
            <li v-for="item in conversations" :key="item.id">
              <button
                  type="button"
                  class="conversation-item"
                  :class="{ active: item.id === selectedID }"
                  :aria-pressed="item.id === selectedID"
                  @click="selectConversation(item.id)"
              >
                {{ item.title }}
              </button>
            </li>
          </ul>
        </el-card>
      </aside>

      <section class="conversation-panel">
        <el-card>
          <template v-if="selectedConversation">
            <div class="section-header">
              <h2>{{ selectedConversation.title }}</h2>

              <el-button
                  link
                  type="primary"
                  :disabled="loadingMessages || sending"
                  @click="loadMessages"
              >
                刷新消息
              </el-button>
            </div>

            <p v-if="loadingMessages" role="status">
              正在加载消息……
            </p>
            <p v-if="completionNotice" class="stream-status" role="status">
              {{ completionNotice }}
            </p>

            <p v-if="recoveryNotice" class="stream-status" role="status">
              {{ recoveryNotice }}
            </p>

            <el-alert
                v-if="messageError"
                :title="messageError"
                type="error"
                :closable="false"
                show-icon
            />

            <el-empty
                v-if="!loadingMessages &&!messageError &&messages.length === 0 &&!currentStream"
                description="还没有消息，发送第一条咨询吧"
                :image-size="70"
            />
            <AgentPanel
                v-if="currentAgent"
                :state="currentAgent"
                :disabled="sending || loadingMessages || !historyReady"
                @confirm="confirmCurrentAction"
                @dismiss="dismissCurrentAction"
            />

            <ul class="message-list">
              <li
                  v-for="message in messages"
                  :key="message.id"
                  class="message-item"
                  :class="{ 'from-user': message.role === 'user' }"
              >
                <div class="message-role">
                  {{ message.role === 'user' ? '我' : '客服' }}
                </div>

                <div class="message-content">{{ message.content }}</div>
                <p
                    v-if="message.role === 'assistant' && message.agent"
                    class="stream-status"
                >
                  {{
                    message.agent.business_effect === 'committed'
                        ? '此轮记录：工单已提交'
                        : message.agent.business_effect === 'unknown'
                            ? '此轮记录：工单提交结果待核对'
                            : '此轮记录：未报告工单提交'
                  }}
                  ·
                  {{
                    message.agent.message_save_status === 'saved'
                        ? '消息已保存'
                        : '消息保存状态待核对'
                  }}
                </p>
                <RuleCitations
                    v-if="message.role === 'assistant'"
                    :items="message.citations ?? []"
                    @unauthorized="handleRequestError($event, '无法读取引用条款')"
                />
              </li>
              <template v-if="currentStream">
                <li class="message-item from-user">
                  <div class="message-role">我</div>
                  <div class="message-content">{{ currentStream.question }}</div>
                </li>

                <li
                    class="message-item"
                    :aria-busy="
        currentStream.status === 'waiting' ||
        currentStream.status === 'streaming'
      "
                >
                  <div class="message-role">客服</div>

                  <div
                      v-if="currentStream.answer"
                      class="message-content"
                  >{{ currentStream.answer }}</div>

                  <RuleCitations
                      :items="currentStream.citations"
                      @unauthorized="handleRequestError($event, '无法读取引用条款')"
                  />

                  <p class="stream-status" role="status">
                    {{
                      currentStream.status === 'streaming' && currentStream.answer
                          ? '已收到回答内容，正在等待完成确认……'
                          : streamStatusText[currentStream.status]
                    }}
                  </p>

                  <el-alert
                      v-if="currentStream.error"
                      :title="currentStream.error"
                      type="error"
                      :closable="false"
                      show-icon
                  />
                </li>
              </template>
            </ul>

            <el-form @submit.prevent="sendMessage">
              <el-input
                  v-model="draft"
                  type="textarea"
                  :rows="4"
                  placeholder="请输入咨询内容"
                  :disabled="
                     sending ||
                     loadingMessages ||
                     !historyReady ||
                     agentBlocksSending
                  "
              />

              <div class="send-actions">
                <el-button
                    type="primary"
                    native-type="submit"
                    :loading="sending"
                    :disabled="sending || loadingMessages || agentBlocksSending || !draft.trim()"
                >
                  发送
                </el-button>
                <el-button
                    v-if="sending &&currentStream &&(currentStream.status === 'waiting' || currentStream.status === 'streaming')"
                    native-type="button"
                    @click="stopGenerating"
                >
                  停止生成
                </el-button>
              </div>
            </el-form>
          </template>

          <el-empty
              v-else
              description="请选择或创建一个会话"
          />
        </el-card>
      </section>
    </div>
  </main>
</template>

<style scoped>
.chat-page {
  max-width: 1200px;
  margin: 0 auto;
  padding: 24px;
}

.chat-header,
.section-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 16px;
}

.chat-header {
  margin-bottom: 24px;
}

.chat-header h1 {
  margin: 0 0 8px;
}

.chat-layout {
  display: grid;
  grid-template-columns: 300px minmax(0, 1fr);
  gap: 20px;
}

.sidebar,
.conversation-panel {
  min-width: 0;
}

.create-button {
  width: 100%;
  margin: 12px 0 16px;
}

.conversation-list {
  list-style: none;
  padding: 0;
  margin: 16px 0 0;
  max-height: 60vh;
  overflow-y: auto;
}

.conversation-item {
  display: block;
  width: 100%;
  margin-bottom: 8px;
  padding: 12px;
  border: 1px solid #dcdfe6;
  border-radius: 6px;
  background: white;
  color: #303133;
  text-align: left;
  overflow-wrap: anywhere;
  cursor: pointer;
  font: inherit;
}

.conversation-item.active {
  border-color: #409eff;
  background: #ecf5ff;
  color: #337ecc;
}

.conversation-panel h2 {
  overflow-wrap: anywhere;
}

@media (max-width: 760px) {
  .chat-layout {
    grid-template-columns: 1fr;
  }
}

.message-list {
  list-style: none;
  padding: 0;
  margin: 20px 0;
  max-height: 45vh;
  overflow-y: auto;
}

.message-item {
  width: fit-content;
  max-width: 85%;
  margin: 0 0 16px;
  padding: 12px 16px;
  border-radius: 10px;
  background: #f0f2f5;
}

.message-item.from-user {
  margin-left: auto;
  background: #ecf5ff;
}

.message-role {
  margin-bottom: 6px;
  color: #606266;
  font-size: 12px;
}

.message-content {
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}

.send-actions {
  display: flex;
  justify-content: flex-end;
  margin-top: 12px;
}

.stream-status {
  margin: 8px 0;
  color: #606266;
  font-size: 12px;
}
</style>