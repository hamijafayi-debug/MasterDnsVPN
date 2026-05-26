#!/usr/bin/env bash
# ==============================================================================
# install.sh — یک‌بار اجرا برای فعال‌سازی Step 27 (Android APK Pipeline)
# ==============================================================================
# این اسکریپت به‌صورت idempotent (قابل اجرای چندباره بدون آسیب) همه‌ی
# تغییرات workflow که سندباکس به دلیل نداشتن permission `workflows`
# نتوانست apply کند را اعمال می‌کند:
#
#   1. کپی build-android-apk.yml به .github/workflows/
#   2. تغییر go-version از 1.21 به 1.25 در build-go.yml و build-test.yml
#   3. افزودن job میانی `build-android` در build-go.yml
#   4. افزودن `build-android` به needs: release و قید if: always()
#   5. افزودن glob `*.apk` به find SHA256SUMS و softprops files
#
# نحوه‌ی استفاده (روی ماشین شخصی شما، نه سندباکس):
#
#   cd /path/to/MasterDnsVPN
#   git checkout genspark_ai_developer       # یا main اگر merge شده
#   bash docs/ci-templates/install.sh
#   git diff                                  # تغییرات را مرور کنید
#   git add -A
#   git commit -m "ci: install Android APK build pipeline (Step 27.2/27.3)"
#   git push
#
# پس از این کار، تنها چیزهایی که باقی می‌ماند Step 27.5 (variable + secrets)
# و 27.6 (ساخت فورک Android) است که در docs/ci-templates/README.md توضیح
# داده شده‌اند.
# ==============================================================================

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TEMPLATE_DIR="${REPO_ROOT}/docs/ci-templates"
WORKFLOW_DIR="${REPO_ROOT}/.github/workflows"
BUILD_GO="${WORKFLOW_DIR}/build-go.yml"
BUILD_TEST="${WORKFLOW_DIR}/build-test.yml"
APK_SRC="${TEMPLATE_DIR}/build-android-apk.yml"
APK_DST="${WORKFLOW_DIR}/build-android-apk.yml"

log()  { printf '[install.sh] %s\n' "$*"; }
warn() { printf '[install.sh] ⚠ %s\n' "$*" >&2; }
die()  { printf '[install.sh] ✗ %s\n' "$*" >&2; exit 1; }

[[ -d "${WORKFLOW_DIR}" ]] || die "Cannot find ${WORKFLOW_DIR}. Run from repo root."
[[ -f "${BUILD_GO}"     ]] || die "Cannot find ${BUILD_GO}."
[[ -f "${BUILD_TEST}"   ]] || die "Cannot find ${BUILD_TEST}."
[[ -f "${APK_SRC}"      ]] || die "Cannot find ${APK_SRC}."

# ------------------------------------------------------------------------------
# Step 1: copy build-android-apk.yml into .github/workflows/
# ------------------------------------------------------------------------------
log "Step 1/5: copying build-android-apk.yml → .github/workflows/"
if [[ -f "${APK_DST}" ]] && cmp -s "${APK_SRC}" "${APK_DST}"; then
  log "  ↳ already in place and identical, skipping."
else
  cp -v "${APK_SRC}" "${APK_DST}"
fi

# ------------------------------------------------------------------------------
# Step 2: bump go-version 1.21 → 1.25
# ------------------------------------------------------------------------------
log "Step 2/5: bumping go-version 1.21 → 1.25"
for f in "${BUILD_GO}" "${BUILD_TEST}"; do
  if grep -q 'go-version: "1\.21"' "$f"; then
    sed -i 's/go-version: "1\.21"/go-version: "1.25"/g' "$f"
    log "  ↳ updated $f"
  else
    log "  ↳ $f already at 1.25 (or non-matching), skipping."
  fi
done

# ------------------------------------------------------------------------------
# Step 3: insert build-android job + update release job in build-go.yml
# ------------------------------------------------------------------------------
log "Step 3/5: wiring build-android job into build-go.yml release pipeline"

if grep -q '^  build-android:' "${BUILD_GO}"; then
  log "  ↳ build-android job already present, skipping insertion."
else
  python3 - "${BUILD_GO}" <<'PYEOF'
import sys, re, pathlib
path = pathlib.Path(sys.argv[1])
text = path.read_text()

# Find the release: job header and inject build-android directly above it.
release_re = re.compile(
    r'^(  release:\n    name: Create Release and attach artifacts\n'
    r'    runs-on: ubuntu-latest\n'
    r'    needs: \[preflight, build\]\n)',
    re.MULTILINE,
)

build_android_block = (
    "  build-android:\n"
    "    name: Build Android APK (MasterDnsPro)\n"
    "    needs: preflight\n"
    "    permissions:\n"
    "      contents: read\n"
    "    uses: ./.github/workflows/build-android-apk.yml\n"
    "    secrets: inherit\n"
    "\n"
)

