// The appearance this browser chose (src/shared/appearance.js), set on
// <html> before the page paints: a module script runs after it, and a
// dark palette would flash the default light one. Same lists as there
// (palettes.test.js runs this file).
;(function () {
  var palettes = ['janus', 'abysse', 'ayu', 'catppuccin', 'dracula', 'everforest', 'flexoki', 'github', 'gruvbox', 'kanagawa', 'night-owl', 'nightfox', 'nord', 'one', 'rose-pine', 'solarized', 'tokyo-night', 'tomorrow']
  var mode = null
  var palette = null
  try {
    mode = localStorage.getItem('janus-theme')
    palette = localStorage.getItem('janus-palette')
  } catch (e) {
    // a private window
  }
  if (mode !== 'light' && mode !== 'dark') {
    mode = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
  }
  if (palettes.indexOf(palette) < 0) palette = 'janus'
  document.documentElement.setAttribute('data-theme', mode)
  document.documentElement.setAttribute('data-palette', palette)
})()
