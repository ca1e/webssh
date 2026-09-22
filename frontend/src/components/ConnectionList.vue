<script setup>
import { computed, reactive, ref } from 'vue'
import { ArrowDown, Connection, DArrowLeft, Monitor, Operation, Refresh } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { useContextMenu } from '../contextMenu'
import { exportBundle, importBundle } from '../api'
import { settings } from '../settings'
import { t } from '../i18n'

const props = defineProps({
  hosts: { type: Array, default: () => [] },
})

const emit = defineEmits(['connect', 'collapse', 'edit', 'delete', 'duplicate', 'refresh'])

// 分组展开/收起状态,持久化到 localStorage
const STORAGE_KEY = 'webssh-collapsed-groups'
const collapsed = reactive(loadCollapsed())
function loadCollapsed() {
  try {
    return new Set(JSON.parse(localStorage.getItem(STORAGE_KEY) || '[]'))
  } catch {
    return new Set()
  }
}
function saveCollapsed() {
  localStorage.setItem(STORAGE_KEY, JSON.stringify([...collapsed]))
}

// 按 group 聚合,未填 group 归入未分组(内部键为空串,显示名走 i18n);
// 分组顺序按首次出现排序
const groups = computed(() => {
  const order = []
  const map = {}
  for (const h of props.hosts) {
    const g = h.group || ''
    if (!map[g]) {
      map[g] = []
      order.push(g)
    }
    map[g].push(h)
  }
  return order.map((g) => ({ name: g, hosts: map[g] }))
})

// 分组显示名:空串 = 未分组
const groupLabel = (name) => name || t('host.ungrouped')

function toggleGroup(name) {
  if (collapsed.has(name)) collapsed.delete(name)
  else collapsed.add(name)
  saveCollapsed()
}

function isCollapsed(name) {
  return collapsed.has(name)
}

// 按设置的主机打开方式触发连接:双击模式保持原逻辑,单击模式单击即连
function connTitle(h) {
  return t(settings.sidebarOpenMode === 'click' ? 'sidebar.clickToConnect' : 'sidebar.dblclickToConnect', { name: h.name })
}
function onClick(h) {
  if (settings.sidebarOpenMode === 'click') emit('connect', h)
}
function onDblClick(h) {
  if (settings.sidebarOpenMode === 'dblclick') emit('connect', h)
}

// 右键菜单(弹出朝向随视口剩余空间自动翻转)
const menu = reactive({ show: false, x: 0, y: 0, host: null })
const menuEl = ref(null)
const { open: openMenuAt, close: closeMenu } = useContextMenu(menu)

function openMenu(host, e) {
  menu.host = host
  openMenuAt(e.clientX, e.clientY, menuEl)
}

function run(action) {
  if (menu.host) emit(action, menu.host)
  closeMenu()
}

// 导入/导出菜单(左键锚定在工具栏按钮下方,复用右键菜单的自动翻转逻辑)
const imex = reactive({ show: false, x: 0, y: 0 })
const imexEl = ref(null)
const fileEl = ref(null)
const { open: imexOpen, close: imexClose } = useContextMenu(imex)

function onImexClick(e) {
  if (imex.show) {
    imexClose()
    return
  }
  const rect = e.currentTarget.getBoundingClientRect()
  imexOpen(rect.left, rect.bottom + 4, imexEl)
}

// 下载 blob:优先系统保存对话框,不支持时回退浏览器下载
async function saveBlob(blob, filename) {
  if (window.showSaveFilePicker) {
    try {
      const handle = await window.showSaveFilePicker({
        suggestedName: filename,
        types: [{ description: 'JSON', accept: { 'application/json': ['.json'] } }],
      })
      const writable = await handle.createWritable()
      await writable.write(blob)
      await writable.close()
      return
    } catch (e) {
      if (e?.name === 'AbortError') return // 用户取消保存,静默
    }
  }
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}

// ---------- 加密导出对话框(口令两次确认;模式只由按钮决定) ----------

const exportDlg = reactive({ show: false, password: '', confirm: '' })

function openExportDialog() {
  imexClose()
  exportDlg.password = ''
  exportDlg.confirm = ''
  exportDlg.show = true
}

async function doExportEncrypted() {
  if (exportDlg.password.length < 4) {
    ElMessage.warning(t('sidebar.pwdTooShort'))
    return
  }
  if (exportDlg.password !== exportDlg.confirm) {
    ElMessage.warning(t('sidebar.pwdMismatch'))
    return
  }
  try {
    const { blob, filename } = await exportBundle('encrypted', exportDlg.password)
    await saveBlob(blob, filename)
    exportDlg.show = false
    ElMessage.success(t('sidebar.exportedEncrypted'))
  } catch (e) {
    ElMessage.error(e.message || t('sidebar.exportFailed'))
  }
}

// 无密码导出:独立按钮显式触发,凭据 base64 明文,仅限内网备份/快速迁移
async function doExportPlaintext() {
  try {
    const { blob, filename } = await exportBundle('plaintext', '')
    await saveBlob(blob, filename)
    exportDlg.show = false
    ElMessage.warning(t('sidebar.exportedPlaintext'))
  } catch (e) {
    ElMessage.error(e.message || t('sidebar.exportFailed'))
  }
}

function doImport() {
  imexClose()
  fileEl.value?.click()
}

