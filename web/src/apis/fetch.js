import axios from 'axios'
import Cookies from 'js-cookie'
import { EXACT_IDS_HEADER, EXACT_IDS_VERSION, normalizeAPIParams, transformAPIRequest, transformAPIResponse } from '../utils/idTransport.js'
import { normalizeApiError } from '../utils/apiError.js'

const getURL = url => {
  // 开发环境使用 Vite 代理，生产环境使用相对路径
  const baseURL = '/'
  return { baseURL, url }
}

export const instance = axios.create({
  transformRequest: [transformAPIRequest, ...axios.defaults.transformRequest],
  transformResponse: [transformAPIResponse],
  headers: {
    'Content-Type': 'application/json',
  },
})

instance.interceptors.request.use(
  async config => {
    config.params = normalizeAPIParams(config.params)
    // Marks the frontend's validated identifier protocol even for bodyless
    // writes: the target ID may exist only in the URL. Stale UI bundles lack
    // this marker and the server can reject their unsafe-range mutations.
    if (['post', 'put', 'patch', 'delete'].includes(config.method?.toLowerCase())) {
      config.headers.set(EXACT_IDS_HEADER, EXACT_IDS_VERSION)
    }
    const token = Cookies.get('danmu_token')
    if (config.headers && !!token) {
      config.headers['Authorization'] = `Bearer ${token}`
    }
    return config
  },
  error => Promise.reject(error)
)

instance.interceptors.response.use(
  res => res,
  error => {
    if (axios.isCancel(error)) return Promise.reject(error)
    const requestUrl = error.config?.url || ''
    const isAuthFlowRequest = [
      '/api/ui/auth/token',
      '/api/ui/auth/auto-login',
      '/api/ui/auth/mfa/verify',
      '/api/ui/auth/mfa/passkey/login/options',
      '/api/ui/auth/mfa/passkey/login/verify',
    ].some(url => requestUrl.includes(url))

    // 401 未授权：自动清理 token 并跳转登录页
    // 登录流程接口交给登录页自己处理，避免旧 token/自动跳转干扰正常登录。
    if (error.response?.status === 401 && !isAuthFlowRequest) {
      Cookies.remove('danmu_token', { path: '/' })
      // 避免在登录页重复跳转
      if (typeof window !== 'undefined' && !window.location.pathname.includes('/login')) {
        window.location.href = '/login'
      }
    }

    return Promise.reject(normalizeApiError(error))
  }
)

const api = {
  get(url, data, other = { headers: {} }) {
    return instance({
      ...other,
      method: 'get',
      baseURL: getURL(url).baseURL,
      url: getURL(url).url,
      headers: { ...other.headers },
      params: data,
      onDownloadProgress: other.onDownloadProgress,
    })
  },
  post(url, data, other = { headers: {} }) {
    return instance({
      ...other,
      method: 'post',
      baseURL: getURL(url).baseURL,
      url: getURL(url).url,
      headers: { ...other.headers },
      data,
      // 同时支持上传和下载进度
      onUploadProgress: other.onUploadProgress,
      onDownloadProgress: other.onDownloadProgress,
    })
  },
  // patch/put/delete 与 post 类似，根据实际需求添加进度配置
  patch(url, data, other = { headers: {} }) {
    return instance({
      ...other,
      method: 'patch',
      baseURL: getURL(url).baseURL,
      url: getURL(url).url,
      headers: { ...other.headers },
      data,
      onUploadProgress: other.onUploadProgress,
      onDownloadProgress: other.onDownloadProgress,
    })
  },
  put(url, data, other = { headers: {} }) {
    return instance({
      ...other,
      method: 'put',
      baseURL: getURL(url).baseURL,
      url: getURL(url).url,
      headers: { ...other.headers },
      data,
      onUploadProgress: other.onUploadProgress,
      onDownloadProgress: other.onDownloadProgress,
    })
  },
  delete(url, data, other = { headers: {} }) {
    // 检查是否是config对象（包含params属性）
    const isConfig = data && typeof data === 'object' && (data.params || data.headers || data.data);
    if (isConfig) {
      return instance({
        ...other,
        ...data,
        method: 'delete',
        baseURL: getURL(url).baseURL,
        url: getURL(url).url,
        headers: { ...other.headers, ...(data.headers || {}) },
        params: data.params,
        data: data.data,
        onUploadProgress: data.onUploadProgress || other.onUploadProgress,
        onDownloadProgress: data.onDownloadProgress || other.onDownloadProgress,
      });
    } else {
      // 向后兼容：data作为请求体
      return instance({
        ...other,
        method: 'delete',
        baseURL: getURL(url).baseURL,
        url: getURL(url).url,
        headers: { ...other.headers },
        data,
        onUploadProgress: other.onUploadProgress,
        onDownloadProgress: other.onDownloadProgress,
      });
    }
  },
}

export default api
