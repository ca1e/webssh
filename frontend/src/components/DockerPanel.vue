<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Document, Refresh, Search, VideoPlay } from '@element-plus/icons-vue'
import { dockerContainers, dockerInspect } from '../api'

const props = defineProps({
  session: { type: Object, required: true },
})
const emit = defineEmits(['open-tab'])

const available = ref(true)
const containers = ref([])
const loading = ref(false)
const error = ref('')
const filterText = ref('')

const filteredContainers = computed(() => {
  const q = filterText.value.trim().toLowerCase()
  if (!q) return containers.value
  return containers.value.filter((c) => (c.name || '').toLowerCase().includes(q))
})

const drawerVisible = ref(false)
const inspectLoading = ref(false)
const inspectData = ref(null) // 解析后的 inspect[0] 对象
const inspectRaw = ref('')
const inspectTitle = ref('')
const inspectRow = ref(null) // 当前查看的容器行

let timer = null

async function autoRefresh() {
  try {
    const res = await dockerContainers(props.session.id)
    available.value = res.available
    containers.value = res.containers || []
    error.value = ''
  } catch { /* 静默 */ }
}

async function manualRefresh() {
  loading.value = true
  await refresh()
  loading.value = false
  resetTimer()
}

async function refresh() {
  error.value = ''
  try {
    const res = await dockerContainers(props.session.id)
    available.value = res.available
    containers.value = res.containers || []
  } catch (e) {
    error.value = e.message
  }
}

// inspect + 分卡片解析
async function openInspect(row) {
  inspectRow.value = row
  inspectTitle.value = `${row.name} · 详情`
  drawerVisible.value = true
  inspectLoading.value = true
  inspectData.value = null
  inspectRaw.value = ''
  try {
    const raw = await dockerInspect(props.session.id, row.id)
    let parsed = raw
    if (typeof raw === 'string') {
      try { parsed = JSON.parse(raw) } catch { /* raw text */ }
    }
    if (Array.isArray(parsed) && parsed.length > 0) {
      inspectData.value = parsed[0]
    }
    inspectRaw.value = typeof raw === 'string' ? raw : JSON.stringify(parsed, null, 2)
  } catch (e) {
    inspectRaw.value = '加载失败: ' + e.message
  } finally {
    inspectLoading.value = false
  }
}

// inspect 分卡片数据
const inspectImage = computed(() => {
  const d = inspectData.value
  if (!d) return null
  return {
    image: d.Config?.Image || d.Image || '-',
    id: (d.Id || '').slice(0, 19) + '…',
    entrypoint: JSON.stringify(d.Config?.Entrypoint || []),
    cmd: JSON.stringify(d.Config?.Cmd || []),
    created: d.Created || '-',
  }
})

const inspectMounts = computed(() => {
  const d = inspectData.value
  if (!d?.Mounts?.length) return []
  return d.Mounts.map((m) => ({
    source: m.Source || m.Name || '-',
    dest: m.Destination || '-',
    type: m.Type || '-',
    rw: m.RW !== false,
  }))
})

const inspectNetworks = computed(() => {
  const d = inspectData.value
  if (!d?.NetworkSettings?.Networks) return []
  return Object.entries(d.NetworkSettings.Networks).map(([name, n]) => ({
    name,
    ip: n.IPAddress || '-',
    gateway: n.Gateway || '-',
    mac: n.MacAddress || '-',
  }))
})

const inspectLabels = computed(() => {
  const labels = inspectData.value?.Config?.Labels
  if (!labels) return []
  return Object.entries(labels).map(([k, v]) => ({ key: k, value: v }))
})

const inspectState = computed(() => {
  const d = inspectData.value
  if (!d?.State) return null
  return {
    status: d.State.Status || '-',
    startedAt: d.State.StartedAt || '-',
    restartCount: d.RestartCount ?? 0,
  }
})

const inspectRestart = computed(() => {
  const d = inspectData.value
  if (!d?.HostConfig?.RestartPolicy) return null
  const rp = d.HostConfig.RestartPolicy
  return {
    name: rp.Name || 'no',
    maxRetry: rp.MaximumRetryCount ?? 0,
    autoRemove: d.HostConfig.AutoRemove === true,
  }
})

