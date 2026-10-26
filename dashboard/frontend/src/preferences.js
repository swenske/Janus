import { createContext, useContext } from 'react'

// The signed-in account's preferences (GET/PUT /api/me/preferences,
// dashboard/backend/preferences.go - kept with the account on the
// Controller): what the Nodes and Hypervisors pages follow. Until the
// Controller answers - or one too old to have them -, the defaults: the
// pages as they were.
export const DEFAULT_PREFERENCES = {
  nodes_refresh_seconds: 15,
  hypervisors_refresh_seconds: 15,
  node_stats: true,
  node_charts: true,
  time_zone: 'local',
  rate_unit: 'bytes',
  chosen: {},
  floors: { nodes: 0, hypervisors: 0 },
  choices: { nodes: [5, 10, 15, 30, 60], hypervisors: [10, 15, 30, 60, 120] },
}

// PreferencesContext: { prefs, save(key, value), reset(), reload(), loaded }.
export const PreferencesContext = createContext({ prefs: DEFAULT_PREFERENCES, save: async () => {}, reset: async () => {}, reload: async () => {}, loaded: false })

export function usePreferences() {
  return useContext(PreferencesContext)
}

// withChoice is what to send to change one preference: the account's
// own choices (not the defaults it didn't choose), with this one.
export function withChoice(chosen, key, value) {
  return { ...(chosen || {}), [key]: value }
}
