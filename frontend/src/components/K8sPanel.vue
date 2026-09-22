<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { Refresh, Search } from '@element-plus/icons-vue'
import { k8sResources, k8sState } from '../api'
import { t } from '../i18n'

// K8s 面板:集群状态卡片 + Pod 列表(watch 增量流)。交互参考 Docker 面板,
// 事件模型借鉴 Freelens 的 kube-watch-event:List 快照全量替换,
// ADDED/MODIFIED upsert、DELETED 删除;断线 3s 后自动重连并重新拉快照。

const props = defineProps({
  session: { type: Object, required: true },
})
const emit = defineEmits(['open-tab'])

// ---------- 集群状态(打开面板时拉一次 + 手动刷新,不自动轮询) ----------
const state = ref(null)
const stateLoading = ref(false)

async function loadState(force = false) {
  stateLoading.value = true
  try {
    state.value = await k8sState(props.session.id, force)
  } catch {
    /* 静默:面板顶部保留上次状态 */
  } finally {
    stateLoading.value = false
  }
}

const distLabel = computed(() => {
  const map = { eks: 'EKS', gke: 'GKE', aks: 'AKS', openshift: 'OpenShift', k3s: 'K3s', kind: 'Kind', minikube: 'Minikube' }
  return map[state.value?.distribution] || 'Kubernetes'
})

// ---------- 命名空间选择(复用转发面板的 k8s/resources 端点) ----------
const namespaces = ref([])
const selectedNs = ref('default') // 默认看 default 命名空间,"全部命名空间"经下拉切换

async function loadNamespaces() {
  try {
    const res = await k8sResources(props.session.id)
    if (Array.isArray(res.namespaces)) namespaces.value = res.namespaces
  } catch {
    /* RBAC 受限时列表为空,选择器仍支持手输(allow-create) */
  }
}

// ---------- Pod watch 增量流 ----------
const podMap = reactive(new Map()) // key: ns/name → 行数据
// connecting | open | closed
const wsStatus = ref('connecting')
const streamError = ref('')
const filterText = ref('')
const nowTick = ref(Date.now())

function podKey(p) {
  return `${p.metadata?.namespace || ''}/${p.metadata?.name || ''}`
}

function rowFromPod(p) {
  const statuses = p.status?.containerStatuses || []
  const total = (p.spec?.containers || []).length || statuses.length
  const ready = statuses.filter((s) => s.ready).length
  const restarts = statuses.reduce((a, s) => a + (s.restartCount || 0), 0)
  return {
    key: podKey(p),
    ns: p.metadata?.namespace || '',
    name: p.metadata?.name || '',
    phase: p.status?.phase || 'Unknown',
    ready: `${ready}/${total}`,
    restarts,
    node: p.spec?.nodeName || '-',
    ip: p.status?.podIP || '-',
    created: p.metadata?.creationTimestamp || '',
    containers: (p.spec?.containers || []).map((c) => c.name),
  }
}

function applyDoc(doc) {
  // kubectl --output-watch-events 的两种形态:
  //   1) 部分版本的初始列表以「含 items 数组的 ADDED 事件」分批发出(object 是
  //      一个 List)——逐个 upsert,不清空(分批多帧,清空会丢前面的批次);
  //   2) 无 type 的 List 快照文档 —— 全量替换;
  //   3) 正常 watch 事件 object 是单个 Pod —— upsert / DELETED 删除。
  const obj = doc.object
  if (doc.kind === 'List' && !doc.type) {
    podMap.clear()
    for (const it of doc.items || []) podMap.set(podKey(it), rowFromPod(it))
    return
  }
  if (Array.isArray(obj?.items)) {
    for (const it of obj.items) podMap.set(podKey(it), rowFromPod(it))
    return
  }
  if (!doc.type || !obj) return
  if (doc.type === 'DELETED') podMap.delete(podKey(obj))
  else podMap.set(podKey(obj), rowFromPod(obj))
}

