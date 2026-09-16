<script setup>
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { addForward, deleteForward, hostInfo, k8sResources, listForwards } from '../api'

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
    ElMessage.success(`已启动:127.0.0.1:${res.listen_port} → ${k8sNs.value}/${target}:${p}`)
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
    if (!ns) throw new Error('命名空间不能为空')
    if (!target) throw new Error('目标不能为空(如 svc/my-app)')
    if (!port) throw new Error('服务端口不能为空')
    const res = await addForward(props.session.id, {
      type: 'k8s',
      listen_port: listen,
      namespace: ns,
      k8s_target: target,
      k8s_port: port,
    })
    k8sForm.listen_port = ''
    ElMessage.success(`K8s 转发已建立:127.0.0.1:${res.listen_port} → ${ns}/${target}:${port}`)
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
    throw new Error(`${name} 必须是 1-65535 的整数`)
  }
  return n
}

async function addLocal() {
  try {
    const listen = toPort(localForm.listen_port, '监听端口')
    const remotePort = toPort(localForm.remote_port, '目标端口')
    if (!localForm.remote_host.trim()) throw new Error('目标主机不能为空')
    await addForward(props.session.id, {
      type: 'local',
      listen_port: listen,
      remote_host: localForm.remote_host.trim(),
      remote_port: remotePort,
    })
    localForm.listen_port = ''
    localForm.remote_host = ''
    localForm.remote_port = ''
    ElMessage.success(`本地转发已建立:127.0.0.1:${listen}`)
    refresh()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function addSocks() {
  try {
    const listen = toPort(socksPort.value, '监听端口')
    await addForward(props.session.id, { type: 'socks5', listen_port: listen })
    socksPort.value = ''
    ElMessage.success(`SOCKS5 代理已启动:127.0.0.1:${listen}`)
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
      当前会话:{{ session.username }}@{{ session.host }}
      <template v-if="session.jumpName">,经跳板机 {{ session.jumpName }} 中转</template>
      <template v-else>,直连</template>
      。所有监听仅绑定 127.0.0.1。
    </el-alert>

    <el-alert
      v-if="forwardDisabled"
      type="error"
      :closable="false"
      style="flex: none"
    >
      ⚠ 该主机 sshd 已禁止 TCP 转发(AllowTcpForwarding={{ sshConfig?.allow_tcp_forwarding || 'no' }}{{ sshConfig?.disable_forwarding === 'yes' ? ', DisableForwarding=yes' : '' }}),本地转发与 SOCKS5 将无法使用;下方的 K8s 服务转发不受影响。
    </el-alert>
    <el-alert
      v-else-if="forwardUnknown"
      type="warning"
      :closable="false"
      style="flex: none"
    >
      无法确认 sshd 转发配置,转发可能不可用。
    </el-alert>

    <el-card class="forward-card" shadow="never">
      <template #header>
        K8s 服务端口转发(kubectl port-forward)
        <el-tag v-if="forwardDisabled" size="small" type="success" style="margin-left: 8px">不受 TCP 转发禁令影响</el-tag>
      </template>

      <div v-if="k8sAvailable === false" class="muted" style="font-size: 12px; margin-bottom: 8px">
        未在目标主机检测到 kubectl(PATH 与登录环境均未找到),此功能需要主机上已部署 kubectl 且可访问集群。
      </div>
      <div v-else-if="k8sListError" class="muted" style="font-size: 12px; margin-bottom: 8px; color: var(--el-color-warning)">
        资源列表获取异常:{{ k8sListError }}
      </div>

      <div class="fwd-form">
        <el-select
          v-model="k8sNs"
          filterable
          allow-create
          default-first-option
          placeholder="命名空间"
          style="width: 170px"
          @change="loadK8sTargets"
        >
          <el-option v-for="n in k8sNsList" :key="n" :label="n" :value="n" />
        </el-select>
        <el-button :loading="k8sLoading" :disabled="k8sAvailable === false" @click="loadK8sTargets">
          刷新资源
        </el-button>
        <span class="muted" style="font-size: 12px">本地端口自动分配;非 Running 的 Pod 不可启动</span>
      </div>

      <el-table
        :data="k8sTargets"
        size="small"
        max-height="240"
        v-loading="k8sLoading"
        empty-text="该命名空间下暂无 Service / Pod"
        style="margin-top: 8px"
      >
        <el-table-column label="类型" width="80">
          <template #default="{ row }">
            <el-tag size="small" :type="row.kind === 'svc' ? 'primary' : 'success'">
              {{ row.kind === 'svc' ? 'Service' : 'Pod' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="name" label="名称" min-width="150" show-overflow-tooltip />
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <span v-if="row.status" :style="podNotReady(row) ? 'color: var(--el-color-danger)' : ''">{{ row.status }}</span>
            <span v-else class="muted">-</span>
          </template>
        </el-table-column>
        <el-table-column label="启动转发(点击端口)" min-width="260">
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
            <span v-if="!row.ports || !row.ports.length" class="muted" style="font-size: 12px">无声明端口,</span>
            <el-input v-model="row.custom" size="small" placeholder="自定义端口" style="width: 110px" />
            <el-button size="small" text type="primary" :disabled="!String(row.custom).trim()" @click="startForward(row, row.custom)">
              启动
            </el-button>
          </template>
        </el-table-column>
      </el-table>

      <el-table
        :data="k8sActive"
        size="small"
        empty-text="暂无活动中的 K8s 转发"
        style="margin-top: 8px"
      >
        <el-table-column label="目标" min-width="190">
          <template #default="{ row }">
            {{ row.namespace }}/{{ row.k8s_target }}:{{ row.k8s_port }}
          </template>
        </el-table-column>
        <el-table-column label="本地地址" width="130">
          <template #default="{ row }">127.0.0.1:{{ row.listen_port }}</template>
        </el-table-column>
        <el-table-column label="连接" width="90">
          <template #default="{ row }">
            <span :style="row.stats?.active > 0 ? 'color: var(--el-color-success); font-weight: 600' : ''">{{ row.stats?.active ?? 0 }}</span>
            / {{ row.stats?.total ?? 0 }}
          </template>
        </el-table-column>
        <el-table-column label="流量 ↓/↑" width="130">
          <template #default="{ row }">
            {{ fmtB(row.stats?.recv_bytes) }} / {{ fmtB(row.stats?.sent_bytes) }}
          </template>
        </el-table-column>
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <el-tooltip v-if="row.last_error" :content="row.last_error" placement="top">
              <el-tag size="small" type="warning">最近报错</el-tag>
            </el-tooltip>
            <el-tag v-else size="small" type="success">运行中</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="70">
          <template #default="{ row }">
            <el-button size="small" type="danger" link @click="stop(row)">停止</el-button>
          </template>
        </el-table-column>
      </el-table>

      <el-collapse style="margin-top: 8px">
        <el-collapse-item title="手动添加(命名端口 / 其他资源类型 / 指定本地端口)" name="adv">
          <div class="fwd-form">
            <el-input v-model="k8sForm.namespace" placeholder="命名空间" style="width: 110px" />
            <el-input v-model="k8sForm.target" placeholder="目标,如 svc/my-app" style="width: 150px" />
            <el-input v-model="k8sForm.port" placeholder="服务端口/名" style="width: 100px" />
            <el-input v-model="k8sForm.listen_port" placeholder="监听端口(空=自动)" style="width: 140px" />
            <el-button type="primary" :disabled="k8sAvailable === false" @click="addK8s">添加转发</el-button>
          </div>
        </el-collapse-item>
      </el-collapse>
    </el-card>

    <el-card class="forward-card" shadow="never">
      <template #header>本地端口转发(ssh -L)</template>
      <div class="fwd-form">
        <el-input v-model="localForm.listen_port" placeholder="监听端口" />
        <span class="muted">→</span>
        <el-input v-model="localForm.remote_host" placeholder="目标主机" style="width: 140px" />
        <el-input v-model="localForm.remote_port" placeholder="目标端口" />
        <el-button type="primary" @click="addLocal">添加转发</el-button>
      </div>
      <div v-for="f in forwards.filter((x) => x.type === 'local')" :key="f.id" class="fwd-list-item">
        <span>127.0.0.1:{{ f.listen_port }} → {{ f.remote_host }}:{{ f.remote_port }}</span>
        <el-button size="small" type="danger" link @click="stop(f)">停止</el-button>
      </div>
      <div v-if="!forwards.some((x) => x.type === 'local')" class="muted" style="font-size: 12px">
        暂无本地转发。添加后,访问本机监听端口的流量将经 SSH 隧道到达目标。
      </div>
    </el-card>

    <el-card class="forward-card" shadow="never">
      <template #header>SOCKS5 动态代理(ssh -D)</template>
      <div class="fwd-form">
        <el-input v-model="socksPort" placeholder="监听端口" />
        <el-button type="primary" @click="addSocks">启动代理</el-button>
      </div>
      <div v-for="f in forwards.filter((x) => x.type === 'socks5')" :key="f.id" class="fwd-list-item">
        <span>SOCKS5 127.0.0.1:{{ f.listen_port }}</span>
        <el-button size="small" type="danger" link @click="stop(f)">停止</el-button>
      </div>
      <div v-if="!forwards.some((x) => x.type === 'socks5')" class="muted" style="font-size: 12px">
        启动后可将浏览器/程序代理指向 127.0.0.1:端口,流量经该 SSH 主机转出。
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.forward-panel :deep(.el-collapse-item__content) {
  padding-bottom: 4px;
}
</style>
