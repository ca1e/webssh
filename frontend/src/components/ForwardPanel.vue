<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { addForward, deleteForward, hostInfo, listForwards } from '../api'

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
