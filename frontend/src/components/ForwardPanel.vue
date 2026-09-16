<script setup>
import { computed, onMounted, reactive, ref, watch } from 'vue'
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

// ---------- K8s 服务转发 ----------
const k8sForm = reactive({ namespace: 'default', target: '', port: '', listen_port: '' })
const k8sAvailable = ref(null) // null=未探测
const k8sNsOptions = ref([])
const k8sTargetOptions = ref([])
const k8sListError = ref('')
const k8sLoading = ref(false)

async function probeK8s() {
  try {
    const res = await k8sResources(props.session.id)
    k8sAvailable.value = !!res.available
    k8sListError.value = res.error || ''
    k8sNsOptions.value = (res.namespaces || []).map((it) => ({ value: it.name, desc: '' }))
  } catch {
    k8sAvailable.value = null // 探测失败不阻断,交由后端在添加时报错
  }
}

async function loadK8sTargets() {
  const ns = k8sForm.namespace.trim()
  if (!ns || k8sAvailable.value === false) return
  k8sLoading.value = true
  try {
    const res = await k8sResources(props.session.id, ns)
    k8sAvailable.value = !!res.available
    k8sListError.value = res.error || ''
    k8sTargetOptions.value = [
      ...(res.services || []).map((it) => ({ value: `svc/${it.name}`, desc: it.desc || '' })),
      ...(res.pods || []).map((it) => ({ value: `pod/${it.name}`, desc: it.desc || '' })),
    ]
  } catch (e) {
    k8sListError.value = e.message
  } finally {
    k8sLoading.value = false
  }
}

const nsSuggestions = (qs, cb) =>
  cb(k8sNsOptions.value.filter((n) => !qs || n.value.includes(qs)).slice(0, 20))
const targetSuggestions = (qs, cb) =>
  cb(k8sTargetOptions.value.filter((n) => !qs || n.value.includes(qs)).slice(0, 30))

// 命名空间变化(去抖)后自动刷新目标建议,不依赖 blur 时机
let nsDebounce = null
watch(() => k8sForm.namespace, () => {
  clearTimeout(nsDebounce)
  nsDebounce = setTimeout(loadK8sTargets, 400)
})

async function addK8s() {
  try {
    const listen = toPort(k8sForm.listen_port, '监听端口')
    const ns = k8sForm.namespace.trim()
    const target = k8sForm.target.trim()
    const port = k8sForm.port.trim()
    if (!ns) throw new Error('命名空间不能为空')
    if (!target) throw new Error('目标不能为空(如 svc/my-app)')
    if (!port) throw new Error('服务端口不能为空')
    await addForward(props.session.id, {
      type: 'k8s',
      listen_port: listen,
      namespace: ns,
      k8s_target: target,
      k8s_port: port,
    })
    k8sForm.listen_port = ''
    ElMessage.success(`K8s 转发已建立:127.0.0.1:${listen} → ${ns}/${target}:${port}`)
    refresh()
  } catch (e) {
    ElMessage.error(e.message)
  }
}

async function refresh() {
  loading.value = true
  try {
    forwards.value = await listForwards(props.session.id)
  } catch (e) {
    ElMessage.error(e.message)
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

onMounted(() => {
  refresh()
  loadSSHConfig()
  probeK8s()
})
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
      ⚠ 该主机 sshd 已禁止 TCP 转发(AllowTcpForwarding={{ sshConfig?.allow_tcp_forwarding || 'no' }}{{ sshConfig?.disable_forwarding === 'yes' ? ', DisableForwarding=yes' : '' }}),创建的转发将无法使用。
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
      <template #header>
        K8s 服务转发(kubectl port-forward)
        <el-tag v-if="forwardDisabled" size="small" type="success" style="margin-left: 8px">不受 TCP 转发禁令影响</el-tag>
      </template>
      <div v-if="k8sAvailable === false" class="muted" style="font-size: 12px; margin-bottom: 8px">
        未在目标主机检测到 kubectl(含登录环境 PATH),此功能需要主机上已部署 kubectl 且可访问集群。
      </div>
      <div v-else-if="k8sListError" class="muted" style="font-size: 12px; margin-bottom: 8px; color: var(--el-color-warning)">
        资源列表获取异常:{{ k8sListError }}(可手动填写目标后直接添加)
      </div>
      <div class="fwd-form">
        <el-autocomplete
          v-model="k8sForm.namespace"
          :fetch-suggestions="nsSuggestions"
          placeholder="命名空间"
          style="width: 130px"
          clearable
          @blur="loadK8sTargets"
          @keyup.enter="loadK8sTargets"
        >
          <template #default="{ item }">
            <span>{{ item.value }}</span>
          </template>
        </el-autocomplete>
        <el-autocomplete
          v-model="k8sForm.target"
          :fetch-suggestions="targetSuggestions"
          :loading="k8sLoading"
          placeholder="目标,如 svc/my-app"
          style="width: 190px"
        >
          <template #default="{ item }">
            <span>{{ item.value }}</span>
            <span v-if="item.desc" class="muted" style="float: right; margin-left: 12px">{{ item.desc }}</span>
          </template>
        </el-autocomplete>
        <el-input v-model="k8sForm.port" placeholder="服务端口/名" style="width: 100px" />
        <span class="muted">→</span>
        <el-input v-model="k8sForm.listen_port" placeholder="监听端口" />
        <el-button type="primary" :disabled="k8sAvailable === false" @click="addK8s">添加转发</el-button>
      </div>
      <div v-for="f in forwards.filter((x) => x.type === 'k8s')" :key="f.id" class="fwd-list-item">
        <span>
          127.0.0.1:{{ f.listen_port }} → k8s {{ f.namespace }}/{{ f.k8s_target }}:{{ f.k8s_port }}
          <el-tooltip v-if="f.last_error" :content="f.last_error" placement="top">
            <el-tag size="small" type="warning" style="margin-left: 6px">最近桥接报错</el-tag>
          </el-tooltip>
        </span>
        <el-button size="small" type="danger" link @click="stop(f)">停止</el-button>
      </div>
      <div v-if="!forwards.some((x) => x.type === 'k8s')" class="muted" style="font-size: 12px">
        适用于 sshd 禁止 TCP 转发但部署了 Kubernetes 的主机:流量经 SSH exec 通道在远端由
        kubectl port-forward 桥接到 Service/Pod,无需远端安装 nc/socat,本机零依赖。
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
