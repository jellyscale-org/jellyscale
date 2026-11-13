#!/usr/bin/env bash
# Decodes JSON files as jellyfin-sdk-kotlin 1.9.4 does (JS-070), with its own serializers.
# Usage: decode.sh <Model> <file> [<Model> <file>]...    (Model[] for a list)
# Fetches the jars of jars.txt from Maven Central into $JELLYSCALE_SDKJVM_CACHE (default
# ~/.cache/jellyscale/sdkjvm), checks their sha256, and fails, never skips, when java
# (a JDK, to run Decode.java), curl or a jar is missing or wrong.
set -uo pipefail
here=${BASH_SOURCE[0]%/*}
command -v java >/dev/null || { echo "FAIL  test/sdkjvm/decode.sh needs java (a JDK 11 or later) on PATH" >&2; exit 1; }
command -v sha256sum >/dev/null && command -v curl >/dev/null || { echo "FAIL  test/sdkjvm/decode.sh needs curl and sha256sum on PATH" >&2; exit 1; }
cache=${JELLYSCALE_SDKJVM_CACHE:-$HOME/.cache/jellyscale/sdkjvm}
mkdir -p "$cache" || exit 1
classpath=""
while read -r coord sum; do
  [[ -z $coord || $coord == "#"* ]] && continue
  IFS=: read -r group artifact version <<<"$coord"
  jar=$cache/$artifact-$version.jar
  if [[ ! -f $jar ]]; then
    curl -fsS -o "$jar.part" "https://repo1.maven.org/maven2/${group//.//}/$artifact/$version/$artifact-$version.jar" || { rm -f "$jar.part"; echo "FAIL  could not fetch $coord from Maven Central" >&2; exit 1; }
    mv "$jar.part" "$jar"
  fi
  [[ $(sha256sum "$jar" | cut -d' ' -f1) == "$sum" ]] || { echo "FAIL  $jar is not the $coord pinned in test/sdkjvm/jars.txt (sha256 differs)" >&2; exit 1; }
  classpath+=$jar:
done <"$here/jars.txt"
exec java -cp "${classpath%:}" "$here/Decode.java" "$@"