// 导入:加密 bundle 追问口令;解密失败/旧数组格式由后端统一给引导文案
async function onImportFile(e) {
  const file = e.target.files?.[0]
  e.target.value = '' // 允许重复选择同一文件
  if (!file) return
  try {
    let password = ''
    try {
      const doc = JSON.parse(await file.text())
      if (doc && typeof doc === 'object' && doc.kdf) {
        const { value } = await ElMessageBox.prompt(
          t('sidebar.importPrompt'), t('sidebar.importPwdTitle'), { inputType: 'password' }
        )
        password = value || ''
      }
    } catch (pe) {
      if (pe === 'cancel' || pe?.action === 'cancel') return // 用户取消
      // 非 JSON 文件交给后端判定格式并给出引导文案
    }
    const res = await importBundle(file, password)
    ElMessage.success(t('sidebar.imported', { n: res.count }))
    emit('refresh')
  } catch (err) {
    ElMessage.error(err.message || t('sidebar.importFailed'))
  }
}
</script>

<template>
  <aside class="sidebar">
    <div style="padding: 10px 10px 0; display: flex; justify-content: flex-end">
      <el-button text style="padding: 8px" :icon="Operation" :title="t('sidebar.imexTitle')" @click.stop="onImexClick" />
      <el-button text style="padding: 8px" :icon="Refresh" @click="$emit('refresh')" :title="t('sidebar.refreshList')" />
      <el-button text style="padding: 8px" :icon="DArrowLeft" @click="$emit('collapse')" :title="t('sidebar.collapseList')" />
    </div>
    <div class="conn-list">
      <template v-for="g in groups" :key="g.name">
        <div
          class="group-header"
          :title="isCollapsed(g.name) ? t('sidebar.clickToExpand') : t('sidebar.clickToCollapse')"
          @click="toggleGroup(g.name)"
        >
          <el-icon class="group-arrow" :class="{ collapsed: isCollapsed(g.name) }">
            <ArrowDown />
          </el-icon>
          <span class="group-name">{{ groupLabel(g.name) }}</span>
          <span class="group-count">{{ g.hosts.length }}</span>
        </div>
        <template v-if="!isCollapsed(g.name)">
          <div
            v-for="h in g.hosts"
            :key="h.id"
            class="conn-item"
            :title="connTitle(h)"
            @click="onClick(h)"
            @dblclick="onDblClick(h)"
            @contextmenu.prevent="openMenu(h, $event)"
          >
            <el-icon class="conn-proto" :class="h.conn_type === 'telnet' ? 'telnet' : 'ssh'">
              <Connection v-if="h.conn_type === 'telnet'" />
              <Monitor v-else />
            </el-icon>
            <div class="conn-info">
              <div class="conn-name">{{ h.name }}</div>
              <div class="conn-host">{{ h.host }}:{{ h.port }}<template v-if="h.conn_type !== 'telnet'"> · {{ h.username }}</template></div>
            </div>
          </div>
        </template>
      </template>
      <div v-if="!hosts.length" class="empty-hint">{{ t('sidebar.empty') }}</div>
    </div>

    <!-- 右键菜单 -->
    <teleport to="body">
      <div
        v-if="menu.show"
        ref="menuEl"
        class="ctx-menu"
        :style="{ left: menu.x + 'px', top: menu.y + 'px' }"
        @contextmenu.prevent
      >
        <div class="ctx-item" @click.stop="run('connect')">{{ t('sidebar.connect') }}</div>
        <div class="ctx-item" @click.stop="run('edit')">{{ t('common.edit') }}</div>
        <div class="ctx-item" @click.stop="run('duplicate')">{{ t('common.duplicate') }}</div>
        <div class="ctx-item danger" @click.stop="run('delete')">{{ t('common.delete') }}</div>
      </div>
    </teleport>

    <!-- 导入/导出菜单 -->
    <teleport to="body">
      <div
        v-if="imex.show"
        ref="imexEl"
        class="ctx-menu"
        :style="{ left: imex.x + 'px', top: imex.y + 'px' }"
        @contextmenu.prevent
      >
        <div class="ctx-item" @click.stop="openExportDialog">{{ t('sidebar.exportEncrypted') }}</div>
        <div class="ctx-item" @click.stop="doImport">{{ t('sidebar.importHosts') }}</div>
      </div>
    </teleport>

    <!-- 加密导出对话框:口令两次确认;无密码导出为独立显式按钮 -->
    <el-dialog v-model="exportDlg.show" :title="t('sidebar.exportTitle')" width="380px" append-to-body>
      <el-input
        v-model="exportDlg.password"
        type="password"
        show-password
        :placeholder="t('sidebar.exportPwdPlaceholder')"
        style="margin-bottom: 10px"
      />
      <el-input
        v-model="exportDlg.confirm"
        type="password"
        show-password
        :placeholder="t('sidebar.exportPwdConfirmPlaceholder')"
      />
      <div style="color: var(--el-text-color-secondary); font-size: 12px; margin-top: 8px">
        {{ t('sidebar.exportNote') }}
      </div>
      <template #footer>
        <el-button type="warning" plain @click="doExportPlaintext">{{ t('sidebar.exportPlaintext') }}</el-button>
        <el-button @click="exportDlg.show = false">{{ t('common.cancel') }}</el-button>
        <el-button type="primary" @click="doExportEncrypted">{{ t('sidebar.exportEncryptedBtn') }}</el-button>
      </template>
    </el-dialog>

    <!-- 导入文件选择(隐藏,系统文件选择框由菜单项触发) -->
    <input ref="fileEl" type="file" accept=".json,application/json" style="display: none" @change="onImportFile" />
  </aside>
</template>
