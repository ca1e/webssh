<script setup>
import { nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Close, Loading } from '@element-plus/icons-vue'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'
import { terminalWSUrl } from '../api'
import { emitCwd } from '../cwdBus'
import { useContextMenu } from '../contextMenu'
import { t } from '../i18n'

const props = defineProps({
  sid: { type: String, required: true },
  host: { type: String, default: '' },
  active: Boolean,
  closable: { type: Boolean, default: false },
  wsUrl: { type: String, default: '' }, // 自定义 WebSocket URL(容器 Shell 等场景),覆盖默认构造
})

const emit = defineEmits(['close', 'focus'])

const wrapEl = ref(null)
const termEl = ref(null)
// connecting | open | closed | exited
const status = ref('connecting')
const hasSelection = ref(false)

// 终端右键菜单(弹出朝向随视口剩余空间自动翻转)
const tmenu = reactive({ show: false, x: 0, y: 0 })
const tmenuEl = ref(null)
const { open: openMenuAt, close: closeTermMenu } = useContextMenu(tmenu)

function openTermMenu(e) {
  openMenuAt(e.clientX, e.clientY, tmenuEl)
}

function doSelectAll() {
  term.selectAll()
}

async function doCopy() {
  const sel = term.getSelection()
  if (!sel) return
  try {
    await navigator.clipboard.writeText(sel)
    ElMessage.success(t('term.copied'))
  } catch {
    document.execCommand('copy')
  }
}

async function doPaste() {
  try {
    const text = await navigator.clipboard.readText()
    if (text) term.paste(text)
  } catch {
    ElMessage.error(t('term.pasteFailed'))
  }
}

let term = null
let fitAddon = null
let ws = null
let ro = null
let disposed = false

function wsSend(obj) {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify(obj))
  }
}

// 连接建立后窗口尺寸变化时通知服务端同步 PTY 大小。SSH 终端、容器 Shell
// 与 K8s Pod 终端的服务端均解析 {type:"resize"} 控制帧。
function sendResize() {
  if (status.value === 'open') wsSend({ type: 'resize', cols: term.cols, rows: term.rows })
}

let fitTimer = null
// 同步 fit:用于连接前精确测量 cols/rows(不走防抖)
function fitSync() {
  if (!fitAddon || !wrapEl.value) return
  if (wrapEl.value.clientWidth < 40 || wrapEl.value.clientHeight < 40) return
  try {
    fitAddon.fit()
  } catch {
    /* 尺寸过小时的 fit 异常可忽略 */
  }
}

// 防抖 fit:用于 resize 事件(连续变化时只执行最后一次)
function fit() {
  if (!fitAddon || !wrapEl.value) return
  if (wrapEl.value.clientWidth < 40 || wrapEl.value.clientHeight < 40) return
  if (fitTimer) clearTimeout(fitTimer)
  fitTimer = setTimeout(() => {
    try {
      fitAddon.fit()
      sendResize()
    } catch {
      /* 尺寸过小时的 fit 异常可忽略 */
    }
  }, 50)
}

function initTerm() {
  term = new Terminal({
    fontSize: 14,
    fontFamily: "'Cascadia Mono', 'JetBrains Mono', Menlo, Consolas, monospace",
    cursorBlink: true,
    scrollback: 10000,
    theme: {
      background: '#0d1117',
      foreground: '#e6edf3',
      cursor: '#58a6ff',
      selectionBackground: '#264f78',
    },
  })
  fitAddon = new FitAddon()
  term.loadAddon(fitAddon)
  term.open(termEl.value)

  term.onData((d) => {
    wsSend({ type: 'input', data: d })
  })
  term.onSelectionChange(() => {
    hasSelection.value = term.hasSelection()
  })
  // 聚焦追踪由模板上 .term-wrap 的 mousedown/focusin 事件完成(xterm 无 onFocus API)

  term.attachCustomKeyEventHandler((e) => {
    if (e.type !== 'keydown' || !e.ctrlKey || !e.shiftKey) return true
    if (e.code === 'KeyC') {
      if (term.hasSelection()) {
        document.execCommand('copy')
        return false
      }
    } else if (e.code === 'KeyV') {
      navigator.clipboard
        .readText()
        .then((t) => t && term.paste(t))
        .catch(() => {})
      return false
    }
    return true
  })

  ro = new ResizeObserver(() => fit())
  ro.observe(wrapEl.value)
}

