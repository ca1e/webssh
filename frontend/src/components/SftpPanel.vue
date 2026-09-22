<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  ArrowUp,
  Bottom,
  CaretBottom,
  Close,
  Delete,
  Document,
  Edit,
  Folder,
  FolderAdd,
  House,
  Refresh,
  Top,
} from '@element-plus/icons-vue'
import {
  cancelTransfer,
  createTransfer,
  deleteTransfer,
  fmtSize,
  joinPath,
  listTransfers,
  localDelete,
  localList,
  localMkdir,
  localRename,
  sftpDelete,
  sftpList,
  sftpMkdir,
  sftpRename,
  sessionCwd,
  transfersWSUrl,
} from '../api'
import { onCwd } from '../cwdBus'
import { t } from '../i18n'

const props = defineProps({
  session: { type: Object, required: true },
  sessions: { type: Array, default: () => [] }, // 所有活动会话(双远程模式用)
})

// 面板级 UI 状态(统一 reactive,保证模板响应性)
const ui = reactive({
  dd: null, // 打开路径下拉的面板 id:'top'|'bottom'|null
  remotePct: 50, // 上下面板高度百分比(20%-80% 可拖动)
  modalVisible: false, // 新建文件夹/重命名 弹窗
  modalMode: 'mkdir', // mkdir | rename
  modalPane: 'top', // 作用面板 id
  name: '',
  oldPath: '',
})

const rootEl = ref(null)
const nameInputRef = ref(null)

function makePane(id, kind, label) {
  return reactive({
    id, // 'top' | 'bottom'
    kind, // 'remote' | 'local'(bottom 可在两者间切换)
    sid: null, // remote 时:top → 当前会话;bottom → 其他会话 id
    label,
    path: '',
    pathInput: '',
    entries: [],
    selected: [],
    loading: false,
    error: '',
    transferring: false,
    showHidden: false, // 显示 . 开头的隐藏文件,默认隐藏
  })
}

// 上面板:当前会话远程;下面板:本机或另一台已连接主机(双远程模式)
// label 仅用于双远程模式下记录下方面板对应会话名,显示名统一走 paneLabel()
const remote = makePane('top', 'remote', '')
const local = makePane('bottom', 'local', '')
const panes = computed(() => [remote, local])

// 面板显示名:本机/远程为界面文案(随语言切换),双远程下方面板显示会话名
function paneLabel(pane) {
  if (pane.kind === 'local') return t('sftp.localPane')
  return pane.id === 'top' ? t('sftp.remotePane') : pane.label || pane.sid
}

// 面板对应的会话 id(local 面板为本机时为 null)
function paneSid(pane) {
  if (pane.kind !== 'remote') return null
  return pane.id === 'top' ? props.session.id : pane.sid
}

// 文件操作按面板归属派发(本机走 local API,远程走对应会话的 SFTP API)
function paneApi(pane) {
  if (pane.kind === 'local') {
    return {
      list: (p) => localList(p),
      del: (p) => localDelete(p),
      mkdir: (p) => localMkdir(p),
      rename: (p, n) => localRename(p, n),
    }
  }
  const sid = paneSid(pane)
  return {
    list: (p) => sftpList(sid, p),
    del: (p) => sftpDelete(sid, p),
    mkdir: (p) => sftpMkdir(sid, p),
    rename: (p, n) => sftpRename(sid, p, n),
  }
}

const paneOf = (id) => (id === 'top' ? remote : local)

// ---------- 路径下拉(首项 .. 上级,其余为子目录全路径) ----------

function parentDir(p) {
  if (!p || p === '/') return '/'
  const parent = p.replace(/\/+$/, '').split('/').slice(0, -1).join('/')
  return parent || '/'
}

