#!/usr/bin/env bash
# Captures PerfectMind fixtures into internal/perfectmind/testdata.
set -euo pipefail

BASE="https://cityofmarkham.perfectmind.com"
W="6825ea71-e5b7-4c2a-948f-9195507ad90a"
CAL="d4d891dd-9e45-474b-97c4-e43c8f8fe3b8" # Aquafit
UA="Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
OUT="$(cd "$(dirname "$0")/.." && pwd)/internal/perfectmind/testdata"
JAR="$(mktemp)"
trap 'rm -f "$JAR"' EXIT
mkdir -p "$OUT"

plus_days() { # portable: BSD date, GNU date fallback
  date -u -v+"$1"d +%Y-%m-%d 2>/dev/null || date -u -d "+$1 days" +%Y-%m-%d
}
TODAY="$(date -u +%Y-%m-%d)"

token() { # $1 = page url
  curl -sS -A "$UA" -c "$JAR" -b "$JAR" "$1" \
    | grep -oE 'name="__RequestVerificationToken"[^>]*value="[^"]+"' | head -1 \
    | sed -E 's/.*value="([^"]+)"/\1/'
}
urlenc() { python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1],safe=""))' "$1"; }

filters() { # $1 = from, $2 = to
  printf 'values[0][Name]=Date+Range&values[0][Value]=%sT00:00:00.000Z&values[0][Value2]=%sT00:00:00.000Z&values[0][ValueKind]=6' "$1" "$2"
  printf '&values[1][Name]=Time+Range&values[1][Value]=&values[1][Value2]=&values[1][ValueKind]=7'
  printf '&values[2][Name]=Keyword&values[2][Value]=&values[2][Value2]=&values[2][ValueKind]=9'
  printf '&values[3][Name]=Age&values[3][Value]=0&values[3][Value2]=1188&values[3][ValueKind]=0'
}

post_json() { # $1 url, $2 referer, $3 body, $4 outfile ; retries once on non-JSON
  local try
  for try in 1 2; do
    curl -sS -A "$UA" -c "$JAR" -b "$JAR" -X POST "$1" \
      -H 'X-Requested-With: XMLHttpRequest' -H "Referer: $2" \
      -H 'Content-Type: application/x-www-form-urlencoded; charset=UTF-8' \
      --data "$3" -o "$4.tmp"
    if jq -e . "$4.tmp" >/dev/null 2>&1; then mv "$4.tmp" "$4"; return 0; fi
    echo "non-JSON response (try $try), retrying" >&2; sleep 5
  done
  rm -f "$4.tmp"; echo "failed: $1" >&2; return 1
}

# 1. categories
PAGE="$BASE/Clients/BookMe4?widgetId=$W"
TOK="$(token "$PAGE")"; sleep 1
post_json "$BASE/Clients/BookMe4V2/GetCategoriesDataV2?embed=False" "$PAGE" \
  "widgetId=$W&$(filters "$TODAY" "$(plus_days 365)")&__RequestVerificationToken=$(urlenc "$TOK")" \
  "$OUT/categories.json"
sleep 1

# 2. classes (Aquafit)
PAGE="$BASE/Clients/BookMe4BookingPages/Classes?calendarId=$CAL&widgetId=$W&embed=False"
classes() { # $1 from, $2 to, $3 outfile
  local tok; tok="$(token "$PAGE")"; sleep 1
  post_json "$BASE/Clients/BookMe4BookingPagesV2/ClassesV2" "$PAGE" \
    "calendarId=$CAL&widgetId=$W&page=0&$(filters "$1" "$2")&__RequestVerificationToken=$(urlenc "$tok")" "$3"
}
classes "$TODAY" "$(plus_days 90)" "$OUT/classes_page1.json"; sleep 1
classes "$(plus_days 400)" "$(plus_days 500)" "$OUT/classes_empty.json"
echo "done"