let ws = null
let reconnectTimer = null
let disposed = false

function closeWs() {
  if (ws) {
    ws.onclose = null
    ws.close()
    ws = null
  }
}

function connect() {
  if (disposed) return
  closeWs()
  if (reconnectTimer) {
    clearTimeout(reconnectTimer)
    reconnectTimer = null
  }
  wsStatus.value = 'connecting'
  streamError.value = ''
  podMap.clear() // 换命名空间/重连后重新拉快照,清掉上一次 watch 的数据
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  const url = `${proto}://${location.host}/api/sessions/${props.session.id}/k8s/pods/ws?namespace=${encodeURIComponent(selectedNs.value)}`
  ws = new WebSocket(url)
  ws.onopen = () => {
    wsStatus.value = 'open'
  }
  ws.onmessage = (ev) => {
    try {
      const doc = JSON.parse(ev.data)
      if (doc.type === 'exit') {
        if (doc.error) streamError.value = doc.error
        return
      }
      applyDoc(doc)
    } catch {
      /* 忽略坏帧 */
    }
  }
  ws.onclose = () => {
    wsStatus.value = 'closed'
    if (!disposed && !reconnectTimer) reconnectTimer = setTimeout(connect, 3000)
  }
  ws.onerror = () => {}
}

watch(selectedNs, () => connect())

// ---------- 过滤与展示 ----------
const podRows = computed(() => {
  const q = filterText.value.trim().toLowerCase()
  const rows = [...podMap.values()]
  const hit = q ? rows.filter((r) => r.name.toLowerCase().includes(q)) : rows
  return hit.sort((a, b) => (a.ns + '/' + a.name).localeCompare(b.ns + '/' + b.name))
})

function phaseType(phase) {
  if (phase === 'Running') return 'success'
  if (phase === 'Pending') return 'warning'
  if (phase === 'Succeeded' || phase === 'Failed') return 'info'
  return 'danger'
}