// 未勾选"显示全部文件"时,过滤掉 . 开头的隐藏文件(列表与下拉共用)
// computed 缓存保证引用稳定,避免 selection 被表格重渲染清空
const remoteVisible = computed(() =>
  remote.showHidden ? remote.entries : remote.entries.filter((e) => !e.name.startsWith('.'))
)
const localVisible = computed(() =>
  local.showHidden ? local.entries : local.entries.filter((e) => !e.name.startsWith('.'))
)

function visibleEntries(pane) {
  return pane.id === 'top' ? remoteVisible.value : localVisible.value
}

function subDirs(pane) {
  return visibleEntries(pane)
    .filter((e) => e.is_dir)
    .map((e) => joinPath(pane.path, e.name))
    .sort()
}

// ---------- 双远程模式:下面板切换目标(本机 / 其他已连接会话) ----------

const bottomTarget = computed({
  get: () => (local.kind === 'remote' ? local.sid : 'local'),
  set: (v) => switchBottom(v),
})

const sessionOptions = computed(() =>
  (props.sessions || []).filter((x) => x.id !== props.session.id && x.connType !== 'telnet')
)

function switchBottom(v) {
  closeDd()
  local.selected = []
  if (v === 'local') {
    local.kind = 'local'
    local.sid = null
    local.label = ''
    load(local, '')
    return
  }
  const ss = sessionOptions.value.find((x) => x.id === v)
  local.kind = 'remote'
  local.sid = v
  local.label = ss ? ss.name : v
  // 初始定位到该会话当前目录
  sessionCwd(v)
    .then((res) => {
      if (res.path) {
        lastSyncedBottom = res.path
        load(local, res.path)
      } else {
        load(local, '')
      }
    })
    .catch(() => load(local, ''))
}

// ---------- 会话 cwd → SFTP 面板单向同步 ----------
// 实时通道:后端 OSC 7 提示符钩子命中 → terminalWS 推 {"type":"cwd"} →
// cwd 事件总线,面板订阅即得,无需轮询。兜底:面板可见时 30s 低频对账一次
// (防 WS 帧丢失、双远程目标会话无终端连接等场景)。
// 仅当会话 cwd 与上次同步值不同才跟随,手动输入的目录不会被覆盖。

let lastSyncedTop = ''
let lastSyncedBottom = ''
let cwdTimer = null
let offTopCwd = null
let offBottomCwd = null

function applyTopCwd(path) {
  if (path && path !== lastSyncedTop) {
    lastSyncedTop = path
    if (remote.path !== path) load(remote, path)
  }
}

function applyBottomCwd(path) {
  if (path && path !== lastSyncedBottom) {
    lastSyncedBottom = path
    if (local.path !== path) load(local, path)
  }
}

// 下面板指向的会话随切换订阅/退订 cwd 事件
watch(
  () => local.sid,
  (sid) => {
    if (offBottomCwd) {
      offBottomCwd()
      offBottomCwd = null
    }
    if (sid && local.kind === 'remote') offBottomCwd = onCwd(sid, applyBottomCwd)
  }
)

// 面板是否可见:所属会话标签页被切走时 el-tab-pane 以 display:none 隐藏,
// 此时 offsetParent 为 null;浏览器标签页后台时 visibilityState 为 hidden。
function panelVisible() {
  if (document.visibilityState !== 'visible') return false
  const el = rootEl.value
  return !!el && !!el.offsetParent
}

async function syncCwd() {
  // 上面板跟随当前会话
  try {
    const res = await sessionCwd(props.session.id)
    applyTopCwd(res.path)
  } catch {
    /* 会话断开等场景忽略 */
  }
  // 双远程模式下,下面板跟随其对应会话
  if (local.kind === 'remote' && local.sid) {
    try {
      const res = await sessionCwd(local.sid)
      applyBottomCwd(res.path)
    } catch {
      /* 忽略 */
    }
  }
  // 对方会话已断开时,下面板切回本机
  if (local.kind === 'remote' && local.sid && !sessionOptions.value.some((x) => x.id === local.sid)) {
    ElMessage.warning(t('sftp.peerDisconnected'))
    switchBottom('local')
  }
}

