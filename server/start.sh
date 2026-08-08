#!/usr/bin/env bash
# Arranque del servidor Lucky Box (Paper + Geyser + Floodgate + ntdLuckyBlock)
set -euo pipefail
cd "$(dirname "$0")"

# RAM: ajusta a tu VPS. Regla practica: deja ~1-2G libres para el SO.
MEM_MIN="${MEM_MIN:-2G}"
MEM_MAX="${MEM_MAX:-4G}"

# Flags de Aikar: reducen los tirones del recolector de basura en servidores MC.
exec java \
  -Xms"${MEM_MIN}" -Xmx"${MEM_MAX}" \
  -XX:+UseG1GC \
  -XX:+ParallelRefProcEnabled \
  -XX:MaxGCPauseMillis=200 \
  -XX:+UnlockExperimentalVMOptions \
  -XX:+DisableExplicitGC \
  -XX:+AlwaysPreTouch \
  -XX:G1NewSizePercent=30 \
  -XX:G1MaxNewSizePercent=40 \
  -XX:G1HeapRegionSize=8M \
  -XX:G1ReservePercent=20 \
  -XX:G1HeapWastePercent=5 \
  -XX:G1MixedGCCountTarget=4 \
  -XX:InitiatingHeapOccupancyPercent=15 \
  -XX:G1MixedGCLiveThresholdPercent=90 \
  -XX:G1RSetUpdatingPauseTimePercent=5 \
  -XX:SurvivorRatio=32 \
  -XX:+PerfDisableSharedMem \
  -XX:MaxTenuringThreshold=1 \
  -Dusing.aikars.flags=https://mcflags.emc.gs \
  -Daikars.new.flags=true \
  -jar paper.jar nogui
