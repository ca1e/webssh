<script setup>
import { nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { Refresh } from '@element-plus/icons-vue'
import { t } from '../i18n'

const props = defineProps({
  wsUrl: { type: String, required: true },
  name: { type: String, default: '' },
  host: { type: String, default: '' },
})

const logBody = ref(null)
const autoScroll = ref(true)
const connected = ref(false)
const logLineCount = ref(0)

let ws = null
let reconnectTimer = null
let disposed = false
let pendingText = ''

function appendLog(text) {
  const el = logBody.value
  if (!el) return
  const wasAtBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 40
  // 用 textNode 追加,避免 innerHTML 开销
  el.appendChild(document.createTextNode(text))
  // 清理过多 DOM 节点(保留最新)
  while (el.childNodes.length > 5000) {
    el.removeChild(el.firstChild)
  }
  logLineCount.value = el.childNodes.length
  if ((autoScroll.value || wasAtBottom) && el.scrollHeight > el.clientHeight) {
    // requestAnimationFrame 确保在下一帧渲染后再滚动
    requestAnimationFrame(() => {
      el.scrollTop = el.scrollHeight
    })
  }
}

function connect() {
  if (disposed) return
  connected.value = false
  try {
    ws = new WebSocket(props.wsUrl)
    ws.binaryType = 'arraybuffer'
  } catch {
    scheduleReconnect()
    return
  }
  ws.onopen = () => { connected.value = true }
  ws.onmessage = (ev) => {
    if (ev.data instanceof ArrayBuffer) {
      appendLog(new TextDecoder().decode(ev.data))
    } else if (typeof ev.data === 'string') {
      appendLog(ev.data)
    }
  }
  ws.onclose = () => {
    connected.value = false
    if (!disposed) scheduleReconnect()
  }
  ws.onerror = () => {}
}

function scheduleReconnect() {
  if (disposed || reconnectTimer) return
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null
    connect()
  }, 3000)
}

function clearLogs() {
  if (logBody.value) {
    logBody.value.textContent = ''
    logLineCount.value = 0
  }
}

function manualReconnect() {
  if (ws) { ws.onclose = null; ws.close() }
  connect()
}

onMounted(connect)
onBeforeUnmount(() => {
  disposed = true
  if (reconnectTimer) clearTimeout(reconnectTimer)
  if (ws) { ws.onclose = null; ws.close() }
})
</script>

<template>
  <div class="container-logs-panel">
    <div class="panel-title">
      {{ t('logs.title') }} · {{ name }} <span class="muted" style="font-weight:400">({{ host }})</span>
      <span class="ws-dot" :class="connected ? 'open' : 'closed'"></span>
      <span class="muted" style="font-size: 11px">{{ connected ? t('logs.connected') : t('logs.disconnected') }}</span>
      <span style="flex: 1"></span>
      <span class="muted" style="font-size: 11px">{{ t('logs.lines', { n: logLineCount }) }}</span>
      <el-checkbox v-model="autoScroll" size="small" :title="t('logs.autoScrollTitle')">{{ t('logs.autoScroll') }}</el-checkbox>
      <el-button size="small" @click="clearLogs" :title="t('logs.clear')">{{ t('logs.clear') }}</el-button>
      <el-button size="small" :icon="Refresh" :title="t('logs.reconnect')" @click="manualReconnect" />
    </div>
    <div ref="logBody" class="log-body"></div>
  </div>
</template>
