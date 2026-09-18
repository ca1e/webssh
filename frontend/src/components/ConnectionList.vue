<script setup>
import { computed, reactive, ref } from 'vue'
import { ArrowDown, Connection, DArrowLeft, Monitor, Operation, Refresh } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import { useContextMenu } from '../contextMenu'
import { fetchHostsFile, importHosts } from '../api'
import { settings } from '../settings'

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

// 按 group 聚合,未填 group 归入"未分组";分组顺序按首次出现排序
const groups = computed(() => {
  const order = []
  const map = {}
  for (const h of props.hosts) {
    const g = h.group || '未分组'
    if (!map[g]) {
      map[g] = []
      order.push(g)
    }
    map[g].push(h)
  }
  return order.map((g) => ({ name: g, hosts: map[g] }))
})

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
  return `${settings.sidebarOpenMode === 'click' ? '单击' : '双击'}连接 ${h.name},右键更多操作`
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

// 导出:下载 hosts.json 原文,优先系统保存对话框,不支持时回退浏览器下载
async function doExport() {
  imexClose()
  try {
    const blob = await fetchHostsFile()
    if (window.showSaveFilePicker) {
      try {
        const handle = await window.showSaveFilePicker({
          suggestedName: 'hosts.json',
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
    a.download = 'hosts.json'
    a.click()
    URL.revokeObjectURL(url)
  } catch (e) {
    ElMessage.error(e.message || '导出失败')
  }
}

function doImport() {
  imexClose()
  fileEl.value?.click()
}

// 导入:读取 JSON 数组提交后端合并;文件解析失败等错误弹一条提示,列表不变
async function onImportFile(e) {
  const file = e.target.files?.[0]
  e.target.value = '' // 允许重复选择同一文件
  if (!file) return
  try {
    const arr = JSON.parse(await file.text())
    if (!Array.isArray(arr)) throw new Error('文件格式错误')
    const res = await importHosts(arr)
    ElMessage.success(`已导入 ${res.count} 台主机`)
    emit('refresh')
  } catch (err) {
    ElMessage.error(err.message || '导入失败')
  }
}
</script>

<template>
  <aside class="sidebar">
    <div style="padding: 10px 10px 0; display: flex; justify-content: flex-end">
      <el-button text style="padding: 8px" :icon="Operation" title="导入 / 导出主机列表" @click.stop="onImexClick" />
      <el-button text style="padding: 8px" :icon="Refresh" @click="$emit('refresh')" title="刷新列表" />
      <el-button text style="padding: 8px" :icon="DArrowLeft" @click="$emit('collapse')" title="收起主机列表" />
    </div>
    <div class="conn-list">
      <template v-for="g in groups" :key="g.name">
        <div class="group-header" :title="`点击${isCollapsed(g.name) ? '展开' : '收起'}`" @click="toggleGroup(g.name)">
          <el-icon class="group-arrow" :class="{ collapsed: isCollapsed(g.name) }">
            <ArrowDown />
          </el-icon>
          <span class="group-name">{{ g.name }}</span>
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
      <div v-if="!hosts.length" class="empty-hint">暂无主机,点击「新建连接」添加</div>
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
        <div class="ctx-item" @click.stop="run('connect')">连接</div>
        <div class="ctx-item" @click.stop="run('edit')">编辑</div>
        <div class="ctx-item" @click.stop="run('duplicate')">复制</div>
        <div class="ctx-item danger" @click.stop="run('delete')">删除</div>
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
        <div class="ctx-item" @click.stop="doExport">导出主机列表</div>
        <div class="ctx-item" @click.stop="doImport">导入主机列表</div>
      </div>
    </teleport>

    <!-- 导入文件选择(隐藏,系统文件选择框由菜单项触发) -->
    <input ref="fileEl" type="file" accept=".json,application/json" style="display: none" @change="onImportFile" />
  </aside>
</template>
