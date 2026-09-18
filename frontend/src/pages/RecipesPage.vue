<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Copy, FileSearch, Plus, RefreshCw, Search, ShieldAlert, ShieldCheck } from 'lucide-vue-next'
import AnalysisExplanationDrawer from '../components/common/AnalysisExplanationDrawer.vue'
import AppShell from '../components/common/AppShell.vue'
import PageHeader from '../components/common/PageHeader.vue'
import PhaseBadge from '../components/common/PhaseBadge.vue'
import StateBadge from '../components/common/StateBadge.vue'
import { useAuth } from '../hooks/useAuth'
import { useAnalysisStore } from '../stores/deviation-analysis'
import { useRecipeStore } from '../stores/culture-recipe'
import { useVesselStore } from '../stores/fermentation-vessel'
import { errorMessage } from '../api/client'
import type { CreateRecipeInput, CultureRecipe, PublishBlocker, RecipeState } from '../types/culture-recipe'

const recipes = useRecipeStore()
const vessels = useVesselStore()
const analyses = useAnalysisStore()
const { canWriteRecipes } = useAuth()
const search = ref('')
const dialog = ref(false)
const drawer = ref(false)
const blockDialog = ref(false)
const saving = ref(false)
const form = reactive({ vessel_id: undefined as number | undefined, recipe_code: '', organism: '', target_duration_h: 24 })

const stateLabels: Record<string, string> = {
  queued: '排队中', analyzing: '分析中', completed: '已完成', failed: '失败',
  reviewed: '已复核', confirmed: '已确认', investigating: '调查中', voided: '已作废',
  ready: '就绪',
}

function recipeInput(): CreateRecipeInput {
  const hours = Array.from({ length: 13 }, (_, index) => index * 2)
  return {
    vessel_id: form.vessel_id!, recipe_code: form.recipe_code, organism: form.organism,
    target_duration_h: form.target_duration_h,
    phase_boundaries_json: [
      { phase: 'lag', start_hour: 0, end_hour: 4 },
      { phase: 'growth', start_hour: 4, end_hour: 10 },
      { phase: 'production', start_hour: 10, end_hour: 20 },
      { phase: 'harvest', start_hour: 20, end_hour: 24 },
    ],
    reference_curves_json: {
      ph: hours.map((hour) => ({ elapsed_h: hour, value: 6.8 - hour * .025 })),
      temperature: hours.map((hour) => ({ elapsed_h: hour, value: 29.5 + hour * .04 })),
      do: hours.map((hour) => ({ elapsed_h: hour, value: 68 - hour * 1.25 })),
      agitation: hours.map((hour) => ({ elapsed_h: hour, value: 320 + hour * 9 })),
    },
    tolerance_profile_json: {
      ph: { weight: 1.3, max_distance: .8 }, temperature: { weight: 1, max_distance: .8 },
      do: { weight: 1.4, max_distance: 1 }, agitation: { weight: .7, max_distance: 1.1 },
    },
  }
}
async function submit() {
  saving.value = true
  try { await recipes.create(recipeInput()); dialog.value = false; ElMessage.success('配方草稿已创建') }
  catch (error) { ElMessage.error(error instanceof Error ? error.message : '创建失败') }
  finally { saving.value = false }
}
function nextState(state: RecipeState): RecipeState | null {
  return state === 'draft' ? 'validated' : state === 'validated' ? 'published' : state === 'published' ? 'obsolete' : null
}
async function advance(row: CultureRecipe) {
  const state = nextState(row.recipe_state)
  if (!state) return
  try {
    await recipes.transition(row, state)
    ElMessage.success(state === 'published' ? '新版本已发布，同组旧版本已自动废止' : '配方状态已更新')
  } catch (error) {
    if (recipes.lastBlockers) blockDialog.value = true
    else ElMessage.error(errorMessage(error))
  }
}
async function copy(row: CultureRecipe) {
  try { await recipes.copy(row); ElMessage.success('已创建下一版本草稿') }
  catch (error) { ElMessage.error(error instanceof Error ? error.message : '复制失败') }
}
function explain(row: CultureRecipe) {
  const analysis = analyses.items.find((item) => item.recipe_id === row.id)
  if (!analysis) { ElMessage.info('当前版本尚无分析解释'); return }
  analyses.selected = analysis; drawer.value = true
}
function gateBlockers(row: CultureRecipe): PublishBlocker[] {
  return row.publish_gate?.blockers ?? []
}
function blockerText(blocker: PublishBlocker): string {
  const version = `v${blocker.recipe_version}`
  if (blocker.kind === 'ready_series') {
    return `就绪时序 ${blocker.run_code || '#' + blocker.series_id} 仍引用 ${version}`
  }
  const state = blocker.state ? stateLabels[blocker.state] ?? blocker.state : '未确认'
  return `分析 #${blocker.analysis_id} 处于「${state}」未确认${blocker.initiated_by_name ? `（发起人 ${blocker.initiated_by_name}）` : ''}，运行 ${blocker.run_code || '-'}`
}
const blockerDetail = computed(() => recipes.lastBlockers)
onMounted(() => Promise.all([recipes.load(), vessels.load(), analyses.load()]))
</script>

