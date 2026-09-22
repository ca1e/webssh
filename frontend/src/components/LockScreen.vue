<script setup>
// 启动密码锁屏:未设置密码时引导设置,已设置时输入解锁;
// 解锁成功写入全局 authState 并通知 App 加载业务数据。
import { computed, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Lock } from '@element-plus/icons-vue'
import { authSetup, authState, authUnlock } from '../api'
import { t } from '../i18n'

const emit = defineEmits(['unlocked'])

const formRef = ref()
const busy = ref(false)
const form = reactive({ password: '', confirm: '' })

const rules = computed(() => {
  const base = {
    password: [{ required: true, message: t('lock.rulePassword'), trigger: 'blur' }],
  }
  if (!authState.hasPassword) {
    base.confirm = [
      { required: true, message: t('lock.ruleConfirm'), trigger: 'blur' },
      {
        validator: (_, v, cb) => (v === form.password ? cb() : cb(new Error(t('lock.ruleMismatch')))),
        trigger: 'blur',
      },
    ]
  }
  return base
})

async function submit() {
  try {
    await formRef.value.validate()
  } catch {
    return
  }
  busy.value = true
  try {
    if (authState.hasPassword) {
      await authUnlock(form.password)
    } else {
      await authSetup(form.password)
      authState.hasPassword = true
    }
    authState.locked = false
    form.password = form.confirm = ''
    emit('unlocked')
  } catch (e) {
    ElMessage.error(e.message)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="lock-screen">
    <div class="lock-inner">
      <div class="lock-brand">
        <el-icon :size="52" class="lock-icon"><Lock /></el-icon>
        <div class="lock-title">WebSSH</div>
        <div class="lock-sub">
          {{ authState.hasPassword ? t('lock.unlockSub') : t('lock.setupSub') }}
        </div>
      </div>
      <el-form ref="formRef" :model="form" :rules="rules" @submit.prevent>
        <el-form-item prop="password" size="large">
          <el-input
            v-model="form.password"
            type="password"
            show-password
            autofocus
            :placeholder="authState.hasPassword ? t('lock.pwdPlaceholder') : t('lock.setupPlaceholder')"
            @keyup.enter="submit"
          />
        </el-form-item>
        <el-form-item v-if="!authState.hasPassword" prop="confirm" size="large">
          <el-input
            v-model="form.confirm"
            type="password"
            show-password
            :placeholder="t('lock.confirmPlaceholder')"
            @keyup.enter="submit"
          />
        </el-form-item>
        <el-button type="primary" size="large" class="lock-btn" :loading="busy" @click="submit">
          {{ authState.hasPassword ? t('lock.unlock') : t('lock.finish') }}
        </el-button>
      </el-form>
    </div>
  </div>
</template>
