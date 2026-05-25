# Step 25 — CI Workflow Patch (manual apply required)

## Why this patch is in `docs/` instead of applied directly

GitHub denies pushes from this repository's GitHub App when those pushes
modify any file under `.github/workflows/`, because the App token does not
hold the `workflows` permission. Step 25 needed to repoint the release
Docker-image tags from the upstream owner (`masterking32`) to this fork
(`hamijafayi-debug`), but the change in `.github/workflows/build-go.yml`
could not be pushed by automation. The maintainer (with write access via a
personal token / web UI) must apply the one-line change manually.

The same situation occurred in Step 23 — see `docs/step23-ci-workflow-patch.md`.

## The change

In `.github/workflows/build-go.yml`, line 804 currently reads:

```yaml
          IMAGE_REFS: masterking32/masterdnsvpn:${{ needs.preflight.outputs.release_tag }},masterking32/masterdnsvpn:latest
```

It must be changed to:

```yaml
          IMAGE_REFS: hamijafayi-debug/masterdnsvpn:${{ needs.preflight.outputs.release_tag }},hamijafayi-debug/masterdnsvpn:latest
```

## Apply via web UI

1. Open the file at:
   https://github.com/hamijafayi-debug/MasterDnsVPN/edit/main/.github/workflows/build-go.yml
2. Jump to line 804 (search for `IMAGE_REFS:`).
3. Replace the two occurrences of `masterking32/masterdnsvpn` with `hamijafayi-debug/masterdnsvpn`.
4. Commit directly to `main` with the message:
   `step 25 patch: re-point release Docker image to fork (hamijafayi-debug/masterdnsvpn)`

## Apply via local clone

```bash
git clone https://github.com/hamijafayi-debug/MasterDnsVPN.git
cd MasterDnsVPN
sed -i 's|masterking32/masterdnsvpn|hamijafayi-debug/masterdnsvpn|g' .github/workflows/build-go.yml
git add .github/workflows/build-go.yml
git commit -m "step 25 patch: re-point release Docker image to fork (hamijafayi-debug/masterdnsvpn)"
git push origin main
```

## What still works without this patch

Everything *except* the published Docker image tag. Specifically:

- `server_linux_install.sh` already downloads from the fork's GitHub
  releases (this was the primary user-reported bug).
- `docker/Dockerfile`, `docker/docker-compose.yml`,
  `docker/docker-entrypoint.sh`, and both `docker/build-*.sh` scripts
  already reference the fork.
- The server and client startup banners now print the fork's URL.
- All README download/clone/pull commands point to the fork.

Without this patch, the CI pipeline will still **build** Docker images
correctly but will **push** them to a registry path the maintainer does
not own (`ghcr.io/masterking32/...`), which will fail with `403 Forbidden`
at the registry push step. After the patch, images publish to
`ghcr.io/hamijafayi-debug/masterdnsvpn:<tag>` and `:latest` as intended.
