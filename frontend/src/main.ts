import { createApp } from 'vue'
import { createPinia } from 'pinia'
import {
    ElAlert,
    ElButton,
    ElCard,
    ElEmpty,
    ElForm,
    ElFormItem,
    ElInput,
} from 'element-plus'

import 'element-plus/dist/index.css'
import './style.css'

import App from './App.vue'
import router from './router'

const app = createApp(App)

app.use(createPinia())

app.component('ElAlert', ElAlert)
app.component('ElButton', ElButton)
app.component('ElCard', ElCard)
app.component('ElEmpty', ElEmpty)
app.component('ElForm', ElForm)
app.component('ElFormItem', ElFormItem)
app.component('ElInput', ElInput)

app.use(router)
app.mount('#app')