const inspectResources = computed(() => {
  const d = inspectData.value
  if (!d?.HostConfig) return null
  const hc = d.HostConfig
  const memBytes = hc.Memory || 0
  const memSwapBytes = hc.MemorySwap || 0
  const nanoCpus = hc.NanoCpus || 0
  const cpuQuota = hc.CpuQuota || 0
  const cpuPeriod = hc.CpuPeriod || 0
  return {
    memory: memBytes > 0 ? fmtBytes(memBytes) : '无限制',
    memorySwap: memSwapBytes > 0 ? fmtBytes(memSwapBytes) : memSwapBytes === 0 ? '无限制' : '等于内存',
    nanoCpus: nanoCpus > 0 ? (nanoCpus / 1e9).toFixed(2) + ' 核' : '-',
    cpuQuota: cpuQuota > 0 && cpuPeriod > 0 ? (cpuQuota / cpuPeriod).toFixed(2) + ' 核' : '无限制',
  }
})

const inspectEnv = computed(() => {
  const env = inspectData.value?.Config?.Env
  if (!env || !env.length) return []
  return env.map((e) => {
    const idx = e.indexOf('=')
    return { key: idx > 0 ? e.slice(0, idx) : e, value: idx > 0 ? e.slice(idx + 1) : '' }
  })
})

const inspectRunConfig = computed(() => {
  const d = inspectData.value
  if (!d?.HostConfig) return null
  return {
    networkMode: d.HostConfig.NetworkMode || '-',
    workingDir: d.Config?.WorkingDir || '-',
    user: d.Config?.User || 'root',
  }
})

const inspectPorts = computed(() => {
  const bindings = inspectData.value?.NetworkSettings?.Ports || inspectData.value?.Config?.ExposedPorts
  if (!bindings) return []
  return Object.entries(bindings).map(([port, binds]) => {
    let host = '-'
    if (Array.isArray(binds) && binds.length > 0) {
      host = binds.map((b) => `${b.HostIp || '0.0.0.0'}:${b.HostPort}`).join(', ')
    } else {
      host = '(exposed)'
    }
    return { port, host }
  })
})

// 日志/Shell 按钮 → 通知 App.vue 打开新 tab
function openLogs() {
  const row = inspectRow.value
  if (!row) return
  emit('open-tab', { type: 'container-logs', sid: props.session.id, cid: row.id, name: row.name })
  drawerVisible.value = false
}

function openShell() {
  const row = inspectRow.value
  if (!row) return
  emit('open-tab', { type: 'container-shell', sid: props.session.id, cid: row.id, name: row.name })
  drawerVisible.value = false
}

function statusType(state) {
  switch (state) {
    case 'running': return 'success'
    case 'exited': return 'info'
    case 'created': return 'primary'
    case 'paused': return 'warning'
    default: return 'warning'
  }
}

function fmtAgo(createdStr) {
  if (!createdStr) return '-'
  const d = new Date(createdStr)
  if (isNaN(d)) return createdStr
  const diff = Date.now() - d.getTime()
  if (diff < 0) return '刚刚'
  const sec = Math.floor(diff / 1000)
  if (sec < 60) return '刚刚'
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min} 分钟前`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr} 小时前`
  const day = Math.floor(hr / 24)
  if (day < 30) return `${day} 天前`
  const mon = Math.floor(day / 30)
  if (mon < 12) return `${mon} 个月前`
  return `${Math.floor(mon / 12)} 年前`
}

function fmtBytes(bytes) {
  if (!bytes || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = bytes
  let i = 0
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++ }
  return v.toFixed(v >= 100 ? 0 : 1) + ' ' + units[i]
}

function resetTimer() {
  if (timer) clearInterval(timer)
  timer = setInterval(autoRefresh, 5000)
}

onMounted(() => {
  manualRefresh()
  resetTimer()
})

onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
})
</script>