// Pod 年龄(30s 粒度刷新,参照 kubectl age 的粗粒度习惯)
function fmtAge(created) {
  if (!created) return '-'
  const diff = Math.max(0, (nowTick.value - new Date(created).getTime()) / 1000)
  if (diff < 60) return `${Math.floor(diff)}s`
  if (diff < 3600) return `${Math.floor(diff / 60)}m`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h`
  return `${Math.floor(diff / 86400)}d`
}

// ---------- 行操作 ----------
function openLogs(row) {
  emit('open-tab', { type: 'pod-logs', sid: props.session.id, ns: row.ns, pod: row.name, name: row.name })
}

function openShell(row) {
  if (row.phase !== 'Running') return
  emit('open-tab', { type: 'pod-shell', sid: props.session.id, ns: row.ns, pod: row.name, name: row.name })
}

let ageTimer = null

onMounted(() => {
  loadState()
  loadNamespaces()
  connect()
  ageTimer = setInterval(() => (nowTick.value = Date.now()), 30000)
})

onBeforeUnmount(() => {
  disposed = true
  if (ageTimer) clearInterval(ageTimer)
  if (reconnectTimer) clearTimeout(reconnectTimer)
  closeWs()
})
</script>

<template>
  <div class="k8s-panel">
    <div class="panel-title">
      Kubernetes · {{ session.name }}
      <span style="flex: 1"></span>
      <span class="ws-dot" :class="wsStatus" :title="t('k8s.streamStatus', { status: wsStatus })"></span>
      <el-button size="small" :icon="Refresh" :loading="stateLoading" :title="t('k8s.reprobe')" @click="loadState(true)" />
    </div>

    <!-- kubectl 不可用 / 探测失败(空态样式与 Docker 面板一致) -->
    <div v-if="state && !state.available" class="empty-state">
      <el-empty :description="state.error || t('k8s.unreachable')" :image-size="60" />
    </div>

    <!-- 集群状态卡片 -->
    <div class="metric-cards" v-if="state && state.available">
      <div class="metric-card">
        <div class="card-title">{{ distLabel }}</div>
        <div class="metric-row"><span class="muted">Context</span><span style="font-family:monospace;font-size:11px">{{ state.context || '-' }}</span></div>
        <div class="metric-row"><span class="muted">{{ t('k8s.cluster') }}</span><span style="font-family:monospace;font-size:11px">{{ state.cluster || '-' }}</span></div>
        <div class="metric-row"><span class="muted">{{ t('k8s.version') }}</span><span style="font-family:monospace;font-size:11px">{{ state.version || '-' }}</span></div>
      </div>
      <div class="metric-card">
        <div class="card-title">{{ t('k8s.nodesAddr') }}</div>
        <div class="metric-row"><span class="muted">{{ t('k8s.nodes') }}</span><span style="font-family:monospace">{{ state.ready_nodes }}/{{ state.node_count }} Ready</span></div>
        <div class="metric-row"><span class="muted">API Server</span><span style="font-family:monospace;font-size:11px">{{ state.server || '-' }}</span></div>
        <div class="metric-row" v-if="state.error"><span class="muted">{{ t('k8s.hint') }}</span><span class="k8s-warn">{{ state.error }}</span></div>
      </div>
    </div>

    <!-- Pod 列表 -->
    <template v-if="state && state.available">
      <div class="k8s-pod-toolbar">
        <el-select
          v-model="selectedNs"
          size="small"
          filterable
          allow-create
          default-first-option
          style="width: 180px"
        >
          <el-option :label="t('k8s.allNamespaces')" value="_all" />
          <el-option v-for="ns in namespaces" :key="ns" :label="ns" :value="ns" />
        </el-select>
        <el-input
          v-model="filterText"
          size="small"
          :icon="Search"
          :placeholder="t('k8s.filterPods')"
          clearable
          style="width: 150px"
        />
      </div>

      <div v-if="streamError" class="panel-error">
        <el-alert :title="streamError" type="warning" show-icon :closable="false" />
      </div>

      <div class="docker-table">
        <el-table :data="podRows" size="small" style="width: 100%" :empty-text="t('k8s.noPods')">
          <el-table-column prop="name" label="Pod" min-width="140" show-overflow-tooltip />
          <el-table-column prop="ns" :label="t('k8s.colNamespace')" min-width="100" show-overflow-tooltip />
          <el-table-column :label="t('common.colStatus')" width="90">
            <template #default="{ row }">
              <el-tag :type="phaseType(row.phase)" size="small">{{ row.phase }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column :label="t('k8s.colReady')" width="60">
            <template #default="{ row }"><span style="font-family:monospace">{{ row.ready }}</span></template>
          </el-table-column>
          <el-table-column :label="t('k8s.colRestarts')" width="55">
            <template #default="{ row }"><span style="font-family:monospace">{{ row.restarts }}</span></template>
          </el-table-column>
          <el-table-column :label="t('k8s.colAge')" width="60">
            <template #default="{ row }">{{ fmtAge(row.created) }}</template>
          </el-table-column>
          <el-table-column :label="t('common.colActions')" width="120" align="center">
            <template #default="{ row }">
              <el-button size="small" text type="primary" @click.stop="openLogs(row)">{{ t('k8s.logs') }}</el-button>
              <el-button
                size="small"
                text
                type="success"
                :disabled="row.phase !== 'Running'"
                :title="row.phase !== 'Running' ? t('k8s.onlyRunningHint') : t('k8s.containers', { names: row.containers.join(', ') })"
                @click.stop="openShell(row)"
              >
                {{ t('k8s.terminal') }}
              </el-button>
            </template>
          </el-table-column>
        </el-table>
      </div>
      <div class="muted" style="padding: 0 10px; font-size: 11px">
        {{ t('k8s.hint2') }}
      </div>
    </template>
  </div>
</template>
