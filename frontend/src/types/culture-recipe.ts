import type { FermentationPhase } from './enums/fermentation-phase'
import type { FermentationVessel } from './fermentation-vessel'

export type RecipeState = 'draft' | 'validated' | 'published' | 'obsolete'
export interface PhaseBoundary { phase: FermentationPhase; start_hour: number; end_hour: number }
export interface CurvePoint { elapsed_h: number; value: number }
export interface ChannelTolerance { weight: number; max_distance: number }

export interface CultureRecipe {
  id: number
  vessel_id: number
  vessel?: FermentationVessel
  recipe_code: string
  version: number
  organism: string
  target_duration_h: number
  phase_boundaries_json: PhaseBoundary[]
  reference_curves_json: Record<string, CurvePoint[]>
  tolerance_profile_json: Record<string, ChannelTolerance>
  recipe_state: RecipeState
  created_by: number
  created_by_name: string
  created_at: string
  updated_at: string
}

export interface CreateRecipeInput {
  vessel_id: number
  recipe_code: string
  organism: string
  target_duration_h: number
  phase_boundaries_json: PhaseBoundary[]
  reference_curves_json: Record<string, CurvePoint[]>
  tolerance_profile_json: Record<string, ChannelTolerance>
}

export type RecipeBlockerKind = 'ready_series' | 'open_analysis'

export interface RecipeBlocker {
  kind: RecipeBlockerKind | string
  id: number
  state: string
  label: string
  vessel_id: number
  recipe_id: number
  recipe_code: string
  recipe_version: number
  series_run_code?: string
  channel?: string
  started_at?: string
  analyzed_at?: string
  initiator?: string
  reason: string
}

export interface RecipePublishGate {
  recipe_code: string
  vessel_id: number
  target_version: number
  published_versions: CultureRecipe[]
  blockers: RecipeBlocker[]
  ready_series_count: number
  open_analysis_count: number
}

export interface RecipeTransitionResult {
  recipe: CultureRecipe
  obsoleted_versions: CultureRecipe[]
}
