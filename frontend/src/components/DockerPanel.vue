<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Document, Refresh, Search, VideoPlay } from '@element-plus/icons-vue'
import { dockerContainers, dockerInfo, dockerInspect } from '../api'
import { t } from '../i18n'

const props = defineProps({
  session: { type: Object, required: true },
})
const emit = defineEmits(['open-tab'])

const available = ref(true)
const containers = ref([])
const loading = ref(false)
const error = ref('')
const filterText = ref('')

// 信息卡片(参考 K8s 面板):docker info 版本/计数 + Compose 项目列表
const info = ref(null)
const compose = ref(null)

async function loadInfo() {
  try {
    const res = await dockerInfo(props.session.id)
    info.value = res.available ? res.info : null
    compose.value = res.available ? res.compose : null
  } catch { /* 静默:卡片保留上次数据 */ }
}

const filteredContainers = computed(() => {
  const q = filterText.value.trim().toLowerCase()
  if (!q) return containers.value
  return containers.value.filter((c) => (c.name || '').toLowerCase().includes(q))
})

const drawerVisible = ref(false)
const inspectLoading = ref(false)
const inspectData = ref(null) // 解析后的 inspect[0] 对象
const inspectRaw = ref('')
const inspectError = ref('')
const inspectRow = ref(null) // 当前查看的容器行
// 抽屉标题:容器名 · 详情(详情文案随语言切换)
const inspectTitle = computed(() => (inspectRow.value ? `${inspectRow.value.name} · ${t('docker.detail')}` : ''))

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
    if (available.value) {
      loadInfo()
    } else {
      info.value = null
      compose.value = null
    }
  } catch (e) {
    error.value = e.message
  }
}

