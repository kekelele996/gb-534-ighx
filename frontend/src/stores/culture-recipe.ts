import { ref } from 'vue'
import { defineStore } from 'pinia'
import { copyRecipe, createRecipe, fetchPublishGate, listRecipes, transitionRecipe } from '../api/culture-recipe'
import { ApiError, errorMessage } from '../api/client'
import type {
  CreateRecipeInput,
  CultureRecipe,
  RecipePublishGate,
  RecipeState,
  RecipeTransitionResult,
} from '../types/culture-recipe'

export const useRecipeStore = defineStore('culture-recipes', () => {
  const items = ref<CultureRecipe[]>([])
  const loading = ref(false)
  const error = ref('')
  // Set when a publish is rejected by the version switch gate. The page reads
  // it to render the blocking list (ready series / unconfirmed analyses).
  const gate = ref<RecipePublishGate | null>(null)
  // Remembers the validated version whose publish was blocked so the blocking
  // list can be re-read after a refresh.
  const gateRecipeId = ref<number | null>(null)

  async function load(search = '') {
    loading.value = true; error.value = ''
    try {
      items.value = (await listRecipes({ search, page_size: 100 })).items
      await detectBlockedGates()
    }
    catch (cause) { error.value = errorMessage(cause) }
    finally { loading.value = false }
  }
  // detectBlockedGates re-reads the switch gate for each validated version that
  // has a published sibling in the same vessel + recipe code. It keeps the page
  // showing the blocking list even after a full refresh, without attempting a
  // publish. It surfaces the first blocked group found.
  async function detectBlockedGates() {
    const candidates = items.value.filter((item) => {
      if (item.recipe_state !== 'validated') return false
      return items.value.some((other) =>
        other.id !== item.id && other.vessel_id === item.vessel_id &&
        other.recipe_code === item.recipe_code && other.recipe_state === 'published')
    })
    for (const item of candidates) {
      try {
        const preview = await fetchPublishGate(item.id)
        if (preview.blockers.length > 0) {
          gate.value = preview
          gateRecipeId.value = item.id
          return
        }
      }
      catch {
        // Gate preview is advisory on page load; ignore transient failures.
      }
    }
  }
  async function create(input: CreateRecipeInput) {
    gate.value = null; gateRecipeId.value = null
    await createRecipe(input); await load()
  }
  async function transition(item: CultureRecipe, state: RecipeState, comment = ''): Promise<RecipeTransitionResult> {
    gate.value = null; gateRecipeId.value = null
    try {
      const result = await transitionRecipe(item.id, state, item.version, comment)
      await load()
      return result
    }
    catch (cause) {
      if (cause instanceof ApiError && cause.code === 'RECIPE_PUBLISH_BLOCKED') {
        gate.value = cause.details as RecipePublishGate
        gateRecipeId.value = item.id
      }
      await load()
      throw cause
    }
  }
  // previewGate re-reads the blocking list for a recipe without attempting a
  // publish, so the page stays consistent after refresh.
  async function previewGate(item: CultureRecipe) {
    gateRecipeId.value = item.id
    gate.value = await fetchPublishGate(item.id)
  }
  function clearGate() { gate.value = null; gateRecipeId.value = null }
  async function copy(item: CultureRecipe) {
    gate.value = null; gateRecipeId.value = null
    await copyRecipe(item.id); await load()
  }
  return {
    items, loading, error, gate, gateRecipeId,
    load, create, transition, previewGate, copy, clearGate,
  }
})