new_release_header = (
    "  release:\n"
    "    name: Create Release and attach artifacts\n"
    "    runs-on: ubuntu-latest\n"
    "    needs: [preflight, build, build-android]\n"
    "    # Always run the release job, even if the Android APK build was\n"
    "    # soft-skipped (ANDROID_APP_REPO not configured). The desktop/Termux\n"
    "    # release must still ship.\n"
    "    if: ${{ always() && needs.build.result == 'success' }}\n"
)

m = release_re.search(text)
if not m:
    print("[install.sh]   ↳ could not locate release: header — skipping (manual edit needed).", file=sys.stderr)
    sys.exit(0)

text = release_re.sub(build_android_block + new_release_header, text)
path.write_text(text)
print("[install.sh]   ↳ inserted build-android job and updated release job needs/if.")
PYEOF
fi

# ------------------------------------------------------------------------------
# Step 4: extend SHA256SUMS find to include *.apk
# ------------------------------------------------------------------------------
log "Step 4/5: extending SHA256SUMS find to include *.apk"
if grep -q "find release_assets -type f \\\\( -name '\\*.zip' -o -name '\\*.tar.gz' \\\\) -print0" "${BUILD_GO}"; then
  python3 - "${BUILD_GO}" <<'PYEOF'
import sys, pathlib
path = pathlib.Path(sys.argv[1])
text = path.read_text()
old = (
    "          find release_assets -type f \\( -name '*.zip' -o -name '*.tar.gz' \\) -print0 \\\n"
    "            | sort -z \\\n"
    "            | xargs -0 sha256sum > release_assets/SHA256SUMS.txt\n"
)
new = (
    "          # Include APK alongside zip/tar.gz so users can verify the\n"
    "          # Android download against the same SHA256SUMS.txt file.\n"
    "          find release_assets -type f \\\n"
    "            \\( -name '*.zip' -o -name '*.tar.gz' -o -name '*.apk' \\) -print0 \\\n"
    "            | sort -z \\\n"
    "            | xargs -0 sha256sum > release_assets/SHA256SUMS.txt\n"
)
if old in text:
    text = text.replace(old, new, 1)
    path.write_text(text)
    print("[install.sh]   ↳ patched SHA256SUMS find.")
else:
    print("[install.sh]   ↳ SHA256SUMS find pattern not found verbatim; manual review needed.")
PYEOF
else
  log "  ↳ SHA256SUMS already includes *.apk (or was hand-edited), skipping."
fi

# ------------------------------------------------------------------------------
# Step 5: add release_assets/**/*.apk to softprops files
# ------------------------------------------------------------------------------
log "Step 5/5: adding release_assets/**/*.apk to softprops files block"
if grep -q "release_assets/\\*\\*/\\*.apk" "${BUILD_GO}"; then
  log "  ↳ *.apk glob already present, skipping."
else
  python3 - "${BUILD_GO}" <<'PYEOF'
import sys, pathlib
path = pathlib.Path(sys.argv[1])
text = path.read_text()
old = (
    "          files: |\n"
    "            release_assets/**/*.zip\n"
    "            release_assets/**/*.tar.gz\n"
    "            release_assets/SHA256SUMS.txt\n"
)
new = (
    "          files: |\n"
    "            release_assets/**/*.zip\n"
    "            release_assets/**/*.tar.gz\n"
    "            release_assets/**/*.apk\n"
    "            release_assets/SHA256SUMS.txt\n"
)
if old in text:
    text = text.replace(old, new, 1)
    path.write_text(text)
    print("[install.sh]   ↳ added *.apk to softprops files.")
else:
    print("[install.sh]   ↳ softprops files block not found verbatim; manual review needed.")
PYEOF
fi

# ------------------------------------------------------------------------------
# Final validation
# ------------------------------------------------------------------------------
log "Validating YAML syntax of all workflows…"
python3 - <<'PYEOF'
import yaml, sys, pathlib
ok = True
for f in pathlib.Path(".github/workflows").glob("*.yml"):
    try:
        yaml.safe_load(f.read_text())
        print(f"  ✓ {f}")
    except yaml.YAMLError as e:
        print(f"  ✗ {f}: {e}")
        ok = False
sys.exit(0 if ok else 1)
PYEOF

log ""
log "========================================================================"
log "✓ install.sh complete. Next steps:"
log "    1. git diff      # review changes"
log "    2. git add -A"
log "    3. git commit -m 'ci: install Android APK build pipeline (Step 27)'"
log "    4. git push"
log ""
log "  Then complete Step 27.5 and 27.6 from docs/ci-templates/README.md:"
log "    • set ANDROID_APP_REPO repo variable"
log "    • set 4 signing secrets"
log "    • build the Android fork from iampedii/WhiteDNS with the core"
log "      submodule swapped to MasterDnsVPN webapp"
log "========================================================================"
