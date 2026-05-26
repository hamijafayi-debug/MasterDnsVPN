# Step 27 — CI Workflow Patch (manual apply required)

## Why this patch is in `docs/` instead of applied directly

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
The maintainer (with write access via a personal token / web UI) must
apply them manually.

The same situation occurred in Step 23 and Step 25 — see
`docs/step23-ci-workflow-patch.md` and `docs/step25-ci-workflow-patch.md`.

## The change

All of Step 27's workflow edits are packaged into a single, idempotent
shell script:

- **Script:** [`docs/ci-templates/install.sh`](ci-templates/install.sh)
- **New workflow file:** [`docs/ci-templates/build-android-apk.yml`](ci-templates/build-android-apk.yml)
- **Detailed README:** [`docs/ci-templates/README.md`](ci-templates/README.md) *(also created by the patch, see below)*

The script applies 5 changes:

| # | Where | Change |
|---|---|---|
| 1 | `.github/workflows/build-android-apk.yml` | Copy of the new workflow from `docs/ci-templates/` |
| 2 | `build-go.yml` line 248 and `build-test.yml` line 247 | `go-version: "1.21"` → `go-version: "1.25"` |
| 3 | `build-go.yml` (above the `release:` job) | New job `build-android:` that calls the new workflow via `uses: ./.github/workflows/build-android-apk.yml` with `secrets: inherit` |
| 4 | `build-go.yml` (`release:` job header) | `needs:` gains `build-android`; new `if: ${{ always() && needs.build.result == 'success' }}` so a soft-skipped Android build cannot drop the desktop release |
| 5 | `build-go.yml` (in `release:` job) | `find` for `SHA256SUMS.txt` and `softprops/action-gh-release@v1`'s `files:` list both gain a `*.apk` entry |

The script is **idempotent** — running it a second time is a no-op (each
step checks for prior application and skips). It also validates the final
YAML with `python3 yaml.safe_load` before exiting.

## Apply via local clone (recommended)

```bash
git clone https://github.com/hamijafayi-debug/MasterDnsVPN.git
cd MasterDnsVPN
git checkout main          # or genspark_ai_developer if you prefer a PR

bash docs/ci-templates/install.sh
git diff                    # review the 5 changes
git add -A
git commit -m "step 27 patch: install Android APK build pipeline + bump Go 1.21→1.25"
git push origin HEAD
```

## Apply via web UI (per-file, slower)

If you prefer the GitHub web UI you must do five separate commits. The
script is much easier; this manual path is here for completeness and
audit.

### 1. Create `.github/workflows/build-android-apk.yml`

