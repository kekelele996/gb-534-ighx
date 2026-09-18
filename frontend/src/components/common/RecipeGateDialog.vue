<script setup lang="ts">
import { computed } from 'vue'
import { AlertTriangle, CheckCircle2, Database, FlaskConical } from 'lucide-vue-next'
import StateBadge from './StateBadge.vue'
import type { RecipeBlocker, RecipePublishGate } from '../../types/culture-recipe'

const props = defineProps<{ modelValue: boolean; gate: RecipePublishGate | null }>()
const emit = defineEmits<{ 'update:modelValue': [boolean]; retry: [] }>()

const open = computed({
  get: () => props.modelValue,
  set: (value) => emit('update:modelValue', value),
})

const seriesBlockers = computed(() => props.gate?.blockers.filter((b) => b.kind === 'ready_series') ?? [])
const analysisBlockers = computed(() => props.gate?.blockers.filter((b) => b.kind === 'open_analysis') ?? [])

function formatTime(value?: string): string {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString()
}

function blockerKey(blocker: RecipeBlocker): string {
  return `${blocker.kind}-${blocker.id}`
}

const kindTitle: Record<string, string> = {
  ready_series: '就绪时序仍在引用',
  open_analysis: '未确认分析仍在引用',
}
</script>

<template>
  <el-dialog v-model="open" title="发布被版本切换闸门拦截" width="min(680px, 94vw)">
    <div v-if="gate" class="gate-content">
      <el-alert type="warning" :closable="false" show-icon class="gate-banner">
        <template #title>
          <strong>{{ gate.recipe_code }} · v{{ gate.target_version }}</strong>
          的发布已被整体拒绝，新版本与旧版本状态均未改变。请先解除下列引用后重试。
        </template>
      </el-alert>

      <section v-if="gate.published_versions.length" class="gate-section">
        <h4><CheckCircle2 :size="15" /> 仍保持已发布的旧版本</h4>
        <ul class="gate-versions">
          <li v-for="version in gate.published_versions" :key="version.id">
            <span>{{ gate.recipe_code }} · v{{ version.version }}</span>
            <StateBadge :state="version.recipe_state" />
          </li>
        </ul>
      </section>

      <section v-if="seriesBlockers.length" class="gate-section">
        <h4><Database :size="15" /> {{ kindTitle.ready_series }}（{{ seriesBlockers.length }}）</h4>
        <ul class="gate-blockers">
          <li v-for="blocker in seriesBlockers" :key="blockerKey(blocker)">
            <div class="blocker-head">
              <strong>{{ blocker.series_run_code || blocker.label }}</strong>
              <StateBadge :state="blocker.state" />
            </div>
            <p class="blocker-reason">{{ blocker.reason }}</p>
            <p class="blocker-meta">
              <span>引用版本 v{{ blocker.recipe_version }}</span>
              <span>通道 {{ blocker.channel || '—' }}</span>
              <span>开始 {{ formatTime(blocker.started_at) }}</span>
            </p>
          </li>
        </ul>
      </section>

      <section v-if="analysisBlockers.length" class="gate-section">
        <h4><FlaskConical :size="15" /> {{ kindTitle.open_analysis }}（{{ analysisBlockers.length }}）</h4>
        <ul class="gate-blockers">
          <li v-for="blocker in analysisBlockers" :key="blockerKey(blocker)">
            <div class="blocker-head">
              <strong>{{ blocker.label }}</strong>
              <StateBadge :state="blocker.state" />
            </div>
            <p class="blocker-reason">{{ blocker.reason }}</p>
            <p class="blocker-meta">
              <span>引用版本 v{{ blocker.recipe_version }}</span>
              <span>发起人 {{ blocker.initiator || '—' }}</span>
              <span>分析时间 {{ formatTime(blocker.analyzed_at) }}</span>
            </p>
          </li>
        </ul>
      </section>

      <el-alert
        v-if="!gate.blockers.length" type="info" :closable="false" show-icon
        title="未返回具体阻塞项，请刷新列表后重试。"
      />
    </div>
    <template #footer>
      <el-button @click="open = false">关闭</el-button>
      <el-button type="primary" @click="emit('retry')">刷新后重试</el-button>
    </template>
  </el-dialog>
</template>
