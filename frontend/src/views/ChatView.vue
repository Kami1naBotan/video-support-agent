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
import { listMessagesAPI } from '../api/message'
import { streamMessageAPI } from '../api/chat-stream'
import type { ChatMessage } from '../api/message'
import {
  errorMessage,
  isCanceled,
  isUnauthorized,
} from '../api/errors'

const router = useRouter()
const auth = useAuthStore()
const pageRevision = auth.revision

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
  status: StreamStatus
  error: string
}

const streamTurn = ref<StreamTurn | null>(null)

const completionNotice = ref('')

const currentStream = computed(() => {
  const turn = streamTurn.value

  return turn?.conversationID === selectedID.value
      ? turn
      : null
})

const streamStatusText: Record<StreamStatus, string> = {
  waiting: '正在等待客服回答……',
  streaming: '正在生成回答……',
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
  return !disposed && auth.revision === pageRevision
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
  if (!isCurrentPage()) return

  messageController?.abort()

  const requestID = ++messageRequestID
  const conversationID = selectedID.value

  messageError.value = ''

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

  try {
    const result = await listMessagesAPI(
        conversationID,
        controller.signal,
    )

    if (!isCurrentRequest()) return

    messages.value = result

    if (
        currentStream.value !== null &&
        (
            !sending.value ||
            currentStream.value.status === 'completed'
        )
    ) {
      streamTurn.value = null
    }
  } catch (error) {
    if (!isCurrentRequest()) return

    const text = await handleRequestError(error, '加载消息失败')

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
      sending.value ||
      loadingMessages.value
  ) {
    return
  }

  // 上一轮展示尚未与历史记录同步，先确认服务端结果。
  if (currentStream.value !== null) {
    ElMessage.warning('请先刷新消息确认上一轮结果，再发送新消息')
    return
  }

  if (!content || [...content].length > 10000) {
    ElMessage.warning('请输入 1～10000 个字符的消息')
    return
  }

  const controller = newController()
  streamController = controller
  sending.value = true
  messageError.value = ''
  completionNotice.value = ''

  streamTurn.value = {
    conversationID,
    question: content,
    answer: '',
    status: 'waiting',
    error: '',
  }

  // 从 ref 中取出 Vue 的响应式对象，后续更新才能驱动页面。
  const turn = streamTurn.value

  function isCurrentStream(): boolean {
    return (
        isCurrentPage() &&
        streamController === controller &&
        selectedID.value === conversationID &&
        streamTurn.value === turn
    )
  }

  try {
    await streamMessageAPI(conversationID, content, {
      signal: controller.signal,

      onEvent(event) {
        if (!isCurrentStream() || controller.signal.aborted) return

        switch (event.event) {
          case 'start':
            turn.status = 'streaming'
            break

          case 'token':
            turn.answer += event.data.content
            break

          case 'citation':
            // 今天不实现 RAG，服务端返回空引用数组。
            break

          case 'error':
            // 请求层会将它转换为异常，由下面的 catch 统一处理。
            break

          case 'done':
            // 等请求层完成协议检查并正常返回，再确认完成状态。
            break
        }
      },
    })

    if (!isCurrentStream()) return

    turn.status = 'completed'
    completionNotice.value = '回答已完成并保存'
    drafts.delete(conversationID)
    draft.value = ''

    // 使用真实历史记录替换临时展示，获得数据库 ID 和时间。
    await loadMessages()
  } catch (error) {
    if (!isCurrentStream()) return

    if (isCanceled(error)) {
      turn.status = 'stopped'
      return
    }

    turn.status = 'failed'

    const text = await handleRequestError(
        error,
        '回答连接异常，请刷新消息确认结果',
    )

    // handleRequestError 内可能发生跳转，因此等待后再检查一次。
    if (text !== null && isCurrentStream()) {
      turn.error =
          `${text}。请刷新消息确认保存结果，避免重复发送。`
    }
  } finally {
    releaseController(controller)

    // 旧请求的 finally 不能清除新请求的控制器和加载状态。
    if (streamController === controller) {
      streamController = null

      if (isCurrentPage()) {
        sending.value = false
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

  streamController?.abort()
  streamController = null
  sending.value = false
  streamTurn.value = null
  completionNotice.value = ''

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

                  <p class="stream-status" role="status">
                    {{ streamStatusText[currentStream.status] }}
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
                  :disabled="sending || loadingMessages"
              />

              <div class="send-actions">
                <el-button
                    type="primary"
                    native-type="submit"
                    :loading="sending"
                    :disabled="sending || loadingMessages || !draft.trim()"
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