Open
[https://github.com/hamijafayi-debug/MasterDnsVPN/new/main/.github/workflows](https://github.com/hamijafayi-debug/MasterDnsVPN/new/main/.github/workflows)
and paste the entire contents of
[`docs/ci-templates/build-android-apk.yml`](ci-templates/build-android-apk.yml).
Commit message:
`step 27 patch (1/5): add Android APK build workflow`

### 2. Bump Go version in `build-go.yml`

Open
[https://github.com/hamijafayi-debug/MasterDnsVPN/edit/main/.github/workflows/build-go.yml](https://github.com/hamijafayi-debug/MasterDnsVPN/edit/main/.github/workflows/build-go.yml)
and at **line 248** change:

```yaml
          go-version: "1.21"
```

to:

```yaml
          go-version: "1.25"
```

Commit: `step 27 patch (2/5): bump CI Go version 1.21 → 1.25`

### 3. Bump Go version in `build-test.yml`

Open
[https://github.com/hamijafayi-debug/MasterDnsVPN/edit/main/.github/workflows/build-test.yml](https://github.com/hamijafayi-debug/MasterDnsVPN/edit/main/.github/workflows/build-test.yml)
and at **line 247** apply the identical `1.21` → `1.25` change.

Commit: `step 27 patch (3/5): bump test CI Go version 1.21 → 1.25`

### 4. Insert `build-android` job + update `release` job header

Open
[https://github.com/hamijafayi-debug/MasterDnsVPN/edit/main/.github/workflows/build-go.yml](https://github.com/hamijafayi-debug/MasterDnsVPN/edit/main/.github/workflows/build-go.yml)
and locate the `release:` job header. Currently it reads (around line 647):

```yaml
  release:
    name: Create Release and attach artifacts
    runs-on: ubuntu-latest
    needs: [preflight, build]
    steps:
```

Replace it with:

```yaml
  build-android:
    name: Build Android APK (MasterDnsPro)
    needs: preflight
    permissions:
      contents: read
    uses: ./.github/workflows/build-android-apk.yml
    secrets: inherit

  release:
    name: Create Release and attach artifacts
    runs-on: ubuntu-latest
    needs: [preflight, build, build-android]
    # Always run the release job, even if the Android APK build was
    # soft-skipped (ANDROID_APP_REPO not configured). The desktop/Termux
    # release must still ship.
    if: ${{ always() && needs.build.result == 'success' }}
    steps:
```

Commit: `step 27 patch (4/5): wire build-android into release pipeline`

### 5. Add `*.apk` to the release file globs

In the same `build-go.yml`, find this block (around line 744):

```yaml
      - name: Build unified SHA256SUMS for release assets
        run: |
          set -euo pipefail
          find release_assets -type f \( -name '*.zip' -o -name '*.tar.gz' \) -print0 \
            | sort -z \
            | xargs -0 sha256sum > release_assets/SHA256SUMS.txt
```

Change it to:

```yaml
      - name: Build unified SHA256SUMS for release assets
        run: |
          set -euo pipefail
          # Include APK alongside zip/tar.gz so users can verify the
          # Android download against the same SHA256SUMS.txt file.
          find release_assets -type f \
            \( -name '*.zip' -o -name '*.tar.gz' -o -name '*.apk' \) -print0 \
            | sort -z \
            | xargs -0 sha256sum > release_assets/SHA256SUMS.txt
```

Then a few lines below find:

```yaml
          files: |
            release_assets/**/*.zip
            release_assets/**/*.tar.gz
            release_assets/SHA256SUMS.txt
```

And insert `release_assets/**/*.apk` so it becomes:

```yaml
          files: |
            release_assets/**/*.zip
            release_assets/**/*.tar.gz
            release_assets/**/*.apk
            release_assets/SHA256SUMS.txt
```

Commit: `step 27 patch (5/5): attach Android APK to releases & SHA256SUMS`

## What still works without this patch

Everything **except** the Android APK. Specifically:

- All 18 desktop / Termux binaries are still built and released by the
  existing matrix (Windows × 3, Linux × 9, macOS × 2, Termux × 2).
- `server_linux_install.sh` continues to download server binaries from
  the fork's releases.
- Docker images (Step 25) and PGO release flags (Step 23) are unaffected.

Without the patch, however:

- The 1.21 → 1.25 Go-version mismatch means the next CI run on a clean
  cache will pick a Go toolchain older than `go.mod` requires and will
  most likely fail with `requires go 1.25.0 (running go 1.21.x)`. This
  is the main reason Step 27.1 is high priority.
- There is no graphical Android APK to ship to users who can't run
  Termux.

## Follow-up actions still required by the maintainer (Step 27.5 / 27.6)

These are *not* in the patch because they are external to the repo:

1. **Set the Android repo variable** at
   `Settings → Secrets and variables → Actions → Variables`:
   - `ANDROID_APP_REPO` = `hamijafayi-debug/MasterDnsPro-Android` (or the
     final fork name)
   - `ANDROID_APP_REF` (optional) = branch / tag, default `main`

2. **Set 4 signing secrets** at
   `Settings → Secrets and variables → Actions → Secrets`:
   - `ANDROID_SIGNING_KEY` (base64 of `release.jks`)
   - `ANDROID_KEY_ALIAS`
   - `ANDROID_KEYSTORE_PASS`
   - `ANDROID_KEY_PASS`

   Generate the keystore once locally:
   ```bash
   keytool -genkey -v -keystore release.jks -keyalg RSA \
     -keysize 2048 -validity 10000 -alias masterdnspro
   base64 -i release.jks | tr -d '\n' > keystore_b64.txt
   # paste the contents of keystore_b64.txt into ANDROID_SIGNING_KEY
   ```

3. **Build the Android fork**:
   1. Fork `https://github.com/iampedii/WhiteDNS` to your account.
   2. Read & honour the upstream `TRADEMARK.MD`.
   3. Swap the Go core submodule:
      ```bash
      git submodule deinit -f third_party/StormDNS
      git rm -f third_party/StormDNS
      git submodule add https://github.com/hamijafayi-debug/MasterDnsVPN.git third_party/MasterDnsVPN
      git commit -am "swap core: StormDNS → MasterDnsVPN (26-step support)"
      ```
   4. In `app/build.gradle.kts`, rename the Go module/package from
      `stormdns-go` to `masterdnsvpn-go`.
   5. Re-brand the app to **MasterDnsPro** (manifest, `strings.xml`,
      icon, colors).
   6. Push the fork to its `main` branch.

WhiteDNS / StormDNS does **not** support the 26 protocol-improvement
steps shipped in this webapp. The submodule swap above is what makes the
APK consume those improvements — without it, the produced APK would be a
plain WhiteDNS rebrand and would not interoperate with the new server.
