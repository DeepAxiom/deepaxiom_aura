#!/usr/bin/env bash
# Turns the frames capture_demo.mjs took into under-fire.gif.
#
#   bash docs/media/src/make_demo_gif.sh <outdir-of-capture_demo.mjs> <dest-dir>
#
# Same two passes as make_gifs.sh — a palette built from this recording, then
# mapped to it — with ffmpeg from a container, so nothing is installed. The
# frames were taken every 250 ms and are played at 12 a second, so the GIF runs
# three times faster than the run did; lower FPS to slow it down.
set -euo pipefail

abs() { (cd "$1" && (pwd -W 2>/dev/null || pwd)); }
mkdir -p "$2"
SRC="$(abs "$1/frames")"
DEST="$(abs "$2")"
FPS="${FPS:-12}"
COLORS="${COLORS:-64}"
WIDTH="${WIDTH:-1000}"
IMAGE="${FFMPEG_IMAGE:-linuxserver/ffmpeg}"
filters="scale=$WIDTH:-1:flags=lanczos"

MSYS_NO_PATHCONV=1 docker run --rm -v "$SRC:/in" -v "$DEST:/out" --entrypoint ffmpeg "$IMAGE" \
  -hide_banner -loglevel error -y -framerate "$FPS" -i /in/under-fire-%05d.png \
  -vf "$filters,palettegen=max_colors=$COLORS:stats_mode=diff" /in/palette.png </dev/null
MSYS_NO_PATHCONV=1 docker run --rm -v "$SRC:/in" -v "$DEST:/out" --entrypoint ffmpeg "$IMAGE" \
  -hide_banner -loglevel error -y -framerate "$FPS" -i /in/under-fire-%05d.png -i /in/palette.png \
  -lavfi "$filters[x];[x][1:v]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle" -loop 0 /out/under-fire.gif </dev/null
rm -f "$SRC/palette.png"
echo "under-fire.gif $(wc -c < "$DEST/under-fire.gif") bytes"
