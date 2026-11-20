2026.11.20 - Feature: versioning, Updates page, About page, self-update fixes

* VERSION file at the top of the repository, starting at 1.0.0, built into every binary and image. Before this, an image built from git reported "dev"
* Version numbering: each release bumps the middle number, 1.0.0 to 1.9.0, then 2.0.0. A test fails on a VERSION that breaks it, and fleetling version --next prints the next one
* The release workflow refuses a tag that doesn't match VERSION (a leading v or V is fine), and no longer passes the version with -ldflags
* New Updates page: Fleetling's running version against GitHub (VERSION on main, the newest release, the newest commit on main against the image's build time), with an Update Fleetling button that uses the existing helper and rollback. Every managed stack with its image check, schedule, last result and Update now, plus Update all. Containers outside managed stacks with an image check, read-only
* The GitHub check runs at most every 6 hours, in the background and from the page, and can be turned off on the Updates page. Updates gets a "new" mark in the sidebar when main has a newer version
* New About page: version and next release, Go and build settings, process, configuration, and per engine the host, OS, kernel, CPUs, memory, engine and Compose versions, storage and cgroups
* The sidebar version links to About
* Fix: Fleetling's own folder, not managed yet, showed "use Manage first" with no Manage button, so self-update couldn't be reached from the UI on a fresh install. It now has the button
* Fix: the self-update waiting page kept waiting forever when the helper failed before changing anything. It now stops and says the running Fleetling was never touched
* Engine: SystemInfo (from /info and /version), image build time in ImageInfo
* The stack page's image check and the Updates page share one checker
* The browser smoke test also loads Updates and About

Reasons for changes: Self-update existed but was hard to find, and on a fresh install couldn't be reached at all because of the missing Manage button. There was no way to tell whether a newer Fleetling existed, and images built from git all said "dev", so there was nothing to compare against. A version file with a fixed numbering gives every build a real version, and the Updates page puts Fleetling and every stack's updates in one place.

2026.11.19 - Feature: Portainer migration trial

* Every Portainer import records what it wrote into each folder: a hash per file, and the compose file as written
* Retire is now Stop Portainer: it turns the container's restart policy off and stops it, and removes nothing. The old policy is recorded
* New trial page (Import, or the banner on the Stacks page) showing, per imported stack, what going back removes and what it keeps, with a compose diff for stacks changed after the import
* Go back to Portainer: restores Portainer's restart policy, starts it, and removes .fleetling.toml plus every file still exactly as the import wrote it. Changed files and app data stay; a folder the import created goes only when empty. The approved snapshot and update schedule of each stack go too. No stack container is touched
* Finish migration removes Portainer's container, what Retire did before. Its data stays
* Going back is refused while an action runs on an imported stack, or when Portainer's container is gone
* Engine: restart policy read on inspect and set with docker update --restart; the test fake supports both
* The trial is a setting, not a new table, so no schema change

Reasons for changes: Retire removed Portainer on the spot, so trying Fleetling meant committing to it. Stopping Portainer and keeping a record of what the import wrote makes the move reversible, with nothing lost either way: no edits made in Fleetling and no app data. The restart policy has to be turned off during the trial, since Docker starts a container with --restart=always again on reboot even after a manual stop.

2026.11.18 - Security patches: drift review, logins, sessions and CI tokens

