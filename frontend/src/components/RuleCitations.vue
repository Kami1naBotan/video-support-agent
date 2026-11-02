<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'

import { getKnowledgeArticleAPI } from '../api/knowledge-article'
import type { KnowledgeArticleDetail } from '../api/knowledge-article'
import { officialSourceHref } from '../api/rule-citation'
import type { RuleCitation } from '../api/rule-citation'
import {
  errorMessage,
  isCanceled,
  isUnauthorized,
} from '../api/errors'
import { useAuthStore } from '../stores/auth'

const props = defineProps<{
  items: RuleCitation[]
}>()

const emit = defineEmits<{
  unauthorized: [error: unknown]
}>()

const auth = useAuthStore()
const selected = ref<RuleCitation | null>(null)
const detail = ref<KnowledgeArticleDetail | null>(null)
const loading = ref(false)
const errorText = ref('')
const receivedAt = ref('')

let activeController: AbortController | null = null
let requestID = 0
let disposed = false

function reset() {
  requestID++
  activeController?.abort()
  activeController = null
  selected.value = null
  detail.value = null
  loading.value = false
  errorText.value = ''
  receivedAt.value = ''
}

async function loadDetail(item: RuleCitation) {
  if (disposed) return

  // 新查询开始时清除旧结果，不能让旧的有效状态继续显示。
  reset()

  const controller = new AbortController()
  const expectedRequestID = requestID
  const expectedAuthRevision = auth.revision

  activeController = controller
  selected.value = item
  loading.value = true

  function isCurrent(): boolean {
    return (
        !disposed &&
        !controller.signal.aborted &&
        activeController === controller &&
        requestID === expectedRequestID &&
        auth.revision === expectedAuthRevision
    )
  }

  try {
    const result = await getKnowledgeArticleAPI(
        item,
        controller.signal,
    )

    if (!isCurrent()) return

    detail.value = result
    receivedAt.value = new Date().toISOString()
  } catch (error) {
    if (!isCurrent() || isCanceled(error)) return

    if (isUnauthorized(error)) {
      reset()
      emit('unauthorized', error)
      return
    }

    errorText.value = errorMessage(error, '读取历史条款失败')
  } finally {
    if (isCurrent()) {
      loading.value = false
      activeController = null
    }
  }
}

function formatTime(value: string): string {
  const time = new Date(value)
  return Number.isFinite(time.getTime())
      ? time.toLocaleString()
      : value
}

const stateLabels = computed(() => {
  if (detail.value === null) return []

  const state = detail.value.state
  const labels = [
    state.is_current_effective
        ? '查询时为当前有效版本'
        : '查询时未标记为当前有效版本',
  ]

  if (state.is_current) labels.push('当前选定版本')
  if (state.is_superseded) labels.push('已被替代')
  if (state.is_expired) labels.push('已过期')
  if (state.is_withdrawn) labels.push('已下线')

  return labels
})

watch(
    () => props.items,
    reset,
    { deep: true, flush: 'sync' },
)

watch(
    () => auth.revision,
    reset,
    { flush: 'sync' },
)

onBeforeUnmount(() => {
  disposed = true
  reset()
})
</script>

<template>
  <section
      v-if="items.length > 0"
      class="rule-citations"
      aria-label="回答引用"
  >
    <p class="citation-heading">回答引用</p>
    <p class="citation-note">
      以下为回答时保存的引用快照。规则是否仍有效，请查询条款状态。
    </p>

    <ul class="citation-list">
      <li v-for="item in items" :key="item.ref_id">
        <strong>[{{ item.ref_id }}] {{ item.title }}</strong>

        <p>
          版本：{{ item.version }}；
          原条款编号：{{ item.article_number.trim() || '原文未编号' }}
        </p>

        <p v-if="item.section_path">
          章节：{{ item.section_path }}
        </p>

        <p>
          生效时间：{{ formatTime(item.effective_at) }}；
          失效时间：
          {{ item.expires_at === null
            ? '未注明固定截止日期'
            : formatTime(item.expires_at) }}
        </p>

        <p>检索时间：{{ formatTime(item.retrieved_at) }}</p>

        <div class="citation-actions">
          <a
              v-if="officialSourceHref(item.source_url)"
              :href="officialSourceHref(item.source_url) ?? undefined"
              target="_blank"
              rel="noopener noreferrer"
          >
            打开官方来源（新窗口）
          </a>
          <span v-else>来源链接不可用</span>

          <button type="button" @click="loadDetail(item)">
            查看条款与查询状态
          </button>
        </div>

        <p
            v-if="selected?.ref_id !== item.ref_id"
            class="citation-note"
        >
          当前状态未核实
        </p>
      </li>
    </ul>

    <section
        v-if="selected"
        class="article-detail"
        aria-label="引用条款详情"
        :aria-busy="loading"
    >
      <div class="detail-heading">
        <strong>[{{ selected.ref_id }}] 条款详情</strong>
        <div class="citation-actions">
          <button
              type="button"
              :disabled="loading"
              @click="loadDetail(selected)"
          >
            重新查询
          </button>
          <button type="button" @click="reset">
            关闭
          </button>
        </div>
      </div>

      <p v-if="loading" role="status">
        正在查询；当前状态未核实。
      </p>

      <p v-else-if="errorText" role="alert">
        {{ errorText }}。当前状态未核实。
      </p>

      <template v-else-if="detail">
        <p role="status">
          本次查询返回：{{ stateLabels.join('；') }}
        </p>
        <p class="citation-note">
          响应接收时间：{{ formatTime(receivedAt) }}。
          状态可能随后变化，需要时请重新查询。
        </p>
        <p v-if="detail.section_path">
          章节：{{ detail.section_path }}
        </p>
        <p>
          原条款编号：
          {{ detail.article_number.trim() || '原文未编号' }}
        </p>
        <div class="article-content">{{ detail.content }}</div>
      </template>

      <p v-else>当前状态未核实</p>
    </section>
  </section>
</template>

<style scoped>
.rule-citations {
  margin-top: 16px;
  padding-top: 12px;
  border-top: 1px solid #dcdfe6;
  overflow-wrap: anywhere;
}

.citation-heading {
  font-weight: 600;
}

.citation-note {
  color: #606266;
  font-size: 13px;
}

.citation-list {
  margin: 12px 0 0;
  padding-left: 20px;
}

.citation-list > li {
  margin-bottom: 16px;
}

.citation-list p,
.article-detail p {
  margin: 6px 0;
}

.citation-actions,
.detail-heading {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 12px;
}

.detail-heading {
  justify-content: space-between;
}

.citation-actions button {
  padding: 5px 10px;
  cursor: pointer;
}

.citation-actions button:disabled {
  cursor: default;
}

.article-detail {
  margin-top: 12px;
  padding: 12px;
  border: 1px solid #dcdfe6;
  border-radius: 6px;
}

.article-content {
  margin-top: 12px;
  max-height: 360px;
  overflow: auto;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  line-height: 1.7;
}
</style>