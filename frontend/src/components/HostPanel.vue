<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { Refresh } from '@element-plus/icons-vue'
import { hostInfo, metricsWSUrl } from '../api'

const props = defineProps({
  session: { type: Object, required: true },
})

// 静态信息(host-info 一次性获取)
const info = ref(null)
const infoError = ref('')

// 动态指标(WebSocket 推送)
const metrics = ref(null)
const wsStatus = ref('connecting') // connecting | open | closed

// 历史数据(sparkline 用,最近 30 个点)
const MAX_POINTS = 30
const history = reactive({
  cpu: [],
  mem: [],
  netRx: [],
  netTx: [],
})
const peaks = reactive({ cpu: 0, mem: 0, netRx: 0, netTx: 0 })

let ws = null
let reconnectTimer = null
let disposed = false

// ---------- 静态信息 ----------
async function loadInfo() {
  infoError.value = ''
  try {
    info.value = await hostInfo(props.session.id)
  } catch (e) {
    infoError.value = e.message
  }
}

// ---------- WebSocket ----------
function connectWS() {
  if (disposed) return
  wsStatus.value = 'connecting'
  ws = new WebSocket(metricsWSUrl(props.session.id))
  ws.onopen = () => { wsStatus.value = 'open' }
  ws.onmessage = (ev) => {
    try {
      const m = JSON.parse(ev.data)
      metrics.value = m
      push('cpu', m.cpu_pct)
      push('mem', m.mem_pct)
      push('netRx', m.net_rx_bps)
      push('netTx', m.net_tx_bps)
    } catch { /* 忽略 */ }
  }
  ws.onclose = () => {
    wsStatus.value = 'closed'
    if (!disposed) {
      reconnectTimer = setTimeout(connectWS, 3000)
    }
  }
  ws.onerror = () => {}
}

function push(key, val) {
  const arr = history[key]
  arr.push(val)
  if (arr.length > MAX_POINTS) arr.shift()
  if (val > peaks[key]) peaks[key] = val
}

// ---------- Sparkline SVG ----------
function sparkPoints(arr, max) {
  if (!arr || arr.length < 2) return ''
  const w = 120, h = 30
  const range = max > 0 ? max : 1
  const step = w / (MAX_POINTS - 1)
  const startIdx = MAX_POINTS - arr.length
  return arr
    .map((v, i) => {
      const x = (startIdx + i) * step
      const y = h - (Math.min(v, range) / range) * h
      return `${x.toFixed(1)},${y.toFixed(1)}`
    })
    .join(' ')
}

const cpuPoints = computed(() => sparkPoints(history.cpu, 100))
const memPoints = computed(() => sparkPoints(history.mem, 100))
const maxNet = computed(() => Math.max(peaks.netRx, peaks.netTx, 1))
const netRxPoints = computed(() => sparkPoints(history.netRx, maxNet.value))
const netTxPoints = computed(() => sparkPoints(history.netTx, maxNet.value))

function fmtBps(bps) {
  if (!bps || bps < 0) return '0'
  if (bps < 1024) return bps.toFixed(0) + ' B/s'
  if (bps < 1048576) return (bps / 1024).toFixed(1) + ' KB/s'
  if (bps < 1073741824) return (bps / 1048576).toFixed(1) + ' MB/s'
  return (bps / 1073741824).toFixed(2) + ' GB/s'
}

function fmtUptime(seconds) {
  if (!seconds || seconds <= 0) return '-'
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const parts = []
  if (d > 0) parts.push(d + ' 天')
  if (h > 0) parts.push(h + ' 小时')
  parts.push(m + ' 分钟')
  return parts.join(' ')
}

function fmtMB(mb) {
  if (!mb || mb <= 0) return '0 MB'
  if (mb >= 1024) return (mb / 1024).toFixed(1) + ' GB'
  return mb.toFixed(0) + ' MB'
}

function memColor(pct) {
  if (pct > 95) return '#f56c6c'
  if (pct > 80) return '#e6a23c'
  return '#409eff'
}

// ---------- 手动刷新 ----------
const refreshing = ref(false)
async function manualRefresh() {
  refreshing.value = true
  await loadInfo()
  if (ws) { ws.close() } // 触发重连
  refreshing.value = false
}