* Fleetling's own writes (save, create, import, convert, the first deploy's path tracking, moving engines) record only the files it wrote, from the bytes it wrote; every other file keeps its approved state instead of being re-read from disk and approved with them
* The first deploy and an engine move build .fleetling.toml from the approved copy, not from the file on disk
* Drift review runs again before every step of a stack job, and right before up -d in an update, so a file changed during a slow pull or build stops the job
* The login limiter counts a try when it starts, so parallel requests can't all get past it; a try that never checked a password (server busy, browser gone) doesn't count
* Password checks wait for an argon2 slot only as long as the request lasts, and past a queue of 16 they get "busy" at once
* Log out revokes that session: a copy of its cookie stops working, across restarts too. The list is kept as a setting, not a new table, so there is no schema change and a rollback to the previous image still opens the database
* Open shells and log streams close on Log out, Log out everywhere, a password change, and when their session expires
* Logged-in posts are capped at 16 MiB
* A stack folder that is a symlink can't be opened by its URL either
* Auto-update skips a stack whose engine doesn't answer instead of going ahead without checking whether it was stopped
* The Portainer import checks for an existing .env before writing anything, so a refused import leaves the folder as it was
* Browser smoke test (web/src/smoke.sh, smoke.mjs): headless Chrome through every page, failing on console errors, uncaught exceptions, missing assets, or an editor or shell that doesn't start; runs in the build and deps workflows
* govulncheck runs on every push, not only in the deps workflow
* The deps workflow is two jobs: the updated code runs with a read-only token and no secrets, and only the job that opens the PR sees DEPS_TOKEN
* The release is two jobs: binaries build with a read-only token, and the job with the write token only downloads them and runs gh release upload
* Older changes.md entries reformatted to this layout: a blank line after the title, * bullets, a blank line before the reasons

Reasons for changes: A review found that Fleetling's own file writes re-read the whole stack folder and approved whatever was there, so a container that rewrote its compose file during a first deploy got that change approved without anyone seeing it, and a change made during a long pull was deployed by the up -d after it. The login limiter could be bypassed by sending many logins at once, and the argon2 queue let anyone holding it open block real logins. A shell outlived Log out everywhere. And the deps workflow ran freshly updated packages in the same job as a token that can write workflows, which is how secrets get stolen from CI.

2026.11.12 - Backend overhaul: release flow

* Releases are made by hand: make the release and its tag on the Releases page, then run the build workflow on that tag from the Actions tab
* The build workflow no longer runs on a tag push; it has a Run workflow button instead
* The release job checks it was run on a tag that has a release, and says which mistake it was when not
* Binaries are attached with gh release upload on the runner, replacing the softprops/action-gh-release action
* README section "Making a release" with the steps

Reasons for changes: A pushed tag built and released on its own, which isn't how I make releases. Making the release first and then running the build on its tag keeps the release notes and the build in my hands, and drops one third-party action that held a write token.

2026.11.12 - Feature: self-management, Podman, builds, converter and auto-update

* Fleetling's own stack runs Update and Restart through a helper container (fleetling updater) that builds or pulls, runs up -d, waits 60 seconds for healthy, and re-tags the old image IDs and runs up -d --no-build when it isn't; every step goes to last-update.log
* The helper tags the current image <name>:fleetling-previous before building, since Docker's containerd image store deletes an untagged image the moment no container uses it
* Stop, Down and Remove on Fleetling's own stack are greyed out with the reason next to each; Save and redeploy on it goes through the helper
* Updating page that waits for the new process through /healthz, then shows the update log
* Podman detection on the new stack form, with the line each signal was found on
* Moving a stack between engines: down on the old one, engine rewritten in .fleetling.toml, up -d on the new one, with a plain warning that named volumes don't move
* The Podman endpoint test checks podman-restart.service and shows the command to enable it
* The containers page uses the shared engine switcher
* CI: integration tests against rootful Podman, a compose deploy on both engines, and an image build job that checks every tool runs
* Build button for stacks with build:, with pull and no-cache toggles and a redeploy offer afterwards
* Builds page for standalone docker build from a folder under the root or an https:// or git@ URL, with saved recipes; tokens in git URLs kept out of the action log
* Drift review covers local build contexts and Dockerfiles, honoring .dockerignore, skipping bind-mounted data, capped at 5000 files or 256 MB
* Converter page and stack Convert tab: normalized config, Podman Quadlet through podlet, Kubernetes through kompose, a lint with line numbers, copy, download as .tar, and save next to the stack
* kompose (release binary checked against its published SHA256_SUM) and podlet (cargo install --locked) in the image
* Auto-update per stack: daily, weekly or a cron expression in the container's time zone; pull, then up -d only if an image ID changed
* Every update run passes the drift gate first and is skipped, logged and flagged when files changed outside Fleetling; stopped stacks are left alone
* Failed or skipped updates show on the stacks page until the next good run
* Update available per service, from an anonymous registry digest check, cached for six hours
* The network form loads host interfaces with a POST after the page opens, so a GET never starts a container
* Stack folder names refuse # and ? and control characters
* FLEETLING_ORIGINS for reverse proxies that rewrite the Host header, for both websockets and form posts
* The deps workflow moves the go line to the newest Go, gates the PR on govulncheck and npm audit, and warns when DEPS_TOKEN has expired
* maintenance.md and this file

