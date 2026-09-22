<script setup>
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { addForward, deleteForward, hostInfo, k8sResources, listForwards } from '../api'
import { t } from '../i18n'

const props = defineProps({
  session: { type: Object, required: true },
})

const forwards = ref([])
const loading = ref(false)
const sshConfig = ref(null)
const configLoaded = ref(false)

// 转发能力:禁止 / 允许 / 未知(仅在配置加载完成后判定,避免加载中闪现"无法确认")
const forwardDisabled = computed(() => {
  const c = sshConfig.value
  if (!c) return false
  return c.disable_forwarding === 'yes' || c.allow_tcp_forwarding === 'no'
})
const forwardUnknown = computed(() => {
  if (!configLoaded.value) return false
  const c = sshConfig.value
  return !c || (!c.allow_tcp_forwarding && !c.disable_forwarding && c.source === '')
})

const localForm = reactive({ listen_port: '', remote_host: '', remote_port: '' })
const socksPort = ref('')
// 转发模式:auto=探测 sshd 自动选择(禁止即原始模式),std=标准通道,
// raw=原始模式(仅 SSH exec 通道,绕过 AllowTcpForwarding=no)
const localMode = ref('auto')
const socksMode = ref('auto')

// ---------- K8s 服务转发(OpenLens 式:选资源 → 一键启动 → 活动/统计) ----------
const k8sNs = ref('default')
const k8sNsList = ref([])
const k8sTargets = ref([])
const k8sAvailable = ref(null) // null=未探测
const k8sListError = ref('')
const k8sLoading = ref(false)
// 手动添加(命名端口 / 其他 kind / 指定本地端口)
const k8sForm = reactive({ namespace: 'default', target: '', port: '', listen_port: '' })

const k8sActive = computed(() => forwards.value.filter((f) => f.type === 'k8s'))

async function refresh() {
  loading.value = true
  try {
    forwards.value = await listForwards(props.session.id)
  } catch {
    /* 轮询失败静默:下次 tick 自动重试 */
  } finally {
    loading.value = false
  }
}

async function loadSSHConfig() {
  try {
    const info = await hostInfo(props.session.id)
    sshConfig.value = info.ssh_config || null
  } catch {
    sshConfig.value = null
  }
  configLoaded.value = true
}

// probeK8s 探测 kubectl 可用性并拉取命名空间列表(面板打开时一次)。
async function probeK8s() {
  try {
    const res = await k8sResources(props.session.id)
    k8sAvailable.value = !!res.available
    k8sListError.value = res.error || ''
    k8sNsList.value = res.namespaces || []
    if (k8sAvailable.value) loadK8sTargets()
  } catch {
    k8sAvailable.value = null // 探测失败不阻断,交由后端在操作时报错
  }
}

async function loadK8sTargets() {
  const ns = k8sNs.value.trim()
  if (!ns || k8sAvailable.value === false) return
  k8sLoading.value = true
  try {
    const res = await k8sResources(props.session.id, ns)
    k8sAvailable.value = !!res.available
    k8sListError.value = res.error || ''
    k8sTargets.value = [
      ...(res.services || []).map((t) => ({ kind: 'svc', ...t, custom: '' })),
      ...(res.pods || []).map((t) => ({ kind: 'pod', ...t, custom: '' })),
    ]
  } catch (e) {
    k8sListError.value = e.message
  } finally {
    k8sLoading.value = false
  }
}

function podNotReady(row) {
  return row.kind === 'pod' && row.status !== 'Running'
}

