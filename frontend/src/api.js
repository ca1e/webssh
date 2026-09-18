// 后端 API 封装:统一 JSON 请求、{error} 错误契约与 WebSocket 地址。

import { reactive } from 'vue'

// 启动密码锁的全局状态:任意请求收到 401 即置 locked,由 App.vue 渲染锁屏。
export const authState = reactive({ locked: false, hasPassword: true })

// opts 透传 fetch 附加选项(如 signal,用于取消连接中的会话请求)。
async function request(method, url, body, opts = {}) {
  const reqOpts = { method, headers: {}, ...opts }
  if (body !== undefined) {
    reqOpts.headers['Content-Type'] = 'application/json'
    reqOpts.body = JSON.stringify(body)
  }
  let res
  try {
    res = await fetch(url, reqOpts)
  } catch (e) {
    // 用户主动取消(signal 中止)时保留 AbortError 语义,供调用方区分
    if (e?.name === 'AbortError') throw e
    throw new Error('无法连接后端服务')
  }
  let data = null
  try {
    data = await res.json()
  } catch {
    /* 空响应体 */
  }
  if (!res.ok) {
    if (res.status === 401 && url !== '/api/auth/unlock' && url !== '/api/auth/status') {
      authState.locked = true
    }
    throw new Error((data && data.error) || `请求失败(HTTP ${res.status})`)
  }
  return data
}

const get = (url) => request('GET', url)
const post = (url, body, opts) => request('POST', url, body, opts)
const put = (url, body, opts) => request('PUT', url, body, opts)
const del = (url) => request('DELETE', url)

// ---------- 启动密码 ----------

export const authStatus = () => get('/api/auth/status')
export const authSetup = (password) => post('/api/auth/setup', { password })
export const authUnlock = (password) => post('/api/auth/unlock', { password })
export const authLogout = () => post('/api/auth/logout')
export const authChangePassword = (currentPassword, newPassword) =>
  post('/api/auth/change', { current_password: currentPassword, new_password: newPassword })

// ---------- 主机 ----------

export const listHosts = () => get('/api/hosts')
export const createHost = (h) => post('/api/hosts', h)
export const updateHost = (id, h) => put(`/api/hosts/${id}`, h)
export const deleteHost = (id) => del(`/api/hosts/${id}`)
export const duplicateHost = (id) => post(`/api/hosts/${id}/duplicate`)
export const testHost = (h) => post('/api/hosts/test', h)
export const importHosts = (hosts) => post('/api/hosts/import', hosts)
export const listKeys = () => get('/api/keys')

// 导出主机列表:返回 hosts.json 原文 Blob(不走 JSON 解析,保留文件字节)
export async function fetchHostsFile() {
  let res
  try {
    res = await fetch('/api/hosts/export')
  } catch {
    throw new Error('无法连接后端服务')
  }
  if (!res.ok) {
    if (res.status === 401) authState.locked = true
    let msg = ''
    try {
      msg = (await res.json())?.error || ''
    } catch {
      /* 非 JSON 错误体 */
    }
    throw new Error(msg || `导出失败(HTTP ${res.status})`)
  }
  return res.blob()
}

// ---------- 会话 ----------

export const openSession = (hostId, trustNewKey = false, signal) =>
  post('/api/sessions', { host_id: hostId, trust_new_key: trustNewKey }, { signal })
export const closeSession = (id) => del(`/api/sessions/${id}`)
export const listSessions = () => get('/api/sessions')

export function terminalWSUrl(sessionId, cols, rows) {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  return `${proto}://${location.host}/api/sessions/${sessionId}/ws?cols=${cols}&rows=${rows}`
}

// 当前跟踪到的远程 shell 工作目录(会话 → SFTP 单向同步)
export const sessionCwd = (sid) => get(`/api/sessions/${sid}/cwd`)

// ---------- SFTP / 本机文件 ----------

export const sftpList = (sid, path) =>
  get(`/api/sessions/${sid}/sftp/ls?path=${encodeURIComponent(path)}`)
export const sftpDelete = (sid, path) =>
  del(`/api/sessions/${sid}/sftp?path=${encodeURIComponent(path)}`)
export const sftpMkdir = (sid, path) =>
  post(`/api/sessions/${sid}/sftp/mkdir`, { path })
export const sftpRename = (sid, path, newName) =>
  post(`/api/sessions/${sid}/sftp/rename`, { path, new_name: newName })

export const localList = (path) => get(`/api/local/ls?path=${encodeURIComponent(path)}`)
export const localDelete = (path) => del(`/api/local?path=${encodeURIComponent(path)}`)
export const localMkdir = (path) => post('/api/local/mkdir', { path })
export const localRename = (path, newName) => post('/api/local/rename', { path, new_name: newName })

// ---------- 统一文件传输(本机↔远程、远程↔远程,异步任务 + WS 进度) ----------

export const createTransfer = (payload) => post('/api/transfers', payload)
export const listTransfers = () => get('/api/transfers')
export const cancelTransfer = (id) => post(`/api/transfers/${id}/cancel`)
export const deleteTransfer = (id) => del(`/api/transfers/${id}`)
export function transfersWSUrl() {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  return `${proto}://${location.host}/api/transfers/ws`
}

// ---------- 主机详情 ----------
export const hostInfo = (sid) => get(`/api/sessions/${sid}/host-info`)
export const metricsWSUrl = (sid) => {
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  return `${proto}://${location.host}/api/sessions/${sid}/metrics/ws`
}

// ---------- Docker ----------
export const dockerContainers = (sid) => get(`/api/sessions/${sid}/docker/containers`)
// 版本 + Compose 项目卡片:低频变化,仅打开面板与手动刷新时拉取
export const dockerInfo = (sid) => get(`/api/sessions/${sid}/docker/info`)
export const dockerInspect = (sid, cid) => get(`/api/sessions/${sid}/docker/inspect?cid=${encodeURIComponent(cid)}`)

// ---------- 转发 ----------

export const listForwards = (sid) => get(`/api/sessions/${sid}/forwards`)
export const addForward = (sid, payload) => post(`/api/sessions/${sid}/forwards`, payload)
export const deleteForward = (sid, fid) => del(`/api/sessions/${sid}/forwards/${fid}`)
export const k8sResources = (sid, namespace) =>
  get(`/api/sessions/${sid}/k8s/resources${namespace ? `?namespace=${encodeURIComponent(namespace)}` : ''}`)

// ---------- K8s 面板(集群状态;Pod watch/日志/终端走 WS,URL 在组件内构造) ----------

export const k8sState = (sid, refresh = false) =>
  get(`/api/sessions/${sid}/k8s/state${refresh ? '?refresh=1' : ''}`)

// ---------- 工具 ----------

export function joinPath(dir, name) {
  if (!dir || dir === '/') return '/' + name
  return dir.replace(/\/+$/, '') + '/' + name
}

export function fmtSize(size, isDir) {
  if (isDir) return '-'
  if (size < 1024) return size + ' B'
  const units = ['KB', 'MB', 'GB', 'TB']
  let v = size
  let i = -1
  do {
    v /= 1024
    i++
  } while (v >= 1024 && i < units.length - 1)
  return v.toFixed(v >= 100 ? 0 : 1) + ' ' + units[i]
}
