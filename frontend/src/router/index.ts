import { createRouter, createWebHistory } from 'vue-router'
import { ElMessage } from 'element-plus'

import { useAuthStore } from '../stores/auth'
import { isCanceled } from '../api/errors'

const router = createRouter({
    history: createWebHistory(),
    routes: [
        {
            path: '/',
            redirect: '/chat',
        },
        {
            path: '/login',
            name: 'login',
            component: () => import('../views/LoginView.vue'),
        },
        {
            path: '/chat',
            name: 'chat',
            component: () => import('../views/ChatView.vue'),
            meta: { requiresAuth: true },
        },
    ],
})

router.beforeEach(async (to) => {
    const auth = useAuthStore()

    if (!to.meta.requiresAuth) {
        return true
    }

    try {
        await auth.restoreSession()
    } catch (error) {
        if (isCanceled(error)) {
            return false
        }

        ElMessage.error('暂时无法验证登录状态，请检查后端连接后重试')
        return { name: 'login' }
    }

    if (!auth.user) {
        return { name: 'login' }
    }

    return true
})

export default router