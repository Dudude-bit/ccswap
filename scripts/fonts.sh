#!/bin/sh
# Maintainer script: rebuilds internal/web/fonts from google/fonts (pinned commit).
# Needs fonttools and brotli (pip install --user fonttools==4.61.1 brotli==1.2.0, the versions the committed
# woff2 files were built with; other versions may produce different bytes). Nothing runs it in tests.
# The commit SHA is content-addressed, so the curl downloads need no extra checksum.
# OFL-Geologica.txt names "Geologisk Project Authors" upstream; it is copied verbatim, do not edit it.
set -eu
SHA=b7c8331cf620346575487ef8db0e2c729f014675
RAW=https://raw.githubusercontent.com/google/fonts/$SHA/ofl
OUT=$(cd "$(dirname "$0")/.." && pwd)/internal/web/fonts
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
UNI='U+0000-00FF,U+0100-017F,U+2000-206F,U+20AC,U+2122,U+2190-2193,U+2212,U+2248,U+0400-04FF'
FEAT='kern,tnum,liga,calt,ccmp,locl,mark,mkmk'

curl -fsSL -o "$TMP/geo.ttf" "$RAW/geologica/Geologica%5BCRSV,SHRP,slnt,wght%5D.ttf"
curl -fsSL -o "$OUT/OFL-Geologica.txt" "$RAW/geologica/OFL.txt"
curl -fsSL -o "$TMP/jbm.ttf" "$RAW/jetbrainsmono/JetBrainsMono%5Bwght%5D.ttf"
curl -fsSL -o "$OUT/OFL-JetBrainsMono.txt" "$RAW/jetbrainsmono/OFL.txt"

# Geologica: keep wght 300-700, pin the other axes to their defaults.
python3 -I -m fontTools varLib.instancer "$TMP/geo.ttf" wght=300:400:700 CRSV=drop SHRP=drop slnt=drop -o "$TMP/geo-i.ttf"
python3 -I -m fontTools.subset "$TMP/geo-i.ttf" --unicodes="$UNI" --layout-features="$FEAT" --flavor=woff2 --output-file="$OUT/geologica.woff2"

# JetBrains Mono: static weight 500.
python3 -I -m fontTools varLib.instancer "$TMP/jbm.ttf" wght=500 -o "$TMP/jbm-i.ttf"
python3 -I -m fontTools.subset "$TMP/jbm-i.ttf" --unicodes="$UNI" --layout-features="$FEAT" --flavor=woff2 --output-file="$OUT/jetbrains-mono.woff2"

# The code points each font draws, as ranges, for web_test.go: every page text must be in them.
for f in geologica jetbrains-mono; do
  python3 -I -c '
import sys
from fontTools.ttLib import TTFont
cps, out = sorted(TTFont(sys.argv[1]).getBestCmap()), []
for c in cps:
    if out and out[-1][1] == c - 1:
        out[-1][1] = c
    else:
        out.append([c, c])
for lo, hi in out:
    print("%04X" % lo if lo == hi else "%04X-%04X" % (lo, hi))
' "$OUT/$f.woff2" >"$OUT/$f.cmap"
done
