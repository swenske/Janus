# JetBrains Mono, for the Controller's figures

The Controller's monospace face (`--mono` in `../../controller.css`, the
family "Janus Mono"): JetBrains Mono 2.304, SIL Open Font License 1.1
(`OFL.txt`, also shipped next to the built pages as
`OFL-JetBrainsMono.txt`). Renamed in the CSS only - the files keep the
fonts' own names and copyright strings.

Made from the release's TrueType files with fontTools 4.57.0 (Debian
13's `python3-fonttools` and `python3-brotli`): a Latin subset and no
layout features - no ligatures, so `->` or `!=` in a configuration stay
as typed.

```sh
curl -fsSLO https://github.com/JetBrains/JetBrainsMono/releases/download/v2.304/JetBrainsMono-2.304.zip
sha256sum JetBrainsMono-2.304.zip
# 6f6376c6ed2960ea8a963cd7387ec9d76e3f629125bc33d1fdcd7eb7012f7bbf
unzip JetBrainsMono-2.304.zip
U="U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+2000-206F,U+2074,U+20AC,U+2122,U+2190-2193,U+2212,U+2215,U+2248,U+2260,U+2264-2265,U+FEFF,U+FFFD"
for w in Regular:400 SemiBold:600; do
  python3 -m fontTools.subset "fonts/ttf/JetBrainsMono-${w%%:*}.ttf" --unicodes="$U" \
    --layout-features="" --name-IDs="*" --flavor=woff2 --output-file="janus-mono-${w#*:}.woff2"
done
sha256sum janus-mono-*.woff2
# a97ed90ebd8ea771b23e65587a2f1069ad2bd3e54dd56a817f169f0ae7c4124a  janus-mono-400.woff2
# 02b0b55006b1e5ae342c62ed5936ceb983596b5b0933143f882f970948024b0a  janus-mono-600.woff2
```

The release publishes no checksum of its own: the zip's above is
the one measured when these were made, 2026-10-18.
