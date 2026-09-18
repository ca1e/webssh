<script setup>
import { computed, reactive, ref } from 'vue'
import { ArrowDown, Connection, DArrowLeft, Monitor, Refresh } from '@element-plus/icons-vue'
import { useContextMenu } from '../contextMenu'
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
</script>

<template>
  <aside class="sidebar">
    <div style="padding: 10px 10px 0; display: flex; justify-content: flex-end">
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
  </aside>
</template>