// ---------- SSH 配置派生 ----------
// sshd 是否禁止 TCP 转发(AllowTcpForwarding=no 或 DisableForwarding=yes)
const forwardDisabled = computed(() => {
  const c = info.value?.ssh_config
  if (!c) return false
  return c.disable_forwarding === 'yes' || c.allow_tcp_forwarding === 'no'
})
const forwardUnknown = computed(() => {
  const c = info.value?.ssh_config
  return !c || (!c.allow_tcp_forwarding && !c.disable_forwarding && c.source === '')
})
const forwardLabel = computed(() => {
  if (forwardDisabled.value) return '已禁止'
  if (forwardUnknown.value) return '未知'
  return '允许'
})
const forwardColor = computed(() => {
  if (forwardDisabled.value) return '#f56c6c'
  if (forwardUnknown.value) return '#909399'
  return '#67c23a'
})
const sshConfigSourceLabel = computed(() => {
  const src = info.value?.ssh_config?.source
  if (src === 'sshd -T') return '· 生效配置'
  if (src === 'config-file') return '· 配置文件(非生效值)'
  return ''
})

onMounted(() => {
  loadInfo()
  connectWS()
})

onBeforeUnmount(() => {
  disposed = true
  if (reconnectTimer) clearTimeout(reconnectTimer)
  if (ws) { ws.onclose = null; ws.close() }
})
</script>

