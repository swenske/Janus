// A theme chosen in the docs' header becomes the landing page's too
// (ThemeProvider.astro copies the other way).
document.addEventListener('change', (e) => {
  const select = e.target
  if (!(select instanceof HTMLSelectElement) || !select.closest('starlight-theme-select')) return
  try {
    localStorage.setItem('janus-theme', select.value === 'light' || select.value === 'dark' ? select.value : 'system')
  } catch {
    // a private window: the choice just isn't remembered
  }
})
