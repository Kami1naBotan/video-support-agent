<script setup lang="ts">
import { onBeforeUnmount, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'

import { useAuthStore } from '../stores/auth'
import {
  errorMessage as getErrorMessage,
  isCanceled,
  isUnauthorized,
} from '../api/errors'

const router = useRouter()
const auth = useAuthStore()

const form = reactive({
  username: '',
  password: '',
})

const loading = ref(false)
const errorMessage = ref('')

let disposed = false
let loginRevision: number | null = null

onBeforeUnmount(() => {
  disposed = true

  if (loading.value && !auth.user && loginRevision !== null) {
    auth.invalidateIfCurrent(loginRevision)
  }

  form.password = ''
})

async function submitLogin() {
  if (loading.value) return

  errorMessage.value = ''

  if (!form.username.trim() || !form.password) {
    errorMessage.value = '请输入用户名和密码'
    return
  }

  loading.value = true

  try {
    const pending = auth.login({
      username: form.username.trim(),
      password: form.password,
    })

    loginRevision = auth.revision
    await pending

    if (disposed) return

    form.password = ''
    ElMessage.success('登录成功')
    await router.replace('/chat')
  } catch (error) {
    if (disposed || isCanceled(error)) {
      return
    }

    errorMessage.value = isUnauthorized(error)
        ? '用户名、密码错误，或登录凭证无效'
        : getErrorMessage(error, '登录失败，请稍后重试')
  } finally {
    if (!disposed) {
      loading.value = false
    }
  }
}
</script>

<template>
  <main class="page">
    <el-card class="login-card">
      <h1>规则智能客服</h1>
      <p>请使用已有测试账号登录。</p>

      <el-form
          :model="form"
          label-position="top"
          @submit.prevent="submitLogin"
      >
        <el-form-item label="用户名">
          <el-input
              v-model="form.username"
              autocomplete="username"
              :disabled="loading"
          />
        </el-form-item>

        <el-form-item label="密码">
          <el-input
              v-model="form.password"
              type="password"
              show-password
              autocomplete="current-password"
              :disabled="loading"
          />
        </el-form-item>

        <el-alert
            v-if="errorMessage"
            :title="errorMessage"
            type="error"
            :closable="false"
            show-icon
        />

        <el-button
            class="login-button"
            type="primary"
            native-type="submit"
            :loading="loading"
            :disabled="loading"
        >
          登录
        </el-button>
      </el-form>
    </el-card>
  </main>
</template>

<style scoped>
.login-card {
  max-width: 420px;
  margin: 48px auto;
}

.login-button {
  width: 100%;
  margin-top: 20px;
}
</style>