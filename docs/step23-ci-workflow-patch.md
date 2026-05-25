# Step 23 — CI workflow patch (apply manually)

The GitHub App token used by the AI developer flow does not have the
`workflows` permission required to push changes to files under
`.github/workflows/`. The CI matrix build hardening from Step 23 must
therefore be applied by a maintainer with `workflows` scope.

Apply this patch by hand to `.github/workflows/build-go.yml` and commit:

## Change 1 — Hardened `Build client` step (around line 304)

Add `-trimpath` and `-s -w` to the existing ldflags:

```yaml
          go build -trimpath -ldflags "-s -w -X masterdnsvpn-go/internal/version.BuildVersion=${{ needs.preflight.outputs.release_tag }}" -o dist/MasterDnsVPN_Client_${{ matrix.platform }}_${{ matrix.arch }}${{ matrix.ext }} ./cmd/client
```

## Change 2 — Hardened `Build server` step (around line 317)

Same flags applied to the server build line.

## Why this isn't in the AI-generated PR

GitHub Apps with the default permission set cannot push to paths under
`.github/workflows/`. The `workflows: write` permission must be granted
separately. The rest of Step 23 (Makefile targets, bench harness PGO
mode, default.pgo files, documentation) is fully self-contained and is
in the PR.

## Expected outcome after applying

- Each binary in the dist/ artifact shrinks ~30% (Linux amd64: client
  11.3MB → 7.8MB, server 11.1MB → 7.7MB).
- Builds become reproducible (no absolute paths leak).
- PGO is auto-enabled because `cmd/{client,server}/default.pgo` are
  already committed (Go 1.21+ detects them).
