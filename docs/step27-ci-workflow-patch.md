# Step 27 — CI Workflow Patch (✅ APPLIED 2026-05-26)

> **Status: COMPLETED.** This patch was applied manually by the maintainer via
> the GitHub web UI on 2026-05-26. All five workflow changes listed below are
> now live on `main`. This document is kept for **historical reference** only,
> consistent with `docs/step23-ci-workflow-patch.md` and
> `docs/step25-ci-workflow-patch.md`.

## Why this patch was in `docs/` instead of applied directly

GitHub denies pushes from this repository's GitHub App when those pushes
modify any file under `.github/workflows/`, because the App token does not
hold the `workflows` permission. Step 27 needed to:

1. Bump Go version 1.21 → 1.25 in both `build-go.yml` and `build-test.yml`
   (`go.mod` already requires `go 1.25.0`).
2. Add a new workflow file `build-android-apk.yml` that produces the
   official **MasterDnsPro** graphical Android APK.
3. Wire that new workflow into the release pipeline so the APK is
   attached to every GitHub Release alongside the desktop / Termux
   binaries.

None of these `.github/workflows/` changes could be pushed by automation.
The maintainer (with write access via a personal token / web UI) applied
them manually on 2026-05-26.

The same situation occurred in Step 23 and Step 25 — see
`docs/step23-ci-workflow-patch.md` and `docs/step25-ci-workflow-patch.md`.

## The five changes that were applied

All edits below are now live on `main` (commits `4bc4af0`, `624692e`,
`d642134`, `d6091c1`, etc.):

### 1. Added `.github/workflows/build-android-apk.yml`

A complete callable workflow (~13 KB) that builds the **MasterDnsPro**
graphical APK. Key features:

- **Soft-skip** when `vars.ANDROID_APP_REPO` is empty (release still ships
  desktop / Termux binaries without an APK).
- **Submodule verification** — checks that the Android fork's core
  submodule points at `hamijafayi-debug/MasterDnsVPN` (this project), not
  at the upstream `iampedii/StormDNS` from WhiteDNS.
- **Unsigned fallback** when any of the 4 signing secrets is missing
  (debug-signed APK with a warning instead of build failure).
- **Final artifact:** `MasterDnsPro_Android_Universal.apk` +
  `MasterDnsPro_Android_Universal.apk.sha256`, uploaded as artifact
  `android-apk` for the `release` job to attach.

### 2. `build-go.yml` — `go-version: "1.21"` → `"1.25"` (line 248)

Aligns CI with `go.mod` (`go 1.25.0`). Previously CI built with Go 1.21
even though the source explicitly required 1.25, causing the Go toolchain
to auto-download 1.25 at job start (slow + brittle).

### 3. `build-test.yml` — `go-version: "1.21"` → `"1.25"` (line 247)

Same as above for the smoke-test workflow.

### 4. `build-go.yml` — new `build-android` job (line 647)

Inserted above the `release` job:

```yaml
build-android:
  name: Build Android APK (MasterDnsPro)
  needs: preflight
  permissions:
    contents: read
  uses: ./.github/workflows/build-android-apk.yml
  secrets: inherit
```

### 5. `build-go.yml` — `release` job wired to APK

- `needs:` updated to `[preflight, build, build-android]`
- `if:` condition: `${{ always() && needs.build.result == 'success' }}`
  (so release still runs even if Android APK soft-skipped).
- `find release_assets -type f \( -name '*.zip' -o -name '*.tar.gz' -o -name '*.apk' \)`
  for SHA256SUMS aggregation.
- `release_assets/**/*.apk` added to `softprops/action-gh-release` files
  block.

## What still requires manual action by the maintainer

After the workflow patch is live (it is, as of 2026-05-26), three more
things have to happen **outside this repo** before APKs actually appear
in releases:

### 27.5 — Repo settings

In `Settings → Secrets and variables → Actions`:

**Variables (Variables tab):**
- `ANDROID_APP_REPO` = `hamijafayi-debug/MasterDnsPro-Android` (or whatever
  you named your WhiteDNS fork)
- `ANDROID_APP_REF` (optional) = `main`

**Secrets (Secrets tab) — 4 required:**
- `ANDROID_SIGNING_KEY` (base64-encoded `.jks` keystore)
- `ANDROID_KEY_ALIAS`
- `ANDROID_KEYSTORE_PASS`
- `ANDROID_KEY_PASS`

Generate the keystore with:
```bash
keytool -genkey -v -keystore masterdnspro.jks -keyalg RSA -keysize 4096 \
        -validity 36500 -alias masterdnspro
base64 -w0 masterdnspro.jks   # paste output into ANDROID_SIGNING_KEY
```

### 27.6 — Android app fork

1. Fork `https://github.com/iampedii/WhiteDNS` to
   `hamijafayi-debug/MasterDnsPro-Android` (or your chosen name).
2. Inside that fork:
   - `git submodule deinit -f core/` (the StormDNS submodule)
   - `git rm -rf core/`
   - `git submodule add https://github.com/hamijafayi-debug/MasterDnsVPN.git core`
   - `git commit -m "swap core submodule from StormDNS to MasterDnsVPN"`
3. Rebrand WhiteDNS → MasterDnsPro per this project's `TRADEMARK.MD`
   (app name, package id, icon, strings, etc.).
4. Push to your fork's `main`.

Once 27.5 and 27.6 are done, the next push of a release tag (or manual
"Run workflow" on `build-go.yml`) will produce
`MasterDnsPro_Android_Universal.apk` in the release alongside the 18
desktop / Termux artifacts. The README's APK download link will work.

## What works without 27.5 / 27.6

The release still ships **all 18 desktop / Termux artifacts** even if the
APK isn't built — the `build-android` job soft-skips when
`ANDROID_APP_REPO` is empty, and the `release` job has
`if: ${{ always() && needs.build.result == 'success' }}` so it always
proceeds as long as the main `build` matrix succeeds.

The only user-visible consequence is that the **APK download link in
`README.MD` / `README_FA.MD` returns 404** until 27.6 is completed.