async function load(pane, path) {
  pane.loading = true
  try {
    const res = await paneApi(pane).list(path)
    pane.entries = res.entries
    pane.path = res.path
    pane.pathInput = res.path
    pane.error = ''
    pane.selected = []
  } catch (e) {
    // 保留原目录与列表,只在面板内显著提示
    pane.error = e.message
    ElMessage.error({ message: e.message, duration: 5000, showClose: true })
  } finally {
    pane.loading = false
  }
}

function enterPath(pane) {
  const p = pane.pathInput.trim()
  if (p && p !== pane.path) load(pane, p)
}

function refresh(pane) {
  load(pane, pane.path)
}

function openRow(pane, row) {
  if (row.is_dir) load(pane, joinPath(pane.path, row.name))
}

// ---------- 传输(上→下 / 下→上,统一异步任务) ----------
// 本机↔远程与远程↔远程走同一 /api/transfers 任务模型:创建即返回,
// 进度经 WS 推送,目标面板在任务完成沿自动刷新。

const dualRemote = computed(() => local.kind === 'remote')

async function submitTransfer(srcPane, dstPane) {
  if (!srcPane.selected.length) return
  if (!dstPane.path) {
    ElMessage.warning(t('sftp.warnTargetDir'))
    return
  }
  srcPane.transferring = true
  const kindOf = (pane) => (pane.kind === 'local' ? 'local' : 'session')
  let ok = 0
  let failMsg = null
  for (const item of srcPane.selected) {
    try {
      const res = await createTransfer({
        source: { kind: kindOf(srcPane), sid: paneSid(srcPane), path: joinPath(srcPane.path, item.name) },
        target: { kind: kindOf(dstPane), sid: paneSid(dstPane), path: joinPath(dstPane.path, item.name) },
        recursive: !!item.is_dir,
      })
      freshTransferIds.add(res.id)
      ok++
    } catch (e) {
      failMsg = `${item.name}: ${e.message}`
    }
  }
  srcPane.transferring = false
  if (failMsg) {
    ElMessage.error({ message: t('sftp.enqueueFailed', { msg: failMsg }), duration: 6000, showClose: true })
  } else {
    ElMessage.success(t('sftp.enqueued', { n: ok }))
  }
}

const download = () => submitTransfer(remote, local)
const upload = () => submitTransfer(local, remote)

// ---------- 传输任务进度(WS 推送,断线退化为拉取) ----------

const transferTasks = ref([])
const prevStatuses = new Map() // 任务 id → 上次状态(识别 running→done 完成沿)
const freshTransferIds = new Set() // 本面板刚创建的任务 id(防止首轮推送前已完成而漏掉完成沿)
let transferWS = null
let transferWSRetryTimer = null
let transferWSDisposed = false

// 面板与任务的"目标键":本机为 'local',远程为会话 id
const paneTargetKey = (pane) => (pane.kind === 'local' ? 'local' : paneSid(pane))
const taskTargetKey = (t) => (t.target_kind === 'local' ? 'local' : t.target_sid)

// 识别刚完成的任务并刷新目标面板,传输的文件才会出现在目标目录列表中
function applyTasks(tasks) {
  transferTasks.value = tasks
  const paneIds = new Set()
  for (const t of tasks) {
    const prev = prevStatuses.get(t.id) ?? (freshTransferIds.has(t.id) ? 'running' : undefined)
    prevStatuses.set(t.id, t.status)
    if (freshTransferIds.has(t.id) && t.status !== 'running') freshTransferIds.delete(t.id)
    if (prev === 'running' && t.status === 'done') {
      if (paneTargetKey(remote) === taskTargetKey(t)) paneIds.add('top')
      if (paneTargetKey(local) === taskTargetKey(t)) paneIds.add('bottom')
    }
  }
  for (const id of paneIds) {
    const pane = paneOf(id)
    load(pane, pane.path)
  }
}

