# Kiowy maintenance guide for cluster-api-provider-gcp

This fork (`kiowy-org/cluster-api-provider-gcp`) tracks upstream
[`kubernetes-sigs/cluster-api-provider-gcp`](https://github.com/kubernetes-sigs/cluster-api-provider-gcp)
with a small set of Kiowy-specific backports, published as custom images to
`ghcr.io/kiowy-org/cluster-api-gcp-controller`.

The canonical, machine-readable record of the upstream base and every
backport is [`kiowy/base.yaml`](./base.yaml). Keep it in sync with reality —
CI and this document both assume it is accurate.

## Branches

- `kiowy/release-1.13` — the approved maintenance branch. Created from the
  exact upstream `v1.13.1` tag/commit. All Kiowy release tags
  (`v1.13.1-kiowy.N`) must point at commits reachable from this branch.
  Protect it in GitHub settings: require the `Kiowy CI` checks
  (`test`, `lint`, `verify-generated`, `build-image`) before merge, require
  linear history, and restrict who can push directly.
- `backport/<description>` — one branch per upstream change being
  backported, based on `kiowy/release-1.13`, merged back via PR.
- Kiowy release tags are protected via a repository tag ruleset matching
  `v*-kiowy.*` that blocks force-push/deletion, so a tag can never be
  silently repointed once published.

## Backporting another upstream change

1. Identify the upstream PR/commit(s) to backport. Confirm it merged
   upstream and note its merge commit SHA (`gh pr view <n> --repo
   kubernetes-sigs/cluster-api-provider-gcp --json mergeCommit`).
2. Branch from the maintenance branch:
   ```sh
   git fetch origin kiowy/release-1.13
   git checkout -b backport/<short-description> origin/kiowy/release-1.13
   ```
3. Fetch and cherry-pick, preserving traceability:
   ```sh
   git fetch upstream <merge-commit-sha>
   git cherry-pick -x -m 1 <merge-commit-sha>
   ```
4. Resolve conflicts by preserving `kiowy/release-1.13` behavior except for
   the intended change — do not pull in unrelated upstream drift (e.g. an
   unrelated dependency bump that happens to conflict). Re-run `go mod tidy`
   only if the backport itself changes dependencies.
5. Run `make test`, `make lint`, `make verify`, and `make docker-build`
   locally, or push the branch and let `Kiowy CI` do it.
6. Add an entry under `backports:` in `kiowy/base.yaml` describing the
   upstream PR, merge commit, the cherry-pick commit SHA once merged, and
   why it's needed.
7. Open a PR into `kiowy/release-1.13` documenting: upstream base, the
   commit being backported, any conflict resolutions, tests run, and known
   limitations. Get it reviewed and merged.

## Fork-local enhancements

Use `fix/<description>` branches based on `kiowy/release-1.13` for changes
that are not upstream backports. Record these under `localChanges:` in
`kiowy/base.yaml`; do not invent an upstream PR or cherry-pick SHA.
The same Kiowy CI checks and PR review into the maintenance branch apply.
After merge, record the implementation commit in the machine-readable entry.

### Pre-provisioned kubeconfig service account

Set `spec.kubeconfigServiceAccountEmail` on a `GCPManagedControlPlane`, or
`spec.template.spec.kubeconfigServiceAccountEmail` on its template. For example:

```yaml
kubeconfigServiceAccountEmail: kubeconfig@example-project.iam.gserviceaccount.com
```

Provision the account separately (for example, through Crossplane). Grant
the controller's federated principal `roles/iam.workloadIdentityUser` on
that account, and authorize the account in the workload cluster using GKE
IAM or Kubernetes RBAC. Enable the IAM Service Account Credentials API.
This field changes only CAPI kubeconfig token generation; it does not
change the controller's Google Cloud API credentials or the node identity.
When omitted, explicit credentials and metadata-based service account
resolution retain their existing behavior. A direct-access workload pool
identifier produces an error asking for the explicit account field.

CAPG requests `cloud-platform` and `userinfo.email` scopes, stores token
expiry on the CAPI kubeconfig Secret, and schedules refresh five minutes
before expiry. Secrets from older versions refresh on the next reconcile.
Token-generation failures preserve the existing Secret. CAPI reconnects
with the latest Secret after its cached credential becomes unauthorized;
live validation must check this reconnect behavior and private endpoint
reachability. Downloaded bearer-token kubeconfigs expire unless refreshed
by a consumer; the separate user kubeconfig still uses the auth plugin.

Use an email topology variable and a ClusterClass patch when each cluster
needs a different account. Install the matching release CRDs before applying
the field; overriding only the controller image is insufficient.

## Publishing a new release (e.g. `v1.13.1-kiowy.2`)

Releases are built and published only by `.github/workflows/kiowy-release.yml`
— never by a local `docker build`/`docker push`.

1. Make sure the change is merged into `kiowy/release-1.13` and `Kiowy CI`
   is green on that branch.
2. Tag the exact commit on `kiowy/release-1.13` you want released:
   ```sh
   git fetch origin kiowy/release-1.13
   git checkout origin/kiowy/release-1.13
   git tag -a v1.13.1-kiowy.2 -m "Kiowy release v1.13.1-kiowy.2" <commit-sha>
   git push origin v1.13.1-kiowy.2
   ```
3. Pushing the tag triggers `Kiowy release`, which:
   - verifies the tagged commit is an ancestor of `kiowy/release-1.13`,
   - re-runs `make test`/`make lint`/`make verify` against the exact tagged
     source (never a moving branch),
   - refuses to proceed if the tag/image already exists (immutable
     releases),
   - builds `linux/amd64` + `linux/arm64`, pushes to
     `ghcr.io/kiowy-org/cluster-api-gcp-controller:v1.13.1-kiowy.2`,
   - attaches build provenance and an SPDX SBOM as GitHub attestations,
   - opens a PR to record the release digest in `kiowy/base.yaml`,
   - creates a GitHub Release with the source commit, upstream base, and
     backport record, attaching `infrastructure-components.yaml`,
     `metadata.yaml`, and cluster templates built from the tagged source.
4. The `kiowy-release` environment requires manual approval before the
   `release` job runs (configure required reviewers on that environment in
   repo settings) — approve it once you're ready to publish.
5. After the run completes, note the immutable digest from the release
   notes / workflow summary; that's what deployments should pin to.

## Identifying the source commit behind an image digest

```sh
# From the image itself:
crane config ghcr.io/kiowy-org/cluster-api-gcp-controller@sha256:<digest> \
  | jq -r '.config.Labels."org.opencontainers.image.revision"'

# Or via GitHub attestations (verifies provenance too):
gh attestation verify oci://ghcr.io/kiowy-org/cluster-api-gcp-controller@sha256:<digest> \
  --repo kiowy-org/cluster-api-provider-gcp

# Or look it up in kiowy/base.yaml under `releases:`, or in the matching
# GitHub Release notes.
```

## Rolling back

1. Pick the previous known-good digest (from `kiowy/base.yaml` `releases:`
   history or a prior GitHub Release).
2. Update the deployment's image reference back to
   `ghcr.io/kiowy-org/cluster-api-gcp-controller@sha256:<previous-digest>`
   in your own deployment/GitOps configuration and roll it out.
3. Kiowy release tags/images are immutable and never overwritten, so
   rolling back is always a pure "point at an older, still-published
   digest" — no rebuild needed. Do not delete the newer tag/release; keep it
   for forensics, and file a follow-up backport/issue for the regression.

## Migrating back to an official CAPG release

Once an official upstream CAPG release contains the Workload Identity
Federation support we need (i.e. an upstream tag whose history includes
`43be338b8ca1456fbba102179ec8b3fd309d8d23` or an equivalent fix):

1. Confirm behavior parity: the upstream image must resolve credentials via
   implicit ADC when neither `credentialsRef` nor
   `GOOGLE_APPLICATION_CREDENTIALS` is set (see `cloud/scope/credentials.go`
   `getCredentials`/`getCredentialDataUsingADC` behavior in this fork as the
   reference), and must still support explicit `credentialsRef` secrets.
2. Switch your deployment's image override back to the official
   `registry.k8s.io/cluster-api-gcp/cluster-api-gcp-controller:<version>`
   image.
3. Validate in a non-prod context first (WIF credential discovery, GKE
   kubeconfig generation, existing clusters reconcile cleanly).
4. Once validated, stop cutting new `kiowy.N` tags from this fork; keep the
   fork and its tags/images around (do not delete) for historical
   reference and potential rollback.