<template>
  <div class="docker-panel">
    <div class="panel-title">
      Docker · {{ session.name }}
      <span style="flex: 1"></span>
      <el-input
        v-model="filterText"
        size="small"
        :icon="Search"
        placeholder="过滤容器名"
        clearable
        style="width: 160px"
      />
      <el-button size="small" :icon="Refresh" :loading="loading" title="手动刷新" @click="manualRefresh" />
    </div>

    <div v-if="error" class="panel-error">
      <el-alert :title="error" type="error" show-icon :closable="false" />
    </div>

    <template v-if="available">
      <div class="docker-table">
        <el-table
          :data="filteredContainers"
          v-loading="loading"
          size="small"
          style="width: 100%"
          empty-text="无容器"
          @row-click="openInspect"
        >
          <el-table-column prop="name" label="容器名" min-width="120" show-overflow-tooltip />
          <el-table-column label="ID" width="110">
            <template #default="{ row }">
              <span style="font-family: monospace; font-size: 11px">{{ row.short_id }}</span>
            </template>
          </el-table-column>
          <el-table-column label="镜像" min-width="130" show-overflow-tooltip>
            <template #default="{ row }">
              <span class="image-link">{{ row.image }}</span>
            </template>
          </el-table-column>
          <el-table-column prop="ports" label="端口" min-width="100" show-overflow-tooltip />
          <el-table-column label="创建时间" width="100">
            <template #default="{ row }">{{ fmtAgo(row.created_at) }}</template>
          </el-table-column>
          <el-table-column label="状态" width="100">
            <template #default="{ row }">
              <el-tag :type="statusType(row.state)" size="small">{{ row.status }}</el-tag>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <div class="muted" style="padding: 0 10px; font-size: 11px">
        点击容器行查看详情 · 5s 自动刷新(无动画)
      </div>
    </template>
    <template v-else-if="!loading">
      <div class="empty-docker">
        <el-empty description="Docker 未安装或未运行" :image-size="60" />
      </div>
    </template>

    <!-- inspect 抽屉:分卡片 -->
    <el-drawer v-model="drawerVisible" :title="inspectTitle" size="55%" :append-to-body="true">
      <div v-if="inspectLoading" class="muted" style="padding: 20px">加载中…</div>
      <template v-else>
        <!-- 顶部操作按钮 -->
        <div class="inspect-actions">
          <el-button size="small" type="primary" :icon="VideoPlay" @click="openLogs">
            查看日志
          </el-button>
          <el-button size="small" type="success" :icon="Document" @click="openShell">
            打开 Shell
          </el-button>
        </div>

        <!-- 镜像 + 状态(一行两列) -->
        <div class="inspect-row2">
          <div class="inspect-card" v-if="inspectImage">
            <div class="inspect-card-title">镜像</div>
            <div class="metric-row"><span class="muted">镜像名</span><span>{{ inspectImage.image }}</span></div>
            <div class="metric-row"><span class="muted">容器 ID</span><span style="font-family:monospace;font-size:11px">{{ inspectImage.id }}</span></div>
            <div class="metric-row"><span class="muted">创建时间</span><span>{{ inspectImage.created }}</span></div>
            <div class="metric-row" v-if="inspectImage.entrypoint !== '[]'"><span class="muted">Entrypoint</span><span style="font-family:monospace;font-size:11px">{{ inspectImage.entrypoint }}</span></div>
            <div class="metric-row" v-if="inspectImage.cmd !== '[]'"><span class="muted">Cmd</span><span style="font-family:monospace;font-size:11px">{{ inspectImage.cmd }}</span></div>
          </div>
          <div class="inspect-card" v-if="inspectState">
            <div class="inspect-card-title">容器状态</div>
            <div class="metric-row">
              <span class="muted">当前状态</span>
              <el-tag :type="statusType(inspectState.status)" size="small">{{ inspectState.status }}</el-tag>
            </div>
            <div class="metric-row"><span class="muted">本次启动</span><span>{{ inspectState.startedAt }}</span></div>
            <div class="metric-row"><span class="muted">重启次数</span><span style="font-family:monospace">{{ inspectState.restartCount }}</span></div>
          </div>
        </div>

        <!-- 端口卡片 -->
        <div class="inspect-card" v-if="inspectPorts.length">
          <div class="inspect-card-title">端口映射</div>
          <div v-for="p in inspectPorts" :key="p.port" class="metric-row">
            <span style="font-family:monospace">{{ p.port }}</span>
            <span class="muted">→ {{ p.host }}</span>
          </div>
        </div>

        <!-- 挂载卡片 -->
        <div class="inspect-card" v-if="inspectMounts.length">
          <div class="inspect-card-title">挂载</div>
          <div v-for="(m, idx) in inspectMounts" :key="idx" class="inspect-mount-row">
            <span style="font-family:monospace;font-size:11px">{{ m.source }}</span>
            <span class="muted">→</span>
            <span style="font-family:monospace;font-size:11px">{{ m.dest }}</span>
            <el-tag size="small" :type="m.rw ? 'success' : 'info'" style="margin-left:auto">{{ m.rw ? 'rw' : 'ro' }}</el-tag>
          </div>
        </div>

        <!-- 网络卡片 -->
        <div class="inspect-card" v-if="inspectNetworks.length">
          <div class="inspect-card-title">网络</div>
          <div v-for="n in inspectNetworks" :key="n.name" style="margin-bottom: 6px">
            <div style="font-weight:600;font-size:12px">{{ n.name }}</div>
            <div class="metric-row"><span class="muted">IP</span><span style="font-family:monospace">{{ n.ip }}</span></div>
            <div class="metric-row"><span class="muted">网关</span><span style="font-family:monospace">{{ n.gateway }}</span></div>
          </div>
        </div>

        <!-- 重启策略 + 资源限制(一行两列) -->
        <div class="inspect-row2">
          <div class="inspect-card" v-if="inspectRestart">
            <div class="inspect-card-title">重启策略</div>
            <div class="metric-row"><span class="muted">策略</span><span style="font-family:monospace">{{ inspectRestart.name }}</span></div>
            <div class="metric-row" v-if="inspectRestart.name === 'on-failure'"><span class="muted">最大重试</span><span style="font-family:monospace">{{ inspectRestart.maxRetry }}</span></div>
            <div class="metric-row"><span class="muted">退出自动删除</span><span>{{ inspectRestart.autoRemove ? '是' : '否' }}</span></div>
          </div>
          <div class="inspect-card" v-if="inspectResources">
            <div class="inspect-card-title">资源限制</div>
            <div class="metric-row"><span class="muted">内存</span><span style="font-family:monospace">{{ inspectResources.memory }}</span></div>
            <div class="metric-row"><span class="muted">Swap</span><span style="font-family:monospace">{{ inspectResources.memorySwap }}</span></div>
            <div class="metric-row"><span class="muted">CPU</span><span style="font-family:monospace">{{ inspectResources.nanoCpus }}</span></div>
            <div class="metric-row"><span class="muted">CPU配额</span><span style="font-family:monospace">{{ inspectResources.cpuQuota }}</span></div>
          </div>
        </div>

        <!-- 环境变量卡片 -->
        <div class="inspect-card" v-if="inspectEnv.length">
          <div class="inspect-card-title">环境变量 ({{ inspectEnv.length }})</div>
          <div v-for="e in inspectEnv" :key="e.key" class="env-row">
            <span class="env-key">{{ e.key }}</span>
            <span class="env-val">{{ e.value }}</span>
          </div>
        </div>

        <!-- 运行配置卡片 -->
        <div class="inspect-card" v-if="inspectRunConfig">
          <div class="inspect-card-title">运行配置</div>
          <div class="metric-row"><span class="muted">网络模式</span><span style="font-family:monospace">{{ inspectRunConfig.networkMode }}</span></div>
          <div class="metric-row"><span class="muted">工作目录</span><span style="font-family:monospace">{{ inspectRunConfig.workingDir }}</span></div>
          <div class="metric-row"><span class="muted">运行用户</span><span style="font-family:monospace">{{ inspectRunConfig.user }}</span></div>
        </div>

        <!-- Labels 卡片 -->
        <div class="inspect-card" v-if="inspectLabels.length">
          <div class="inspect-card-title">Labels</div>
          <div v-for="l in inspectLabels" :key="l.key" class="metric-row">
            <span class="muted" style="font-family:monospace;font-size:11px">{{ l.key }}</span>
            <span style="font-family:monospace;font-size:11px">{{ l.value }}</span>
          </div>
        </div>

        <!-- 原始 JSON -->
        <div class="inspect-card">
          <div class="inspect-card-title">原始 JSON</div>
          <pre class="inspect-pre">{{ inspectRaw }}</pre>
        </div>
      </template>
    </el-drawer>
  </div>
</template>
