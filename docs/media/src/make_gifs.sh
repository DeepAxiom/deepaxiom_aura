#!/usr/bin/env bash
# Turns the scene videos capture.mjs recorded into palette-optimised GIFs.
#
#   bash docs/media/src/make_gifs.sh <outdir-of-capture.mjs> <dest-dir>
#
# Two passes per scene: palettegen builds a palette from that scene only, then
# paletteuse maps to it. ffmpeg runs from a container, so nothing is installed.
# Width 1000 px, 12 fps; lower FPS or COLORS if a scene comes out over 4 MB.
set -euo pipefail

# Absolute path in the form Docker wants (C:/... under Git Bash, /... elsewhere).
abs() { (cd "$1" && (pwd -W 2>/dev/null || pwd)); }
mkdir -p "$2"
SRC="$(abs "$1/video")"
DEST="$(abs "$2")"
FPS="${FPS:-12}"
COLORS="${COLORS:-96}"
WIDTH="${WIDTH:-1000}"
IMAGE="${FFMPEG_IMAGE:-linuxserver/ffmpeg}"
LIST='import json,sys; [print(s["name"], round(s["start"],2), round(s["length"],2)) for s in json.load(open(sys.argv[1]))]'

python -c "$LIST" "$SRC/scenes.json" | tr -d '\r' | while read -r name start length; do
  filters="fps=$FPS,scale=$WIDTH:-1:flags=lanczos"
  MSYS_NO_PATHCONV=1 docker run --rm -v "$SRC:/in" -v "$DEST:/out" --entrypoint ffmpeg "$IMAGE" \
    -hide_banner -loglevel error -y -ss "$start" -t "$length" -i "/in/$name.webm" \
    -vf "$filters,palettegen=max_colors=$COLORS:stats_mode=diff" "/in/$name.palette.png" </dev/null
  MSYS_NO_PATHCONV=1 docker run --rm -v "$SRC:/in" -v "$DEST:/out" --entrypoint ffmpeg "$IMAGE" \
    -hide_banner -loglevel error -y -ss "$start" -t "$length" -i "/in/$name.webm" -i "/in/$name.palette.png" \
    -lavfi "$filters[x];[x][1:v]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle" -loop 0 "/out/$name.gif" </dev/null
  echo "$name.gif $(wc -c < "$DEST/$name.gif") bytes"
done
