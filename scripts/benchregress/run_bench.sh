#!/usr/bin/env bash
# run_bench.sh — اجرای بنچ‌مارک‌های Go برای رگرسیون CI
#
# استفاده:
#   ./run_bench.sh <output-file> [benchtime] [count]
#
# نمونه:
#   ./run_bench.sh current.txt 1s 1
#   ./run_bench.sh baseline.txt 2s 3
#
# پکیج‌های هدف:
#   internal/arq, internal/basecodec, internal/client, internal/compression,
#   internal/dnscache, internal/dnsparser, internal/logger, internal/security,
#   internal/streamutil, internal/udpserver, internal/vpnproto
#
# خروجی به فرمت استاندارد `go test -bench` با خط‌های `goos/goarch/pkg/cpu` نوشته می‌شود
# تا توسط scripts/benchregress/main.go قابل پارس باشد.

set -euo pipefail

OUT="${1:-bench-out.txt}"
BENCHTIME="${2:-1s}"
COUNT="${3:-1}"

# پکیج‌های کلیدی که در CI ارزیابی می‌شوند. ترتیب پایدار است تا diff آسان باشد.
PACKAGES=(
  "./internal/arq/..."
  "./internal/basecodec/..."
  "./internal/client/..."
  "./internal/compression/..."
  "./internal/dnscache/..."
  "./internal/dnsparser/..."
  "./internal/logger/..."
  "./internal/security/..."
  "./internal/streamutil/..."
  "./internal/udpserver/..."
  "./internal/vpnproto/..."
)

# پاک‌سازی فایل خروجی
: > "$OUT"

echo "[run_bench] benchtime=$BENCHTIME count=$COUNT packages=${#PACKAGES[@]}" >&2

# هدر env برای reproducibility
{
  echo "# bench-output v1"
  echo "# date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "# go: $(go version)"
  echo "# benchtime: $BENCHTIME"
  echo "# count: $COUNT"
} >> "$OUT"

for PKG in "${PACKAGES[@]}"; do
  echo "[run_bench] benching $PKG ..." >&2
  # `-run=^$` تست‌های واحد را خاموش می‌کند. `-benchmem` allocs را اضافه می‌کند.
  # خروجی append می‌شود تا یک فایل واحد همه پکیج‌ها را در بر بگیرد.
  # خطاهای پکیج‌هایی که بنچ ندارند، نادیده گرفته می‌شوند (no-op exit 0).
  if ! go test -run='^$' -bench=. -benchmem -benchtime="$BENCHTIME" -count="$COUNT" \
       -timeout=15m "$PKG" >> "$OUT" 2>&1; then
    echo "[run_bench] WARNING: $PKG returned non-zero" >&2
    # ادامه می‌دهیم چون ممکن است یک پکیج خراب باشد ولی بقیه را گزارش کنیم
  fi
done

echo "[run_bench] done → $OUT" >&2