async function refreshTasksFallback() {
  try {
    applyTasks(await listTransfers())
  } catch {
    /* 忽略 */
  }
}

function connectTransferWS() {
  if (transferWSDisposed) return
  transferWS = new WebSocket(transfersWSUrl())
  transferWS.onmessage = (ev) => {
    try {
      applyTasks(JSON.parse(ev.data))
    } catch {
      /* 忽略 */
    }
  }
  transferWS.onclose = () => {
    transferWS = null
    if (transferWSDisposed) return
    refreshTasksFallback()
    transferWSRetryTimer = setTimeout(connectTransferWS, 3000)
  }
  transferWS.onerror = () => {}
}

function taskPct(t) {
  if (t.status === 'done') return 100
  if (!t.total) return 0
  return Math.min(100, Math.round((t.done / t.total) * 100))
}

function statusText(task) {
  if (task.status === 'done') return t('sftp.done')
  if (task.status === 'canceled') return t('sftp.canceled')
  if (task.status === 'failed') return t('sftp.failed')
  return t('sftp.transferring')
}

// 删除传输记录;运行中的任务会先被后端取消再移除
async function removeTask(t) {
  try {
    await deleteTransfer(t.id)
  } catch (e) {
    ElMessage.error(e.message)
  }
  refreshTasksFallback()
}

// ---------- 删除 ----------

async function removeItems(pane) {
  if (!pane.selected.length) return
  const names = pane.selected.map((s) => s.name).join('、')
  try {
    await ElMessageBox.confirm(
      t('sftp.deleteConfirm', { pane: paneLabel(pane), path: pane.path, names }),
      t('common.deleteConfirmTitle'),
      { type: 'warning', confirmButtonText: t('common.delete'), confirmButtonClass: 'el-button--danger' }
    )
  } catch {
    return
  }
  const fails = []
  for (const item of pane.selected) {
    const p = joinPath(pane.path, item.name)
    try {
      await paneApi(pane).del(p)
    } catch (e) {
      fails.push(`${item.name}: ${e.message}`)
    }
  }
  if (fails.length) {
    ElMessage.error({ message: t('sftp.deleteFailed', { msgs: fails.join(';') }), duration: 6000, showClose: true })
  } else {
    ElMessage.success(t('sftp.deleteOk'))
  }
  load(pane, pane.path)
}

// ---------- 拖动调整上下面板高度(20%-80%) ----------

function startHResize(e) {
  e.preventDefault()
  const rect = e.target.parentElement.getBoundingClientRect()
  const onMove = (ev) => {
    const pct = ((ev.clientY - rect.top) / rect.height) * 100
    ui.remotePct = Math.min(80, Math.max(20, pct))
  }
  const onUp = () => {
    window.removeEventListener('mousemove', onMove)
    window.removeEventListener('mouseup', onUp)
    document.body.style.cursor = ''
    document.body.style.userSelect = ''
  }
  window.addEventListener('mousemove', onMove)
  window.addEventListener('mouseup', onUp)
  document.body.style.cursor = 'row-resize'
  document.body.style.userSelect = 'none'
}

// ---------- 新建文件夹 / 重命名 ----------

function openNameModal(mode, pane) {
  if (mode === 'rename') {
    if (pane.selected.length !== 1) {
      ElMessage.warning(t('sftp.renameNeedOne'))
      return
    }
    ui.modalMode = 'rename'
    ui.modalPane = pane.id
    ui.name = pane.selected[0].name
    ui.oldPath = joinPath(pane.path, pane.selected[0].name)
  } else {
    ui.modalMode = 'mkdir'
    ui.modalPane = pane.id
    ui.name = ''
  }
  ui.modalVisible = true
}

