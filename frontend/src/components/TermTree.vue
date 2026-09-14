<script setup>
// 终端分割树:叶子 = 一个终端实例,split = 水平(h,上下)/垂直(v,左右)分割节点。
// SFC 通过文件名自引用实现递归渲染。
import TerminalPane from './TerminalPane.vue'

defineProps({
  tree: { type: Object, required: true },
  sid: { type: String, required: true },
  host: { type: String, default: '' },
  active: Boolean,
  count: { type: Number, required: true }, // 叶子总数,>1 时叶子可关闭
})

defineEmits(['close', 'focus'])
</script>

<template>
  <div v-if="tree.type === 'split'" class="term-split" :class="tree.dir">
    <TermTree
      :tree="tree.children[0]"
      :sid="sid"
      :host="host"
      :active="active"
      :count="count"
      @close="(k) => $emit('close', k)"
      @focus="(k) => $emit('focus', k)"
    />
    <TermTree
      :tree="tree.children[1]"
      :sid="sid"
      :host="host"
      :active="active"
      :count="count"
      @close="(k) => $emit('close', k)"
      @focus="(k) => $emit('focus', k)"
    />
  </div>
  <div v-else class="term-leaf">
    <TerminalPane
      :sid="sid"
      :host="host"
      :active="active"
      :closable="count > 1"
      @close="$emit('close', tree.key)"
      @focus="$emit('focus', tree.key)"
    />
  </div>
</template>
