# Maintaining Fleetling

This is the monthly check I run on Fleetling from its GitHub page, plus what to do on the server afterwards. It takes about twenty minutes when nothing is wrong. Everything here uses the GitHub web UI and plain `git` and `docker` commands, no `gh` CLI.

You need:

- The repo open in a browser: `https://github.com/NullAngst/Fleetling`
- An SSH session on the server, in `/opt/fleetling` (or whichever folder holds Fleetling's compose file)

## 1. The weekly deps PRs

The deps workflow runs every Monday and opens a PR called "Weekly dependency update" when it finds something newer.

1. Open the Pull requests tab.
2. Open the newest "Weekly dependency update". There's only ever one; each week's replaces the last.
3. Scroll to the checks at the bottom. Green means vet, staticcheck, the race tests, govulncheck, npm audit and an image build all passed.
4. If green, click Merge pull request, then Confirm merge.
5. If there's no PR this week, open the Actions tab, click deps on the left, and open the newest run. Green with no PR means nothing changed. Red means it found a problem and didn't open one.

Reading a red run: click it, then click the step with the red X. What it usually means:

| Failed step | Usually means |
| --- | --- |
| Go and Go modules | A module was moved or retracted upstream. The error names it. |
| npm packages | A package's new major version changed how it builds. Check its changelog. |
| go vet, staticcheck | A new Go release or a module changed an API Fleetling uses. The error has the file and line. |
| go test -race | Real breakage from an update. The failing test name says which part. |
| govulncheck | A known vulnerability in Go or a module, with no fixed version yet. The output names the module and the advisory. |
| npm audit | Same, for the browser packages. |
| Image builds | A base image, the kompose download or the podlet build broke. See step 5 below. |
| create-pull-request | The token. See step 2. |

A red govulncheck or npm audit holds back the whole weekly update until the upstream fix ships. Check back next week, or bring the fix to me if it's urgent.

## 2. The DEPS_TOKEN secret

Fine-grained tokens expire. When this one has, the deps run shows a yellow warning: "DEPS_TOKEN has expired or was revoked". The run still works with the built-in token, but it can't move the pinned actions and CI doesn't run on its PR.

To make a new one:

1. Click your avatar, top right, then Settings.
2. At the bottom of the left menu, click Developer settings.
3. Click Personal access tokens, then Fine-grained tokens, then Generate new token.
4. Name it `fleetling-deps` and pick an expiry. A year is fine.
5. Under Repository access, pick Only select repositories, then `NullAngst/Fleetling`.
6. Under Permissions, Repository permissions, set Contents to Read and write.
7. Set Pull requests to Read and write.
8. Set Workflows to Read and write.
9. Click Generate token and copy it. GitHub only shows it once.
10. Back in the Fleetling repo, open Settings, then Secrets and variables, then Actions.
11. Click `DEPS_TOKEN`, then Update, paste the token, and Save. If it doesn't exist yet, click New repository secret and name it `DEPS_TOKEN`.
12. Open Actions, click deps, then Run workflow, to check the warning is gone.

## 3. Security alerts

1. Open the Security tab.
2. Click Dependabot alerts. Each one names a module or package and the fixed version. Most are fixed by merging the next deps PR; if the fix is out but the PR hasn't picked it up, run the deps workflow by hand (Actions, deps, Run workflow).
3. Click Code scanning, if it's turned on. Anything there points at a file and line.
4. Open the newest deps run in Actions and expand the govulncheck and npm audit steps. "No vulnerabilities found" and "found 0 vulnerabilities" are what you want.

## 4. Updating the running copy

Once the deps PR is merged, the server needs the new code.

1. Open Fleetling, then Stacks, then Fleetling's own stack.
2. Click Update. The confirm shows the exact `docker run` command for the helper.
3. The page says "Updating, reconnecting" and opens the update log when the new version answers. A build from the repo takes a few minutes.
4. Read the log. It ends in `done` when it worked. If it says `rolling back` and `FAILED: rolled back to the previous images`, the new build didn't come up healthy and the old one is running again. The lines above say why.

If the Update button is broken, from the server instead:

```
cd /opt/fleetling
docker compose build --pull && docker compose up -d
```

The log is `/opt/fleetling/data/last-update.log` (or `<FLEETLING_DATA>/last-update.log`), also on the Last update page. Read it from the server with:

```
cat /opt/fleetling/data/last-update.log
```

If a helper is stuck, `docker logs fleetling-updater` shows what it's doing, and `docker rm -f fleetling-updater` clears it.

## 5. Upstream changes worth a look

Every couple of months, skim the release notes of the things Fleetling leans on hardest. Most months nothing applies.

1. The moby client. Fleetling imports `github.com/moby/moby/client` and `github.com/moby/moby/api`. Moby has been splitting its Go modules; if a deps run fails with "module declares its path as", the import path moved. Check the moby releases page for the new path.
2. Compose CLI flags Fleetling passes: `--progress plain`, `--ansi never`, `config --format json`, `pull --ignore-buildable`, `up -d --no-build`. Check the Compose release notes for any of them being renamed or removed.
3. kompose and podlet. The converter reads their output: podlet's `# FileName=` headers and `---` separators, kompose's multi-document YAML. If the Converter page shows a parse error after an image rebuild, one of those changed. Check `https://github.com/kubernetes/kompose/releases` and `https://github.com/containers/podlet/releases`. If kompose stops publishing `SHA256_SUM` with its releases, the image build fails at the kompose stage on purpose.
4. Major versions of HTMX, CodeMirror and xterm.js. The deps workflow takes them as they come; a major version can change the API. If a page stops working after a deps merge, the browser's console (F12) names the script.

## 6. Stacks needing attention

1. Open Fleetling's Stacks page.
2. Look for red "changed outside Fleetling, review" flags. Open each and read the diff. If you didn't make the change, don't approve it; find out what did.
3. Look for "auto-update failed" and "auto-update skipped". Open the stack's Updates tab for the reason.
4. Open Action log and scan the Exit column for red entries since last month.

## 7. TLS

The certificate renews itself 30 days before it expires and logs the new fingerprint. After a renewal, your browser warns once again.

1. Check the current fingerprint: `docker logs fleetling 2>&1 | grep sha256= | tail -n 1`
2. Compare it with what the browser shows (click the warning's details, or the padlock).
3. If they match, accept it.

If they don't match and nothing renewed, stop and find out why before logging in.

## 8. Backups

Two things matter: `<data>/fleetling.db` (password hash, session secret, settings, approved file records, schedules, recipes, the action log) and `<data>/tls/` (the certificate). Everything else is in the stack folders.

1. Back up the data folder: `tar -C /opt/fleetling -czf ~/fleetling-data-$(date +%F).tar.gz data`
2. Every few months, test a restore: stop Fleetling with `docker stop fleetling`, move `data` aside, unpack the backup in its place, start it with `docker start fleetling`, log in, check the Stacks page, then stop it again and move the real `data` back if you want the newer one.

## 9. If something broke after an update

The helper rolls back on its own when the new version doesn't come up healthy. If it came up but something's wrong anyway:

1. Find the previous image. Every update keeps it as `fleetling:fleetling-previous`; `docker images fleetling` lists it next to `fleetling:local`.
2. Put it back under the name the compose file uses: `docker tag fleetling:fleetling-previous fleetling:local`
3. Recreate from it without building: `cd /opt/fleetling && docker compose up -d --no-build`
4. Open the Action log and filter on the stack, and read `changes.md` for what the update changed.

Now Fleetling is checked, updated and backed up, and you know what changed since last month.