<template>
  <AppShell>
    <div class="page-wrap">
      <PageHeader eyebrow="VERSIONED PROCESS MODEL" title="配方版本" description="维护阶段边界、参考曲线与容差配置；发布新版本时旧已发布版本自动废止，存在未了结引用时发布将被闸门拒绝。">
        <el-tooltip content="刷新数据"><el-button circle aria-label="刷新" @click="recipes.load(search)"><RefreshCw :size="17" /></el-button></el-tooltip>
        <el-button v-if="canWriteRecipes" type="primary" @click="dialog = true"><Plus :size="16" />新建配方</el-button>
      </PageHeader>
      <div class="toolbar">
        <el-input v-model="search" clearable placeholder="搜索配方编号或菌种" @keyup.enter="recipes.load(search)"><template #prefix><Search :size="15" /></template></el-input>
        <span>{{ recipes.items.length }} 个版本</span>
      </div>
      <el-alert v-if="recipes.error" :title="recipes.error" type="error" :closable="false" show-icon />
      <el-skeleton v-if="recipes.loading" :rows="7" animated />
      <div v-else-if="!recipes.items.length" class="empty-state"><h2>暂无配方版本</h2><p>创建草稿并完成校验后即可发布。</p></div>
      <el-table v-else :data="recipes.items" row-key="id">
        <el-table-column label="配方版本" min-width="210">
          <template #default="{ row }"><div class="primary-cell"><strong>{{ row.recipe_code }} · v{{ row.version }}</strong><span>{{ row.organism }}</span></div></template>
        </el-table-column>
        <el-table-column label="关联罐体" min-width="180"><template #default="{ row }"><div class="primary-cell"><strong>{{ row.vessel?.vessel_code }}</strong><span>{{ row.vessel?.name }}</span></div></template></el-table-column>
        <el-table-column label="阶段边界" min-width="310">
          <template #default="{ row }"><div class="phase-list"><PhaseBadge v-for="phase in row.phase_boundaries_json" :key="phase.phase" :phase="phase.phase" /><small>{{ row.target_duration_h }} h</small></div></template>
        </el-table-column>
        <el-table-column label="通道 / 容差" width="130"><template #default="{ row }"><span class="numeric">{{ Object.keys(row.reference_curves_json).length }}</span><small class="cell-note">参考通道</small></template></el-table-column>
        <el-table-column label="版本状态" min-width="200">
          <template #default="{ row }">
            <div class="gate-cell">
              <StateBadge :state="row.recipe_state" />
              <el-popover v-if="gateBlockers(row).length" placement="top" :width="340" trigger="hover">
                <template #reference>
                  <el-tag
                    :type="row.recipe_state === 'validated' ? 'danger' : 'warning'"
                    size="small" effect="plain" class="gate-tag"
                  >
                    <ShieldAlert v-if="row.recipe_state === 'validated'" :size="13" />
                    <ShieldCheck v-else :size="13" />
                    {{ gateBlockers(row).length }} 项{{ row.recipe_state === 'validated' ? '阻塞' : '在保引用' }}
                  </el-tag>
                </template>
                <div class="gate-popover">
                  <strong>{{ row.recipe_state === 'validated' ? '发布将被拒绝：' : '该版本仍被引用保护：' }}</strong>
                  <ul>
                    <li v-for="blocker in gateBlockers(row)" :key="`${blocker.kind}-${blocker.series_id ?? blocker.analysis_id}`">
                      {{ blockerText(blocker) }}
                    </li>
                  </ul>
                </div>
              </el-popover>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="210" align="right">
          <template #default="{ row }">
            <el-tooltip content="查看相关分析解释"><el-button text circle aria-label="查看分析解释" @click="explain(row)"><FileSearch :size="16" /></el-button></el-tooltip>
            <el-tooltip v-if="canWriteRecipes" content="复制为下一版本"><el-button text circle aria-label="复制版本" @click="copy(row)"><Copy :size="16" /></el-button></el-tooltip>
            <el-button v-if="canWriteRecipes && nextState(row.recipe_state)" size="small" @click="advance(row)">
              {{ row.recipe_state === 'draft' ? '校验' : row.recipe_state === 'validated' ? '发布' : '废止' }}
            </el-button>
          </template>
        </el-table-column>
      </el-table>
    </div>
    <el-dialog v-model="dialog" title="新建配方草稿" width="min(620px, 94vw)">
      <el-form label-position="top">
        <el-form-item label="目标发酵罐"><el-select v-model="form.vessel_id" placeholder="选择发酵罐"><el-option v-for="vessel in vessels.items.filter((item) => item.vessel_state === 'active')" :key="vessel.id" :label="`${vessel.vessel_code} · ${vessel.name}`" :value="vessel.id" /></el-select></el-form-item>
        <div class="form-grid two">
          <el-form-item label="配方编号"><el-input v-model="form.recipe_code" placeholder="YEAST-FEDBATCH-C" /></el-form-item>
          <el-form-item label="目标时长 (h)"><el-input-number v-model="form.target_duration_h" :min="1" :max="10000" disabled /></el-form-item>
        </div>
        <el-form-item label="菌种"><el-input v-model="form.organism" /></el-form-item>
        <div class="configuration-note">新版本将以 0–4 / 4–10 / 10–20 / 20–24 h 的四阶段参考模板建立，可在发布前经 API 编辑。</div>
      </el-form>
      <template #footer><el-button @click="dialog = false">取消</el-button><el-button type="primary" :loading="saving" :disabled="!form.vessel_id" @click="submit">创建草稿</el-button></template>
    </el-dialog>
    <el-dialog v-model="blockDialog" title="发布被版本闸门拒绝" width="min(620px, 94vw)">
      <div v-if="blockerDetail" class="block-dialog">
        <el-alert
          type="error" :closable="false" show-icon
          :title="`配方 ${blockerDetail.recipe_code} 的新版本未能发布，旧版本状态保持不变；请先了结以下 ${blockerDetail.blockers.length} 项引用后重试。`"
        />
        <el-table :data="blockerDetail.blockers" size="small" class="block-table">
          <el-table-column label="类型" width="110">
            <template #default="{ row }">
              <el-tag :type="row.kind === 'ready_series' ? 'danger' : 'warning'" size="small">
                {{ row.kind === 'ready_series' ? '就绪时序' : '未确认分析' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="版本" width="70"><template #default="{ row }">v{{ row.recipe_version }}</template></el-table-column>
          <el-table-column label="阻塞原因" min-width="320"><template #default="{ row }">{{ blockerText(row) }}</template></el-table-column>
        </el-table>
        <p class="block-hint">刷新后列表回读一致：新版本仍为「已校验」，旧版本仍为「已发布」。</p>
      </div>
      <template #footer><el-button type="primary" @click="blockDialog = false">我知道了</el-button></template>
    </el-dialog>
    <AnalysisExplanationDrawer v-model="drawer" :analysis="analyses.selected" />
  </AppShell>
</template>
