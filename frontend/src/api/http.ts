import axios from 'axios'
import { readToken } from '../auth/token'

export const http = axios.create({
    baseURL: '/api/v1',
    timeout: 60000,
})

http.interceptors.request.use((config) => {
    const token = readToken()

    if (token) {
        config.headers.set('Authorization', `Bearer ${token}`)
    }

    return config
})