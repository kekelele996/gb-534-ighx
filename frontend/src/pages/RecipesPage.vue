<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Copy, FileSearch, Plus, RefreshCw, Search, ShieldAlert } from 'lucide-vue-next'
import AnalysisExplanationDrawer from '../components/common/AnalysisExplanationDrawer.vue'
import AppShell from '../components/common/AppShell.vue'
import PageHeader from '../components/common/PageHeader.vue'
import PhaseBadge from '../components/common/PhaseBadge.vue'
import RecipeGateDialog from '../components/common/RecipeGateDialog.vue'
import StateBadge from '../components/common/StateBadge.vue'
import { useAuth } from '../hooks/useAuth'
import { useAnalysisStore } from '../stores/deviation-analysis'
import { useRecipeStore } from '../stores/culture-recipe'
import { useVesselStore } from '../stores/fermentation-vessel'
import { ApiError } from '../api/client'
import type { CreateRecipeInput, CultureRecipe, RecipeState } from '../types/culture-recipe'

const recipes = useRecipeStore()
const vessels = useVesselStore()
const analyses = useAnalysisStore()
const { canWriteRecipes } = useAuth()
const search = ref('')
const dialog = ref(false)
const drawer = ref(false)
const gateDialog = ref(false)
const saving = ref(false)
const form = reactive({ vessel_id: undefined as number | undefined, recipe_code: '', organism: '', target_duration_h: 24 })

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
  recipes.clearGate()
  try {
    const result = await recipes.transition(row, state)
    if (state === 'published' && result.obsoleted_versions.length) {
      ElMessage.success(`已发布 v${result.recipe.version}，旧版本已自动废止`)
    } else {
      ElMessage.success('配方状态已更新')
    }
  }
  catch (error) {
    if (error instanceof ApiError && error.code === 'RECIPE_PUBLISH_BLOCKED') {
      gateDialog.value = true
      return
    }
    ElMessage.error(error instanceof Error ? error.message : '状态更新失败')
  }
}
async function retryAfterGate() {
  gateDialog.value = false
  await recipes.load(search.value)
  ElMessage.info('列表已刷新，解除引用后可再次发布')
}
// gateSummary counts, for each validated version, the ready series and open
// analyses that would block a publish, so the table can warn before the user
// tries. It is derived from the analysis store and series loaded elsewhere;
// the authoritative list comes from the gate preview endpoint on demand.
async function openGatePreview(row: CultureRecipe) {
  try {
    await recipes.previewGate(row)
    gateDialog.value = true
  }
  catch (error) {
    ElMessage.error(error instanceof Error ? error.message : '读取阻塞清单失败')
  }
}
const gateBlocked = computed(() => recipes.gate && recipes.gate.blockers.length > 0)
async function refresh() {
  await recipes.load(search.value)
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
onMounted(() => Promise.all([recipes.load(), vessels.load(), analyses.load()]))
</script>

<template>
  <AppShell>
    <div class="page-wrap">
      <PageHeader eyebrow="VERSIONED PROCESS MODEL" title="配方版本" description="维护阶段边界、参考曲线与容差配置；发布新版本会自动废止同罐体同编号的旧已发布版本，引用未解除时整次发布回滚。">
        <el-tooltip content="刷新数据"><el-button circle aria-label="刷新" @click="refresh"><RefreshCw :size="17" /></el-button></el-tooltip>
        <el-button v-if="canWriteRecipes" type="primary" @click="dialog = true"><Plus :size="16" />新建配方</el-button>
      </PageHeader>
      <div class="toolbar">
        <el-input v-model="search" clearable placeholder="搜索配方编号或菌种" @keyup.enter="refresh"><template #prefix><Search :size="15" /></template></el-input>
        <span>{{ recipes.items.length }} 个版本</span>
      </div>
      <el-alert v-if="recipes.error" :title="recipes.error" type="error" :closable="false" show-icon />
      <el-alert
        v-if="gateBlocked" type="warning" show-icon :closable="false" class="gate-inline"
      >
        <template #title>
          <span>
            <ShieldAlert :size="14" /> {{ recipes.gate?.recipe_code }} · v{{ recipes.gate?.target_version }}
            的发布被拦截：{{ recipes.gate?.ready_series_count }} 条就绪时序、{{ recipes.gate?.open_analysis_count }} 个未确认分析仍在引用旧版本。
          </span>
        </template>
        <el-button size="small" type="warning" plain @click="gateDialog = true">查看阻塞清单</el-button>
      </el-alert>
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
        <el-table-column label="状态" width="100"><template #default="{ row }"><StateBadge :state="row.recipe_state" /></template></el-table-column>
        <el-table-column label="操作" width="250" align="right">
          <template #default="{ row }">
            <el-tooltip content="查看相关分析解释"><el-button text circle aria-label="查看分析解释" @click="explain(row)"><FileSearch :size="16" /></el-button></el-tooltip>
            <el-tooltip v-if="canWriteRecipes" content="复制为下一版本"><el-button text circle aria-label="复制版本" @click="copy(row)"><Copy :size="16" /></el-button></el-tooltip>
            <el-tooltip v-if="canWriteRecipes && row.recipe_state === 'validated'" content="预检发布闸门与阻塞清单">
              <el-button text circle aria-label="预检发布闸门" @click="openGatePreview(row)"><ShieldAlert :size="16" /></el-button>
            </el-tooltip>
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
    <AnalysisExplanationDrawer v-model="drawer" :analysis="analyses.selected" />
    <RecipeGateDialog v-model="gateDialog" :gate="recipes.gate" @retry="retryAfterGate" />
  </AppShell>
</template>
