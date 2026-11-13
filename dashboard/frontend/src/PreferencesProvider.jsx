import { useCallback, useEffect, useMemo, useState } from 'react'
import { call, postJSON } from './call.js'
import { DEFAULT_PREFERENCES, PreferencesContext, withChoice } from './preferences.js'
import { setDisplay } from './shared/units.js'

// merged is the Controller's answer over the defaults - its choices too,
// key by key.
function merged(p) {
  return { ...DEFAULT_PREFERENCES, ...p, choices: { ...DEFAULT_PREFERENCES.choices, ...p.choices } }
}

// PreferencesProvider loads the account's preferences once signed in, and
// saves a change at once.
export default function PreferencesProvider({ children }) {
  const [prefs, setPrefs] = useState(DEFAULT_PREFERENCES)
  const [loaded, setLoaded] = useState(false)
  // reload reads them again - after an admin changed the floors.
  const reload = useCallback(
    () =>
      call('/api/me/preferences', { background: true })
        .then((p) => p && setPrefs(merged(p)))
        .catch(() => {})
        .finally(() => setLoaded(true)),
    [],
  )
  useEffect(() => {
    reload()
  }, [reload])
  const put = useCallback(async (body) => {
    const p = await postJSON('/api/me/preferences', body, 'PUT')
    if (p) setPrefs(merged(p))
    return p
  }, [])
  // A change shows at once; the Controller's answer replaces it - or, if
  // it refuses, the preferences as they were come back.
  const save = useCallback(
    async (key, value) => {
      const before = prefs
      setPrefs({ ...prefs, [key]: value, chosen: withChoice(prefs.chosen, key, value) })
      try {
        return await put(withChoice(prefs.chosen, key, value))
      } catch (err) {
        setPrefs(before)
        throw err
      }
    },
    [put, prefs],
  )
  const reset = useCallback(() => put({}), [put])
  // The formatters follow the account's time zone and rate unit
  // (shared/units.js) before anything below draws a figure.
  setDisplay(prefs)
  const value = useMemo(() => ({ prefs, save, reset, reload, loaded }), [prefs, save, reset, reload, loaded])
  return <PreferencesContext.Provider value={value}>{children}</PreferencesContext.Provider>
}