<template>
  <div class="host-panel">
    <div class="panel-title">
      主机详情 · {{ session.name }}
      <span class="ws-dot" :class="wsStatus"></span>
      <span class="muted" style="font-weight: 400; font-size: 12px">
        {{ wsStatus === 'open' ? '实时监控已连接' : wsStatus === 'connecting' ? '监控连接中…' : '监控断开,重连中…' }}
      </span>
      <span style="flex: 1"></span>
      <el-button size="small" :icon="Refresh" :loading="refreshing" title="手动刷新" @click="manualRefresh" />
    </div>

    <div v-if="infoError" class="muted" style="padding: 0 12px">{{ infoError }}</div>

    <template v-if="info">
      <!-- 基础信息 + 资源使用 -->
      <div class="metric-cards">
        <div class="metric-card">
          <div class="card-title">基础信息</div>
          <div class="metric-row"><span class="muted">主机名</span><span>{{ info.hostname }}</span></div>
          <div class="metric-row"><span class="muted">系统</span><span>{{ info.os || '-' }}</span></div>
          <div class="metric-row"><span class="muted">内核</span><span>{{ info.kernel || '-' }}</span></div>
          <div class="metric-row"><span class="muted">架构</span><span>{{ info.arch || '-' }}</span></div>
          <div class="metric-row"><span class="muted">运行时长</span><span>{{ fmtUptime(metrics?.uptime_seconds) }}</span></div>
        </div>
        <div class="metric-card">
          <div class="card-title">资源使用</div>
          <div class="prog-row">
            <span class="prog-label">CPU</span>
            <el-progress :percentage="metrics?.cpu_pct || 0" :format="(p) => parseFloat(p.toFixed(2)) + '%'" :color="memColor(metrics?.cpu_pct || 0)" :stroke-width="12" style="flex:1" />
          </div>
          <div class="prog-row">
            <span class="prog-label">MEM</span>
            <el-progress :percentage="metrics?.mem_pct || 0" :format="(p) => parseFloat(p.toFixed(2)) + '%'" :color="memColor(metrics?.mem_pct || 0)" :stroke-width="12" style="flex:1" />
          </div>
          <div class="metric-row" style="padding-top: 0">
            <span></span>
            <span class="muted" style="font-size: 11px">{{ fmtMB(metrics?.mem_used_mb) }} / {{ fmtMB(metrics?.mem_total_mb) }}</span>
          </div>
          <div class="prog-row" v-if="metrics?.swap_total_mb > 0">
            <span class="prog-label">SWAP</span>
            <el-progress :percentage="metrics?.swap_pct || 0" :format="(p) => parseFloat(p.toFixed(2)) + '%'" :color="memColor(metrics?.swap_pct || 0)" :stroke-width="12" style="flex:1" />
          </div>
          <div class="metric-row" style="margin-top: 4px">
            <span class="muted">Load</span>
            <span style="font-family: monospace">{{ metrics?.load_1m?.toFixed(2) ?? '-' }} / {{ metrics?.load_5m?.toFixed(2) ?? '-' }} / {{ metrics?.load_15m?.toFixed(2) ?? '-' }}</span>
          </div>
          <div class="metric-row" v-if="info.cpu_model">
            <span class="muted">CPU</span>
            <span style="font-size: 12px; text-align: right">{{ info.cpu_model }} ({{ info.cpu_cores }}核)</span>
          </div>
        </div>
      </div>

      <!-- SSH 配置 + 实时监控(两列并排) -->
      <div class="metric-cards">
        <div class="metric-card">
          <div class="card-title">SSH 配置 <span class="muted" style="font-weight:400; font-size:11px">{{ sshConfigSourceLabel }}</span></div>
          <div class="metric-row">
            <span class="muted">TCP 转发</span>
            <span :style="{ color: forwardColor, fontWeight: 600 }">{{ forwardLabel }}</span>
          </div>
          <div class="metric-row"><span class="muted">DisableForwarding</span><span>{{ info.ssh_config?.disable_forwarding || '-' }}</span></div>
          <div class="metric-row"><span class="muted">PermitOpen</span><span>{{ info.ssh_config?.permit_open || 'any' }}</span></div>
          <div class="metric-row"><span class="muted">Agent 转发</span><span>{{ info.ssh_config?.allow_agent_forwarding || '-' }}</span></div>
        </div>

        <div class="metric-card">
          <div class="card-title">实时监控 <span class="muted" style="font-weight:400; font-size:11px">5s 推送</span></div>
          <div class="sparkline-row">
            <span class="spark-label">CPU</span>
            <svg class="sparkline" viewBox="0 0 120 30" preserveAspectRatio="none">
              <polyline :points="cpuPoints" fill="none" stroke="#409eff" stroke-width="1.5" />
            </svg>
            <span class="spark-cur">{{ metrics?.cpu_pct?.toFixed(1) ?? '-' }}%</span>
            <span class="spark-peak">峰值 {{ peaks.cpu.toFixed(1) }}%</span>
          </div>
          <div class="sparkline-row">
            <span class="spark-label">MEM</span>
            <svg class="sparkline" viewBox="0 0 120 30" preserveAspectRatio="none">
              <polyline :points="memPoints" fill="none" stroke="#67c23a" stroke-width="1.5" />
            </svg>
            <span class="spark-cur">{{ metrics?.mem_pct?.toFixed(1) ?? '-' }}%</span>
            <span class="spark-peak">峰值 {{ peaks.mem.toFixed(1) }}%</span>
          </div>
          <div class="sparkline-row">
            <span class="spark-label">NET↑</span>
            <svg class="sparkline" viewBox="0 0 120 30" preserveAspectRatio="none">
              <polyline :points="netTxPoints" fill="none" stroke="#e6a23c" stroke-width="1.5" />
            </svg>
            <span class="spark-cur">{{ fmtBps(metrics?.net_tx_bps) }}</span>
            <span class="spark-peak">峰值 {{ fmtBps(peaks.netTx) }}</span>
          </div>
          <div class="sparkline-row">
            <span class="spark-label">NET↓</span>
            <svg class="sparkline" viewBox="0 0 120 30" preserveAspectRatio="none">
              <polyline :points="netRxPoints" fill="none" stroke="#409eff" stroke-width="1.5" />
            </svg>
            <span class="spark-cur">{{ fmtBps(metrics?.net_rx_bps) }}</span>
            <span class="spark-peak">峰值 {{ fmtBps(peaks.netRx) }}</span>
          </div>
        </div>
      </div>

      <!-- 磁盘 -->
      <div class="metric-card">
        <div class="card-title">磁盘</div>
        <el-table :data="info.disks" size="small" style="width: 100%" empty-text="无磁盘数据">
          <el-table-column prop="device" label="设备" min-width="100" show-overflow-tooltip />
          <el-table-column prop="fs_type" label="类型" width="70" />
          <el-table-column prop="size" label="大小" width="65" />
          <el-table-column prop="used" label="已用" width="65" />
          <el-table-column prop="avail" label="可用" width="65" />
          <el-table-column label="使用率" width="80">
            <template #default="{ row }">
              <span :style="{ color: row.use_num > 90 ? '#f56c6c' : row.use_num > 80 ? '#e6a23c' : 'inherit' }">
                {{ row.use_pct }}
              </span>
            </template>
          </el-table-column>
          <el-table-column prop="mount" label="挂载点" min-width="100" show-overflow-tooltip />
        </el-table>
      </div>

      <!-- 进程 TOP20 -->
      <div class="metric-card">
        <div class="card-title">进程 TOP20 (CPU)</div>
        <el-table :data="info.processes" size="small" style="width: 100%" empty-text="无进程数据">
          <el-table-column prop="user" label="用户" width="70" />
          <el-table-column prop="pid" label="PID" width="70" />
          <el-table-column label="CPU%" width="65">
            <template #default="{ row }">{{ row.cpu_pct.toFixed(1) }}%</template>
          </el-table-column>
          <el-table-column label="内存%" width="65">
            <template #default="{ row }">{{ row.mem_pct.toFixed(1) }}%</template>
          </el-table-column>
          <el-table-column prop="command" label="命令" min-width="180" show-overflow-tooltip />
        </el-table>
      </div>
    </template>
  </div>
</template>
