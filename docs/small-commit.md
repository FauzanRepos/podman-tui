# /small-commit (agent-neutral local workflow)

This repository has no existing agent command-registration convention. This is an equivalent local workflow, **not** a claim that every agent registers a slash command from a file. Invoke it by reading this document; the executable helper is `python3 hack/small-commit.py`.

1. Inspect `git status --short --branch`, `git diff`, and `git diff --cached`. Record pre-existing user changes before editing. Select one independently reviewable change. Split unrelated work; use patch staging manually when a file contains user-owned edits. Do not commit a whole mixed file with the helper.
2. Review the exact files and added lines, including untracked content. Check credentials, URLs, private keys, generated binaries, temporary files and changes to existing behavior. Confirm documentation identifies unsupported/unverified behavior.
3. Choose targeted validation. Go changes: run affected packages with the repository's vendored remote build tags, then build and vet. Deployment changes additionally require a throwaway real-runtime scenario where the required dependency exists. Documentation/workflow-only changes: inspect links/commands and exercise helper help. Existing failures belong in the baseline record, not disguised as passes.
4. Run the helper with the exact file allowlist, a Conventional Commit and one already selected validation command:

   ```sh
   python3 hack/small-commit.py --message 'feat(deploy): add application metadata' \
     --validate 'go test -mod=vendor -tags "exclude_graphdriver_btrfs containers_image_openpgp remote" ./deploy' \
     -- deploy/model.go deploy/model_test.go docs/deployment.md
   ```

   The validation command is trusted maintainer input, not application/repository data. The helper refuses any pre-existing staged changes, validates path confinement, checks selected content for likely credentials/artifacts, runs validation before staging, and rejects content changed during validation. It stages only the allowlist, verifies indexed bytes match the reviewed/validated bytes, and commits the index with DCO signoff. It never amends or pushes. Secret detection is a guardrail, not a security certification; manual review remains required.
   Negative credential-URL fixtures are admitted only in `_test.go` files using reserved `.invalid` hosts; key/token detection still applies. Exercise the guard with `python3 hack/small_commit_test.py`, which uses disposable repositories rather than the user's index.
5. Return the printed commit hash and logical summary. Continue with the next logical change. A failed validation creates no commit. If commit creation fails, the selected files remain staged; inspect and resolve deliberately, not by staging everything.

On a dirty baseline, use explicit patches and inspect the resulting index manually. Preserve unrelated changes. Never use `git add .`, `git add -A`, reset, clean, amend, force-update or push as part of this workflow.