async function submitNameModal() {
  const pane = paneOf(ui.modalPane)
  const name = ui.name.trim()
  if (!name) {
    ElMessage.error(t('sftp.nameEmpty'))
    return
  }
  if ([...name].length > 64) {
    ElMessage.error(t('sftp.nameTooLong'))
    return
  }
  if (name.includes('/') || name.includes('\\') || name === '.' || name === '..') {
    ElMessage.error(t('sftp.nameInvalid'))
    return
  }
  try {
    if (ui.modalMode === 'mkdir') {
      const p = joinPath(pane.path, name)
      await paneApi(pane).mkdir(p)
      ElMessage.success(t('sftp.dirCreated', { name }))
    } else {
      await paneApi(pane).rename(ui.oldPath, name)
      ElMessage.success(t('sftp.renameOk'))
    }
    ui.modalVisible = false
    load(pane, pane.path)
  } catch (e) {
    ElMessage.error({ message: e.message, duration: 6000, showClose: true })
  }
}

// ---------- 路径下拉开合 ----------

function toggleDd(pane) {
  ui.dd = ui.dd === pane.id ? null : pane.id
}

function pickDd(pane, p) {
  ui.dd = null
  load(pane, p)
}

function closeDd() {
  ui.dd = null
}

function onGlobalKeydown(e) {
  if (e.key === 'Escape') {
    closeDd()
    ui.modalVisible = false
  }
}

onMounted(async () => {
  // 上面板初始定位到会话当前目录(即主目录)
  try {
    const res = await sessionCwd(props.session.id)
    if (res.path) lastSyncedTop = res.path
  } catch {
    /* 忽略,回退为 home */
  }
  load(remote, lastSyncedTop)
  load(local, '')
  offTopCwd = onCwd(props.session.id, applyTopCwd)
  // 兜底对账:仅面板可见时执行,不可见时推送通道仍在后台正常驱动跟随
  cwdTimer = setInterval(() => {
    if (panelVisible()) syncCwd()
  }, 30000)
  connectTransferWS()
  refreshTasksFallback() // 首帧立即同步一次,不等 WS 首推
  window.addEventListener('click', closeDd)
  window.addEventListener('keydown', onGlobalKeydown)
  window.addEventListener('resize', closeDd)
})

onBeforeUnmount(() => {
  if (cwdTimer) clearInterval(cwdTimer)
  if (offTopCwd) offTopCwd()
  if (offBottomCwd) offBottomCwd()
  transferWSDisposed = true
  if (transferWSRetryTimer) clearTimeout(transferWSRetryTimer)
  if (transferWS) {
    transferWS.onclose = null
    transferWS.close()
  }
  window.removeEventListener('click', closeDd)
  window.removeEventListener('keydown', onGlobalKeydown)
  window.removeEventListener('resize', closeDd)
})
</script>