// inspect + 分卡片解析
async function openInspect(row) {
  inspectRow.value = row
  drawerVisible.value = true
  inspectLoading.value = true
  inspectData.value = null
  inspectRaw.value = ''
  inspectError.value = ''
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
    inspectError.value = e.message
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
    memory: memBytes > 0 ? fmtBytes(memBytes) : t('docker.unlimited'),
    memorySwap: memSwapBytes > 0 ? fmtBytes(memSwapBytes) : memSwapBytes === 0 ? t('docker.unlimited') : t('docker.sameAsMemory'),
    nanoCpus: nanoCpus > 0 ? (nanoCpus / 1e9).toFixed(2) + ' ' + t('docker.cores') : '-',
    cpuQuota: cpuQuota > 0 && cpuPeriod > 0 ? (cpuQuota / cpuPeriod).toFixed(2) + ' ' + t('docker.cores') : t('docker.unlimited'),
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
  if (diff < 0) return t('docker.justNow')
  const sec = Math.floor(diff / 1000)
  if (sec < 60) return t('docker.justNow')
  const min = Math.floor(sec / 60)
  if (min < 60) return t('docker.minAgo', { n: min })
  const hr = Math.floor(min / 60)
  if (hr < 24) return t('docker.hourAgo', { n: hr })
  const day = Math.floor(hr / 24)
  if (day < 30) return t('docker.dayAgo', { n: day })
  const mon = Math.floor(day / 30)
  if (mon < 12) return t('docker.monthAgo', { n: mon })
  return t('docker.yearAgo', { n: Math.floor(mon / 12) })
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
        :placeholder="t('docker.filter')"
        clearable
        style="width: 160px"
      />
      <el-button size="small" :icon="Refresh" :loading="loading" :title="t('common.manualRefresh')" @click="manualRefresh" />
    </div>

    <div v-if="error" class="panel-error">
      <el-alert :title="error" type="error" show-icon :closable="false" />
    </div>

    <template v-if="available">
      <!-- 信息卡片(参考 K8s 面板):版本/计数 + Compose 项目 -->
      <div class="metric-cards" v-if="info">
        <div class="metric-card">
          <div class="card-title">Docker</div>
          <div class="metric-row"><span class="muted">{{ t('docker.clientVersion') }}</span><span style="font-family:monospace">{{ info.client_version || '-' }}</span></div>
          <div class="metric-row"><span class="muted">{{ t('docker.serverVersion') }}</span><span style="font-family:monospace">{{ info.server_version || '-' }}</span></div>
          <div class="metric-row"><span class="muted">{{ t('docker.apiVersion') }}</span><span style="font-family:monospace">{{ info.api_version || '-' }}</span></div>
          <div class="metric-row"><span class="muted">{{ t('docker.storageDriver') }}</span><span style="font-family:monospace">{{ info.storage_driver || '-' }}</span></div>
          <div class="metric-row"><span class="muted">{{ t('docker.os') }}</span><span style="font-family:monospace;font-size:11px" :title="info.os">{{ info.os_type }} {{ info.arch }}</span></div>
          <div class="metric-row"><span class="muted">{{ t('docker.containers') }}</span><span style="font-family:monospace">{{ t('docker.containerStat', { running: info.running, total: info.containers }) }}</span></div>
          <div class="metric-row"><span class="muted">{{ t('docker.images') }}</span><span style="font-family:monospace">{{ info.images }}</span></div>
        </div>
        <div class="metric-card">
          <div class="card-title">Compose</div>
          <div class="metric-row"><span class="muted">{{ t('docker.command') }}</span><span style="font-family:monospace">{{ compose?.command || t('docker.notInstalled') }}</span></div>
          <div class="metric-row" v-if="compose?.version"><span class="muted">{{ t('docker.version') }}</span><span style="font-family:monospace">{{ compose.version }}</span></div>
          <template v-if="compose?.projects?.length">
            <div class="metric-row" v-for="p in compose.projects" :key="p.name">
              <span style="font-family:monospace;font-size:12px">{{ p.name }}</span>
              <span class="muted" style="font-size:11px" :title="p.config_files">{{ p.status }}</span>
            </div>
          </template>
          <div class="metric-row" v-else><span class="muted">{{ t('docker.projects') }}</span><span class="muted">{{ t('docker.noProjects') }}</span></div>
        </div>
      </div>

      <div class="docker-table">
        <el-table
          :data="filteredContainers"
          v-loading="loading"
          size="small"
          style="width: 100%"
          :empty-text="t('docker.empty')"
          @row-click="openInspect"
        >
          <el-table-column prop="name" :label="t('docker.colName')" min-width="120" show-overflow-tooltip />
          <el-table-column label="ID" width="110">
            <template #default="{ row }">
              <span style="font-family: monospace; font-size: 11px">{{ row.short_id }}</span>
            </template>
          </el-table-column>
          <el-table-column :label="t('docker.colImage')" min-width="130" show-overflow-tooltip>
            <template #default="{ row }">
              <span class="image-link">{{ row.image }}</span>
            </template>
          </el-table-column>
          <el-table-column prop="ports" :label="t('docker.colPorts')" min-width="100" show-overflow-tooltip />
          <el-table-column :label="t('docker.colCreated')" width="100">
            <template #default="{ row }">{{ fmtAgo(row.created_at) }}</template>
          </el-table-column>
          <el-table-column :label="t('common.colStatus')" width="100">
            <template #default="{ row }">
              <el-tag :type="statusType(row.state)" size="small">{{ row.status }}</el-tag>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <div class="muted" style="padding: 0 10px; font-size: 11px">
        {{ t('docker.hint') }}
      </div>
    </template>
    <template v-else-if="!loading">
      <div class="empty-state">
        <el-empty :description="t('docker.notAvailable')" :image-size="60" />
      </div>
    </template>

    <!-- inspect 抽屉:分卡片 -->
    <el-drawer v-model="drawerVisible" :title="inspectTitle" size="55%" :append-to-body="true">
      <div v-if="inspectLoading" class="muted" style="padding: 20px">{{ t('common.loading') }}</div>
      <template v-else>
        <!-- 顶部操作按钮 -->
        <div class="inspect-actions">
          <el-button size="small" type="primary" :icon="VideoPlay" @click="openLogs">
            {{ t('docker.viewLogs') }}
          </el-button>
          <el-button size="small" type="success" :icon="Document" @click="openShell">
            {{ t('docker.openShell') }}
          </el-button>
        </div>

        <!-- 镜像 + 状态(一行两列) -->
        <div class="inspect-row2">
          <div class="inspect-card" v-if="inspectImage">
            <div class="inspect-card-title">{{ t('docker.image') }}</div>
            <div class="metric-row"><span class="muted">{{ t('docker.imageName') }}</span><span>{{ inspectImage.image }}</span></div>
            <div class="metric-row"><span class="muted">{{ t('docker.containerId') }}</span><span style="font-family:monospace;font-size:11px">{{ inspectImage.id }}</span></div>
            <div class="metric-row"><span class="muted">{{ t('docker.colCreated') }}</span><span>{{ inspectImage.created }}</span></div>
            <div class="metric-row" v-if="inspectImage.entrypoint !== '[]'"><span class="muted">Entrypoint</span><span style="font-family:monospace;font-size:11px">{{ inspectImage.entrypoint }}</span></div>
            <div class="metric-row" v-if="inspectImage.cmd !== '[]'"><span class="muted">Cmd</span><span style="font-family:monospace;font-size:11px">{{ inspectImage.cmd }}</span></div>
          </div>
          <div class="inspect-card" v-if="inspectState">
            <div class="inspect-card-title">{{ t('docker.state') }}</div>
            <div class="metric-row">
              <span class="muted">{{ t('docker.currentState') }}</span>
              <el-tag :type="statusType(inspectState.status)" size="small">{{ inspectState.status }}</el-tag>
            </div>
            <div class="metric-row"><span class="muted">{{ t('docker.startedAt') }}</span><span>{{ inspectState.startedAt }}</span></div>
            <div class="metric-row"><span class="muted">{{ t('docker.restartCount') }}</span><span style="font-family:monospace">{{ inspectState.restartCount }}</span></div>
          </div>
        </div>

        <!-- 端口卡片 -->
        <div class="inspect-card" v-if="inspectPorts.length">
          <div class="inspect-card-title">{{ t('docker.portMapping') }}</div>
          <div v-for="p in inspectPorts" :key="p.port" class="metric-row">
            <span style="font-family:monospace">{{ p.port }}</span>
            <span class="muted">→ {{ p.host }}</span>
          </div>
        </div>

        <!-- 挂载卡片 -->
        <div class="inspect-card" v-if="inspectMounts.length">
          <div class="inspect-card-title">{{ t('docker.mounts') }}</div>
          <div v-for="(m, idx) in inspectMounts" :key="idx" class="inspect-mount-row">
            <span style="font-family:monospace;font-size:11px">{{ m.source }}</span>
            <span class="muted">→</span>
            <span style="font-family:monospace;font-size:11px">{{ m.dest }}</span>
            <el-tag size="small" :type="m.rw ? 'success' : 'info'" style="margin-left:auto">{{ m.rw ? 'rw' : 'ro' }}</el-tag>
          </div>
        </div>

        <!-- 网络卡片 -->
        <div class="inspect-card" v-if="inspectNetworks.length">
          <div class="inspect-card-title">{{ t('docker.networks') }}</div>
          <div v-for="n in inspectNetworks" :key="n.name" style="margin-bottom: 6px">
            <div style="font-weight:600;font-size:12px">{{ n.name }}</div>
            <div class="metric-row"><span class="muted">IP</span><span style="font-family:monospace">{{ n.ip }}</span></div>
            <div class="metric-row"><span class="muted">{{ t('docker.gateway') }}</span><span style="font-family:monospace">{{ n.gateway }}</span></div>
          </div>
        </div>

        <!-- 重启策略 + 资源限制(一行两列) -->
        <div class="inspect-row2">
          <div class="inspect-card" v-if="inspectRestart">
            <div class="inspect-card-title">{{ t('docker.restartPolicy') }}</div>
            <div class="metric-row"><span class="muted">{{ t('docker.policy') }}</span><span style="font-family:monospace">{{ inspectRestart.name }}</span></div>
            <div class="metric-row" v-if="inspectRestart.name === 'on-failure'"><span class="muted">{{ t('docker.maxRetry') }}</span><span style="font-family:monospace">{{ inspectRestart.maxRetry }}</span></div>
            <div class="metric-row"><span class="muted">{{ t('docker.autoRemove') }}</span><span>{{ inspectRestart.autoRemove ? t('common.yes') : t('common.no') }}</span></div>
          </div>
          <div class="inspect-card" v-if="inspectResources">
            <div class="inspect-card-title">{{ t('docker.resources') }}</div>
            <div class="metric-row"><span class="muted">{{ t('docker.memory') }}</span><span style="font-family:monospace">{{ inspectResources.memory }}</span></div>
            <div class="metric-row"><span class="muted">Swap</span><span style="font-family:monospace">{{ inspectResources.memorySwap }}</span></div>
            <div class="metric-row"><span class="muted">CPU</span><span style="font-family:monospace">{{ inspectResources.nanoCpus }}</span></div>
            <div class="metric-row"><span class="muted">{{ t('docker.cpuQuota') }}</span><span style="font-family:monospace">{{ inspectResources.cpuQuota }}</span></div>
          </div>
        </div>

        <!-- 环境变量卡片 -->
        <div class="inspect-card" v-if="inspectEnv.length">
          <div class="inspect-card-title">{{ t('docker.env', { n: inspectEnv.length }) }}</div>
          <div v-for="e in inspectEnv" :key="e.key" class="env-row">
            <span class="env-key">{{ e.key }}</span>
            <span class="env-val">{{ e.value }}</span>
          </div>
        </div>

        <!-- 运行配置卡片 -->
        <div class="inspect-card" v-if="inspectRunConfig">
          <div class="inspect-card-title">{{ t('docker.runConfig') }}</div>
          <div class="metric-row"><span class="muted">{{ t('docker.networkMode') }}</span><span style="font-family:monospace">{{ inspectRunConfig.networkMode }}</span></div>
          <div class="metric-row"><span class="muted">{{ t('docker.workingDir') }}</span><span style="font-family:monospace">{{ inspectRunConfig.workingDir }}</span></div>
          <div class="metric-row"><span class="muted">{{ t('docker.runUser') }}</span><span style="font-family:monospace">{{ inspectRunConfig.user }}</span></div>
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
          <div class="inspect-card-title">{{ t('docker.rawJson') }}</div>
          <pre class="inspect-pre">{{ inspectError ? t('docker.loadFailed', { msg: inspectError }) : inspectRaw }}</pre>
        </div>
      </template>
    </el-drawer>
  </div>
</template>