Reasons for changes: Phases 7 to 11 of the build plan, built in one go, plus the cleanup items left from earlier reviews. Auto-update was the reason drift review exists: nothing changed outside Fleetling gets deployed without a person looking at it first.

2026.10.07 - Feature: remove with folders

* Remove page for stacks: down, optional -v and --rmi all, then only the ticked folders, deepest first and the stack folder last
* Seven deletion safety rules, each with a test: under the root only, symlinks resolved first, never a path another stack or container uses, never a mount point, never follow symlinks while deleting, outside paths never offered, sizes shown first
* Every path checked again after down; each deleted or kept path logged with its reason
* created_paths read from the approved copy of .fleetling.toml
* Remove for single containers with the same folder options for their own bind mounts
* Deps workflow moves the pinned third-party action SHAs to each new release when DEPS_TOKEN is set
* SVG favicon

Reasons for changes: Phase 6 of the build plan. Deleting data is the riskiest thing Fleetling does, so every path has to pass every rule twice before it goes.

2026.10.07 - UX overhaul

* Container yard theme: steel sidebar, crane-yellow accent, dark console surface for commands, logs, files and the terminal
* Hazard band and named red button on destructive confirms
* Barlow, Barlow Condensed and Red Hat Mono bundled and self-hosted, with their OFL licenses
* Filter chips, segmented action toolbars with icons, facts strip on stack and container pages, YAML colors
* Phone layout: sideways-scrolling nav, stacked table rows

Reasons for changes: The first UI was too bland to use comfortably.

2026.10.05 - Security patches: TLS and drift review

* HTTPS on by default with a self-signed ECDSA P-256 certificate, renewed 30 days before expiry, fingerprint logged
* Drift review: each stack's deployment files are recorded, and every compose action is refused until changes made outside Fleetling are reviewed

Reasons for changes: Plain HTTP put the password and session cookie on the wire. A container that can write its own stack folder could change what gets deployed, and auto-update must never deploy that unreviewed.

2026.10.05 - Security patches: first review

* argon2 hashing limited to two at a time, so login floods can't exhaust memory
* CI keeps the token out of .git/config, npm installs run without scripts, third-party actions pinned to commits
* Portainer importer refuses redirects; login limiter counts IPv6 per /64; env var names validated; tokens stripped from git URLs in the log; log filter regex made linear

Reasons for changes: Security and code quality review before trying Fleetling on a real server.

2026.10.05 - Feature: containers, Portainer import, networks, images and volumes

* Containers: actions, live logs with frame stripping, web shell, inspect, stats
* Portainer import through its API or its data folder, relative path fix-up, retire button
* Networks with bridge, macvlan and ipvlan; image and volume lists, removal and prunes with previews

Reasons for changes: Phases 3 to 5 of the build plan, bringing Fleetling to parity with what Portainer did on this server.

2026.10.04 - Initial build

* First-run setup, login, settings, engine endpoint test, stack list with On disk and External
* Stack lifecycle: create, edit with diff, deploy, update, restart, recreate, start, stop, down, live output, action log, created_paths tracking

Reasons for changes: Phases 1 and 2 of the build plan.