<template>
  <div ref="rootEl" class="sftp-panel">
    <div class="panel-title">
      SFTP · {{ session.name }}
      <span class="muted" style="font-weight: 400">{{ session.username }}@{{ session.host }}</span>
      <span v-if="dualRemote" class="dual-badge">{{ t('sftp.dualMode') }}</span>
    </div>

    <template v-for="(pane, i) in panes" :key="pane.id">
      <div
        class="file-pane"
        :class="[pane.id, pane.kind, { remote2: pane.id === 'bottom' && pane.kind === 'remote' }]"
        :style="pane.id === 'top' ? { flex: 'none', height: ui.remotePct + '%' } : {}"
      >
        <div class="pane-header">
          <span v-if="pane.id === 'top'" class="pane-label" :class="pane.kind">{{ paneLabel(pane) }}</span>
          <el-select
            v-else
            v-model="bottomTarget"
            size="small"
            class="target-select"
            :title="t('sftp.targetSelectTitle')"
          >
            <el-option value="local" :label="t('sftp.localPane')" />
            <el-option
              v-for="s in sessionOptions"
              :key="s.id"
              :value="s.id"
              :label="s.name"
            />
          </el-select>
          <el-button
            size="small"
            :icon="ArrowUp"
            :title="t('sftp.parentDir')"
            :disabled="parentDir(pane.path) === pane.path"
            @click="load(pane, parentDir(pane.path))"
          />
          <el-button
            size="small"
            :icon="House"
            :title="t('sftp.homeDir')"
            @click="load(pane, '~')"
          />
          <div class="path-wrap">
            <el-input
              v-model="pane.pathInput"
              class="path-input"
              size="small"
              :placeholder="t('sftp.pathPlaceholder')"
              @keyup.enter="enterPath(pane)"
            >
              <template #suffix>
                <el-icon
                  class="path-dd-trigger"
                  :class="{ active: ui.dd === pane.id }"
                  :title="t('sftp.ddTitle')"
                  @click.stop="toggleDd(pane)"
                >
                  <CaretBottom />
                </el-icon>
              </template>
            </el-input>
            <!-- 与输入框对齐同宽的下拉;超出区域滚动;下面板向上弹出 -->
            <div
              v-if="ui.dd === pane.id"
              class="path-dd-menu"
              :class="{ up: pane.id === 'bottom' }"
              @contextmenu.prevent
            >
              <div
                class="dd-item"
                :class="{ disabled: parentDir(pane.path) === pane.path }"
                @click.stop="pickDd(pane, parentDir(pane.path))"
              >
                ..
              </div>
              <div
                v-for="p in subDirs(pane)"
                :key="p"
                class="dd-item"
                :class="{ current: p === pane.path }"
                :title="p"
                @click.stop="pickDd(pane, p)"
              >
                {{ p }}
              </div>
              <div v-if="!subDirs(pane).length" class="dd-item disabled">{{ t('sftp.noSubdirs') }}</div>
            </div>
          </div>
          <el-button
            size="small"
            :icon="Refresh"
            :title="t('common.refresh')"
            :loading="pane.loading"
            @click="refresh(pane)"
          />
        </div>

        <div class="pane-actions">
          <template v-if="pane.id === 'top'">
            <el-button
              size="small"
              type="primary"
              :icon="Bottom"
              :loading="pane.transferring"
              :disabled="!pane.selected.length"
              :title="dualRemote ? t('sftp.sendDownTitle') : t('sftp.downloadTitle')"
              @click="download"
            >
              {{ dualRemote ? t('sftp.sendDown') : t('sftp.download') }}
            </el-button>
          </template>
          <template v-else>
            <el-button
              size="small"
              type="primary"
              :icon="Top"
              :loading="pane.transferring"
              :disabled="!pane.selected.length"
              :title="dualRemote ? t('sftp.sendUpTitle') : t('sftp.uploadTitle')"
              @click="upload"
            >
              {{ dualRemote ? t('sftp.sendUp') : t('sftp.upload') }}
            </el-button>
          </template>
          <el-button size="small" :icon="FolderAdd" @click="openNameModal('mkdir', pane)">
            {{ t('sftp.mkdir') }}
          </el-button>
          <el-button size="small" :icon="Edit" @click="openNameModal('rename', pane)">
            {{ t('sftp.rename') }}
          </el-button>
          <el-button
            size="small"
            type="danger"
            :icon="Delete"
            :disabled="!pane.selected.length"
            @click="removeItems(pane)"
          >
            {{ t('common.delete') }}
          </el-button>
          <el-checkbox
            v-model="pane.showHidden"
            size="small"
            class="show-hidden"
            :title="t('sftp.showHiddenTitle')"
          >
            {{ t('sftp.showHidden') }}
          </el-checkbox>
        </div>

        <el-alert
          v-if="pane.error"
          :title="pane.error"
          type="error"
          show-icon
          closable
          class="pane-alert"
          @close="pane.error = ''"
        />

        <div class="pane-table">
          <el-table
            :data="visibleEntries(pane)"
            v-loading="pane.loading"
            size="small"
            height="100%"
            :empty-text="t('sftp.emptyDir')"
            @selection-change="(s) => (pane.selected = s)"
            @row-dblclick="(row) => openRow(pane, row)"
          >
            <el-table-column type="selection" width="36" />
            <el-table-column prop="name" :label="t('common.colName')" min-width="150" show-overflow-tooltip>
              <template #default="{ row }">
                <span class="file-name" :class="{ 'is-file': !row.is_dir }">
                  <el-icon><Folder v-if="row.is_dir" /><Document v-else /></el-icon>
                  <span :style="row.is_dir ? 'cursor:pointer' : ''">{{ row.name }}</span>
                </span>
              </template>
            </el-table-column>
            <el-table-column :label="t('common.colSize')" width="80">
              <template #default="{ row }">{{ fmtSize(row.size, row.is_dir) }}</template>
            </el-table-column>
            <el-table-column :label="t('sftp.colOwner')" width="100" show-overflow-tooltip>
              <template #default="{ row }">
                <span :title="`${row.owner} / ${row.group}`">{{ row.owner }} / {{ row.group }}</span>
              </template>
            </el-table-column>
            <el-table-column prop="mode" :label="t('sftp.colMode')" width="100" />
          </el-table>
        </div>
      </div>
      <div
        v-if="i === 0"
        class="hsplitter"
        :title="t('sftp.dragHeight')"
        @mousedown="startHResize"
      ></div>
    </template>

    <!-- 传输任务进度(WS 推送) -->
    <div class="relay-tasks" v-if="transferTasks.length">
      <div v-for="task in transferTasks.slice(0, 6)" :key="task.id" class="relay-task">
        <span
          class="rt-route"
          :title="`${task.source_path} → ${task.target_name}:${task.target_path}`"
        >
          {{ task.source_name }} → {{ task.target_name }} · {{ task.target_path }}
        </span>
        <el-progress
          class="rt-bar"
          :percentage="taskPct(task)"
          :stroke-width="8"
          :status="task.status === 'done' ? 'success' : task.status === 'failed' ? 'exception' : undefined"
        />
        <span class="rt-size">{{ fmtSize(task.done, false) }}/{{ fmtSize(task.total, false) }}</span>
        <span class="rt-status" :class="task.status" :title="task.error || ''">{{ statusText(task) }}</span>
        <el-button
          v-if="task.status === 'running'"
          size="small"
          type="danger"
          link
          @click="cancelTransfer(task.id)"
        >
          {{ t('common.cancel') }}
        </el-button>
        <el-icon class="rt-delete" :title="t('sftp.deleteRecord')" @click="removeTask(task)">
          <Close />
        </el-icon>
      </div>
    </div>

    <!-- 新建文件夹 / 重命名 弹窗(名称最长 64 字符) -->
    <el-dialog
      v-model="ui.modalVisible"
      :title="ui.modalMode === 'mkdir' ? t('sftp.mkdir') : t('sftp.rename')"
      width="420px"
      :close-on-click-modal="false"
      append-to-body
      @opened="nameInputRef?.focus?.()"
    >
      <div class="muted" style="margin-bottom: 8px">
        {{ t('sftp.location', { path: paneOf(ui.modalPane).path }) }}
      </div>
      <el-input
        ref="nameInputRef"
        v-model="ui.name"
        maxlength="64"
        show-word-limit
        :placeholder="t('sftp.namePlaceholder')"
        @keyup.enter="submitNameModal"
      />
      <template #footer>
        <el-button @click="ui.modalVisible = false">{{ t('common.cancel') }}</el-button>
        <el-button type="primary" @click="submitNameModal">{{ t('common.ok') }}</el-button>
      </template>
    </el-dialog>
  </div>
</template>
