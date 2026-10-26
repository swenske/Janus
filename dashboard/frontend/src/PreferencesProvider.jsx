import { useCallback, useEffect, useMemo, useState } from 'react'
import { call, postJSON } from './call.js'
import { DEFAULT_PREFERENCES, PreferencesContext, withChoice } from './preferences.js'

// PreferencesProvider loads the account's preferences once signed in, and
// saves a change at once.
export default function PreferencesProvider({ children }) {
  const [prefs, setPrefs] = useState(DEFAULT_PREFERENCES)
  const [loaded, setLoaded] = useState(false)
  useEffect(() => {
    call('/api/me/preferences', { background: true })
      .then((p) => p && setPrefs({ ...DEFAULT_PREFERENCES, ...p }))
      .catch(() => {})
      .finally(() => setLoaded(true))
  }, [])
  const put = useCallback(async (body) => {
    const p = await postJSON('/api/me/preferences', body, 'PUT')
    if (p) setPrefs({ ...DEFAULT_PREFERENCES, ...p })
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
  const value = useMemo(() => ({ prefs, save, reset, loaded }), [prefs, save, reset, loaded])
  return <PreferencesContext.Provider value={value}>{children}</PreferencesContext.Provider>
}