// 远程 shell 工作目录由服务端注入的 OSC 7 提示符钩子跟踪(bash/zsh),
// 前端不再做 cd 按键启发式(原实现依赖前端解析输入行,竞态且易漏)。

function connect() {
  if (disposed) return
  status.value = 'connecting'
  term.reset()
  // 连接前同步 fit,确保 WS URL 带正确的 cols/rows(而非默认 80x24)
  fitSync()
  const url = props.wsUrl || terminalWSUrl(props.sid, term.cols, term.rows)
  ws = new WebSocket(url)
  ws.binaryType = 'arraybuffer'

  ws.onopen = () => {
    status.value = 'open'
    fit()
    term.focus()
  }
  ws.onmessage = (ev) => {
    if (typeof ev.data === 'string') {
      try {
        const msg = JSON.parse(ev.data)
        if (msg.type === 'exit') status.value = 'exited'
        // shell 目录变更推送:转发给 cwd 总线,驱动 SFTP 面板跟随
        else if (msg.type === 'cwd') emitCwd(props.sid, msg.path)
      } catch {
        /* 忽略 */
      }
      return
    }
    term.write(new Uint8Array(ev.data))
  }
  ws.onclose = () => {
    if (status.value !== 'exited') status.value = 'closed'
  }
  ws.onerror = () => {}
}

function reconnect() {
  if (ws) {
    ws.onclose = null
    ws.close()
    ws = null
  }
  connect()
}

watch(
  () => props.active,
  (v) => {
    if (v) nextTick(() => {
      fit()
      if (status.value === 'open') term.focus()
    })
  }
)

onMounted(() => {
  initTerm()
  connect()
})

onBeforeUnmount(() => {
  disposed = true
  if (ro) ro.disconnect()
  if (ws) {
    ws.onclose = null
    ws.close()
  }
  if (term) term.dispose()
})
</script>

<template>
  <div
    class="term-wrap"
    ref="wrapEl"
    @contextmenu.prevent="openTermMenu"
    @mousedown="emit('focus')"
    @focusin="emit('focus')"
  >
    <div ref="termEl" style="height: 100%"></div>
    <el-icon
      v-if="closable"
      class="term-close"
      :title="t('term.closeThis')"
      @click.stop="emit('close')"
    >
      <Close />
    </el-icon>
    <div class="term-mask" v-if="status !== 'open'">
      <template v-if="status === 'connecting'">
        <el-icon :size="28" class="is-loading"><Loading /></el-icon>
        <span class="mask-text">{{ t('term.connecting', { host }) }}</span>
      </template>
      <template v-else-if="status === 'exited'">
        <span class="mask-text">{{ t('term.exited') }}</span>
        <el-button size="small" @click="reconnect">{{ t('term.reopenShell') }}</el-button>
      </template>
      <template v-else>
        <span class="mask-text">{{ t('term.disconnected') }}</span>
        <el-button size="small" type="primary" @click="reconnect">{{ t('term.reconnect') }}</el-button>
      </template>
    </div>

    <!-- 终端右键菜单 -->
    <teleport to="body">
      <div
        v-if="tmenu.show"
        ref="tmenuEl"
        class="ctx-menu"
        :style="{ left: tmenu.x + 'px', top: tmenu.y + 'px' }"
        @contextmenu.prevent
      >
        <div class="ctx-item" @click.stop="doSelectAll(); closeTermMenu()">{{ t('term.selectAll') }}</div>
        <div
          class="ctx-item"
          :class="{ disabled: !hasSelection }"
          @click.stop="hasSelection && (doCopy(), closeTermMenu())"
        >
          {{ t('term.copy') }}
        </div>
        <div class="ctx-item" @click.stop="doPaste(); closeTermMenu()">{{ t('term.paste') }}</div>
      </div>
    </teleport>
  </div>
</template>