// startForward 一键转发:本地端口传 0,由内核自动分配,响应回填真实端口。
async function startForward(row, port) {
  const p = String(port || '').trim()
  if (!p) return
  try {
    const target = `${row.kind}/${row.name}`
    const res = await addForward(props.session.id, {
      type: 'k8s',
      listen_port: 0,
      namespace: k8sNs.value.trim(),
      k8s_target: target,
      k8s_port: p,
    })
    ElMessage.success(t('fwd.started', { port: res.listen_port, target: `${k8sNs.value}/${target}:${p}` }))
    refresh()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function addK8s() {
  try {
    const ns = k8sForm.namespace.trim()
    const target = k8sForm.target.trim()
    const port = k8sForm.port.trim()
    const listen = Number(k8sForm.listen_port) || 0
    if (!ns) throw new Error(t('fwd.errNsEmpty'))
    if (!target) throw new Error(t('fwd.errTargetEmpty'))
    if (!port) throw new Error(t('fwd.errPortEmpty'))
    const res = await addForward(props.session.id, {
      type: 'k8s',
      listen_port: listen,
      namespace: ns,
      k8s_target: target,
      k8s_port: port,
    })
    k8sForm.listen_port = ''
    ElMessage.success(t('fwd.k8sEstablished', { port: res.listen_port, target: `${ns}/${target}:${port}` }))
    refresh()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

function fmtB(n) {
  if (!n) return '0 B'
  if (n < 1024) return n + ' B'
  const units = ['KB', 'MB', 'GB', 'TB']
  let v = n
  let i = -1
  do {
    v /= 1024
    i++
  } while (v >= 1024 && i < units.length - 1)
  return v.toFixed(v >= 100 ? 0 : 1) + ' ' + units[i]
}

function toPort(v, name) {
  const n = Number(v)
  if (!Number.isInteger(n) || n < 1 || n > 65535) {
    throw new Error(t('fwd.errPortRange', { name }))
  }
  return n
}

async function addLocal() {
  try {
    const listen = toPort(localForm.listen_port, t('fwd.listenPort'))
    const remotePort = toPort(localForm.remote_port, t('fwd.remotePort'))
    if (!localForm.remote_host.trim()) throw new Error(t('fwd.errHostEmpty'))
    await addForward(props.session.id, {
      type: 'local',
      listen_port: listen,
      remote_host: localForm.remote_host.trim(),
      remote_port: remotePort,
      mode: localMode.value,
    })
    localForm.listen_port = ''
    localForm.remote_host = ''
    localForm.remote_port = ''
    ElMessage.success(t('fwd.localEstablished', { port: listen }))
    refresh()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function addSocks() {
  try {
    const listen = toPort(socksPort.value, t('fwd.listenPort'))
    await addForward(props.session.id, { type: 'socks5', listen_port: listen, mode: socksMode.value })
    socksPort.value = ''
    ElMessage.success(t('fwd.socksStarted', { port: listen }))
    refresh()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function stop(f) {
  try {
    await deleteForward(props.session.id, f.id)
    refresh()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

// 活动转发统计轮询(连接数/流量),2s 一次
let pollTimer = null
onMounted(() => {
  refresh()
  loadSSHConfig()
  probeK8s()
  pollTimer = setInterval(refresh, 2000)
})
onUnmounted(() => clearInterval(pollTimer))
</script>

<template>
  <div class="forward-panel">
    <el-alert
      type="info"
      :closable="false"
      style="flex: none"
    >
      {{ t('fwd.currentSession', { target: `${session.username}@${session.host}` }) }}<template v-if="session.jumpName">{{ t('fwd.viaJumpRelay', { name: session.jumpName }) }}</template><template v-else>{{ t('fwd.direct') }}</template>{{ t('fwd.bindLocal') }}
    </el-alert>

    <el-alert
      v-if="forwardDisabled"
      type="warning"
      :closable="false"
      style="flex: none"
    >
      {{ t('fwd.forwardDisabled', {
        tcp: sshConfig?.allow_tcp_forwarding || 'no',
        dis: sshConfig?.disable_forwarding === 'yes' ? ', DisableForwarding=yes' : '',
      }) }}
    </el-alert>
    <el-alert
      v-else-if="forwardUnknown"
      type="warning"
      :closable="false"
      style="flex: none"
    >
      {{ t('fwd.forwardUnknown') }}
    </el-alert>

    <el-card class="forward-card" shadow="never">
      <template #header>
        {{ t('fwd.k8sTitle') }}
        <el-tag v-if="forwardDisabled" size="small" type="success" style="margin-left: 8px">{{ t('fwd.unaffectedByBan') }}</el-tag>
      </template>

      <div v-if="k8sAvailable === false" class="muted" style="font-size: 12px; margin-bottom: 8px">
        {{ t('fwd.noKubectl') }}
      </div>
      <div v-else-if="k8sListError" class="muted" style="font-size: 12px; margin-bottom: 8px; color: var(--el-color-warning)">
        {{ t('fwd.listError', { err: k8sListError }) }}
      </div>

      <div class="fwd-form">
        <el-select
          v-model="k8sNs"
          filterable
          allow-create
          default-first-option
          :placeholder="t('fwd.namespace')"
          style="width: 170px"
          @change="loadK8sTargets"
        >
          <el-option v-for="n in k8sNsList" :key="n" :label="n" :value="n" />
        </el-select>
        <el-button :loading="k8sLoading" :disabled="k8sAvailable === false" @click="loadK8sTargets">
          {{ t('fwd.refreshResources') }}
        </el-button>
        <span class="muted" style="font-size: 12px">{{ t('fwd.autoPortHint') }}</span>
      </div>

      <el-table
        :data="k8sTargets"
        size="small"
        max-height="240"
        v-loading="k8sLoading"
        :empty-text="t('fwd.noTargets')"
        style="margin-top: 8px"
      >
        <el-table-column :label="t('common.colType')" width="80">
          <template #default="{ row }">
            <el-tag size="small" :type="row.kind === 'svc' ? 'primary' : 'success'">
              {{ row.kind === 'svc' ? 'Service' : 'Pod' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="name" :label="t('common.colName')" min-width="150" show-overflow-tooltip />
        <el-table-column :label="t('common.colStatus')" width="90">
          <template #default="{ row }">
            <span v-if="row.status" :style="podNotReady(row) ? 'color: var(--el-color-danger)' : ''">{{ row.status }}</span>
            <span v-else class="muted">-</span>
          </template>
        </el-table-column>
        <el-table-column :label="t('fwd.startFwdCol')" min-width="260">
          <template #default="{ row }">
            <el-button
              v-for="p in row.ports"
              :key="p.port"
              size="small"
              text
              type="primary"
              :disabled="podNotReady(row)"
              @click="startForward(row, p.port)"
            >
              {{ p.port }}<template v-if="p.name">({{ p.name }})</template>
            </el-button>
            <span v-if="!row.ports || !row.ports.length" class="muted" style="font-size: 12px">{{ t('fwd.noDeclaredPorts') }}</span>
            <el-input v-model="row.custom" size="small" :placeholder="t('fwd.customPort')" style="width: 110px" />
            <el-button size="small" text type="primary" :disabled="!String(row.custom).trim()" @click="startForward(row, row.custom)">
              {{ t('fwd.start') }}
            </el-button>
          </template>
        </el-table-column>
      </el-table>

      <el-table
        :data="k8sActive"
        size="small"
        :empty-text="t('fwd.noActive')"
        style="margin-top: 8px"
      >
        <el-table-column :label="t('fwd.colTarget')" min-width="190">
          <template #default="{ row }">
            {{ row.namespace }}/{{ row.k8s_target }}:{{ row.k8s_port }}
          </template>
        </el-table-column>
        <el-table-column :label="t('fwd.colLocalAddr')" width="130">
          <template #default="{ row }">127.0.0.1:{{ row.listen_port }}</template>
        </el-table-column>
        <el-table-column :label="t('fwd.colConns')" width="90">
          <template #default="{ row }">
            <span :style="row.stats?.active > 0 ? 'color: var(--el-color-success); font-weight: 600' : ''">{{ row.stats?.active ?? 0 }}</span>
            / {{ row.stats?.total ?? 0 }}
          </template>
        </el-table-column>
        <el-table-column :label="t('fwd.colTraffic')" width="130">
          <template #default="{ row }">
            {{ fmtB(row.stats?.recv_bytes) }} / {{ fmtB(row.stats?.sent_bytes) }}
          </template>
        </el-table-column>
        <el-table-column :label="t('common.colStatus')" width="90">
          <template #default="{ row }">
            <el-tooltip v-if="row.last_error" :content="row.last_error" placement="top">
              <el-tag size="small" type="warning">{{ t('fwd.lastErrorTag') }}</el-tag>
            </el-tooltip>
            <el-tag v-else size="small" type="success">{{ t('fwd.running') }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column :label="t('common.colActions')" width="70">
          <template #default="{ row }">
            <el-button size="small" type="danger" link @click="stop(row)">{{ t('fwd.stop') }}</el-button>
          </template>
        </el-table-column>
      </el-table>

      <el-collapse style="margin-top: 8px">
        <el-collapse-item :title="t('fwd.manualAdd')" name="adv">
          <div class="fwd-form">
            <el-input v-model="k8sForm.namespace" :placeholder="t('fwd.namespace')" style="width: 110px" />
            <el-input v-model="k8sForm.target" :placeholder="t('fwd.targetPlaceholder')" style="width: 150px" />
            <el-input v-model="k8sForm.port" :placeholder="t('fwd.portPlaceholder')" style="width: 100px" />
            <el-input v-model="k8sForm.listen_port" :placeholder="t('fwd.listenPlaceholder')" style="width: 140px" />
            <el-button type="primary" :disabled="k8sAvailable === false" @click="addK8s">{{ t('fwd.addForward') }}</el-button>
          </div>
        </el-collapse-item>
      </el-collapse>
    </el-card>

    <el-card class="forward-card" shadow="never">
      <template #header>{{ t('fwd.localTitle') }}</template>
      <div class="fwd-form">
        <el-input v-model="localForm.listen_port" :placeholder="t('fwd.listenPort')" />
        <span class="muted">→</span>
        <el-input v-model="localForm.remote_host" :placeholder="t('fwd.remoteHost')" style="width: 140px" />
        <el-input v-model="localForm.remote_port" :placeholder="t('fwd.remotePort')" />
        <el-select v-model="localMode" style="width: 110px" :title="t('fwd.modeTitle')">
          <el-option :label="t('fwd.modeAuto')" value="auto" />
          <el-option :label="t('fwd.modeStd')" value="std" />
          <el-option :label="t('fwd.modeRaw')" value="raw" />
        </el-select>
        <el-button type="primary" @click="addLocal">{{ t('fwd.addForward') }}</el-button>
      </div>
      <div v-for="f in forwards.filter((x) => x.type === 'local')" :key="f.id" class="fwd-list-item">
        <span>
          127.0.0.1:{{ f.listen_port }} → {{ f.remote_host }}:{{ f.remote_port }}
          <el-tag v-if="f.mode === 'raw'" size="small" type="warning" style="margin-left: 4px">{{ t('fwd.rawTag') }}</el-tag>
        </span>
        <el-button size="small" type="danger" link @click="stop(f)">{{ t('fwd.stop') }}</el-button>
      </div>
      <div v-if="!forwards.some((x) => x.type === 'local')" class="muted" style="font-size: 12px">
        {{ t('fwd.noLocal') }}
      </div>
    </el-card>

    <el-card class="forward-card" shadow="never">
      <template #header>{{ t('fwd.socksTitle') }}</template>
      <div class="fwd-form">
        <el-input v-model="socksPort" :placeholder="t('fwd.listenPort')" />
        <el-select v-model="socksMode" style="width: 110px" :title="t('fwd.modeTitle')">
          <el-option :label="t('fwd.modeAuto')" value="auto" />
          <el-option :label="t('fwd.modeStd')" value="std" />
          <el-option :label="t('fwd.modeRaw')" value="raw" />
        </el-select>
        <el-button type="primary" @click="addSocks">{{ t('fwd.startProxy') }}</el-button>
      </div>
      <div v-for="f in forwards.filter((x) => x.type === 'socks5')" :key="f.id" class="fwd-list-item">
        <span>
          SOCKS5 127.0.0.1:{{ f.listen_port }}
          <el-tag v-if="f.mode === 'raw'" size="small" type="warning" style="margin-left: 4px">{{ t('fwd.rawTag') }}</el-tag>
        </span>
        <el-button size="small" type="danger" link @click="stop(f)">{{ t('fwd.stop') }}</el-button>
      </div>
      <div v-if="!forwards.some((x) => x.type === 'socks5')" class="muted" style="font-size: 12px">
        {{ t('fwd.noSocks') }}
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.forward-panel :deep(.el-collapse-item__content) {
  padding-bottom: 4px;
}
</style>
