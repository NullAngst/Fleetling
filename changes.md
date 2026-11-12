2026.11.12 - Backend overhaul: release flow
- Releases are made by hand: make the release and its tag on the Releases page, then run the build workflow on that tag from the Actions tab
- The build workflow no longer runs on a tag push; it has a Run workflow button instead
- The release job checks it was run on a tag that has a release, and says which mistake it was when not
- Binaries are attached with gh release upload on the runner, replacing the softprops/action-gh-release action
- README section "Making a release" with the steps
Reasons for changes: A pushed tag built and released on its own, which isn't how I make releases. Making the release first and then running the build on its tag keeps the release notes and the build in my hands, and drops one third-party action that held a write token.

2026.11.12 - Feature: self-management, Podman, builds, converter and auto-update
- Fleetling's own stack runs Update and Restart through a helper container (fleetling updater) that builds or pulls, runs up -d, waits 60 seconds for healthy, and re-tags the old image IDs and runs up -d --no-build when it isn't; every step goes to last-update.log
- The helper tags the current image <name>:fleetling-previous before building, since Docker's containerd image store deletes an untagged image the moment no container uses it
- Stop, Down and Remove on Fleetling's own stack are greyed out with the reason next to each; Save and redeploy on it goes through the helper
- Updating page that waits for the new process through /healthz, then shows the update log
- Podman detection on the new stack form, with the line each signal was found on
- Moving a stack between engines: down on the old one, engine rewritten in .fleetling.toml, up -d on the new one, with a plain warning that named volumes don't move
- The Podman endpoint test checks podman-restart.service and shows the command to enable it
- The containers page uses the shared engine switcher
- CI: integration tests against rootful Podman, a compose deploy on both engines, and an image build job that checks every tool runs
- Build button for stacks with build:, with pull and no-cache toggles and a redeploy offer afterwards
- Builds page for standalone docker build from a folder under the root or an https:// or git@ URL, with saved recipes; tokens in git URLs kept out of the action log
- Drift review covers local build contexts and Dockerfiles, honoring .dockerignore, skipping bind-mounted data, capped at 5000 files or 256 MB
- Converter page and stack Convert tab: normalized config, Podman Quadlet through podlet, Kubernetes through kompose, a lint with line numbers, copy, download as .tar, and save next to the stack
- kompose (release binary checked against its published SHA256_SUM) and podlet (cargo install --locked) in the image
- Auto-update per stack: daily, weekly or a cron expression in the container's time zone; pull, then up -d only if an image ID changed
- Every update run passes the drift gate first and is skipped, logged and flagged when files changed outside Fleetling; stopped stacks are left alone
- Failed or skipped updates show on the stacks page until the next good run
- Update available per service, from an anonymous registry digest check, cached for six hours
- The network form loads host interfaces with a POST after the page opens, so a GET never starts a container
- Stack folder names refuse # and ? and control characters
- FLEETLING_ORIGINS for reverse proxies that rewrite the Host header, for both websockets and form posts
- The deps workflow moves the go line to the newest Go, gates the PR on govulncheck and npm audit, and warns when DEPS_TOKEN has expired
- maintenance.md and this file
Reasons for changes: Phases 7 to 11 of the build plan, built in one go, plus the cleanup items left from earlier reviews. Auto-update was the reason drift review exists: nothing changed outside Fleetling gets deployed without a person looking at it first.

2026.10.07 - Feature: remove with folders
- Remove page for stacks: down, optional -v and --rmi all, then only the ticked folders, deepest first and the stack folder last
- Seven deletion safety rules, each with a test: under the root only, symlinks resolved first, never a path another stack or container uses, never a mount point, never follow symlinks while deleting, outside paths never offered, sizes shown first
- Every path checked again after down; each deleted or kept path logged with its reason
- created_paths read from the approved copy of .fleetling.toml
- Remove for single containers with the same folder options for their own bind mounts
- Deps workflow moves the pinned third-party action SHAs to each new release when DEPS_TOKEN is set
- SVG favicon
Reasons for changes: Phase 6 of the build plan. Deleting data is the riskiest thing Fleetling does, so every path has to pass every rule twice before it goes.

2026.10.07 - UX overhaul
- Container yard theme: steel sidebar, crane-yellow accent, dark console surface for commands, logs, files and the terminal
- Hazard band and named red button on destructive confirms
- Barlow, Barlow Condensed and Red Hat Mono bundled and self-hosted, with their OFL licenses
- Filter chips, segmented action toolbars with icons, facts strip on stack and container pages, YAML colors
- Phone layout: sideways-scrolling nav, stacked table rows
Reasons for changes: The first UI was too bland to use comfortably.

2026.10.05 - Security patches: TLS and drift review
- HTTPS on by default with a self-signed ECDSA P-256 certificate, renewed 30 days before expiry, fingerprint logged
- Drift review: each stack's deployment files are recorded, and every compose action is refused until changes made outside Fleetling are reviewed
Reasons for changes: Plain HTTP put the password and session cookie on the wire. A container that can write its own stack folder could change what gets deployed, and auto-update must never deploy that unreviewed.

2026.10.05 - Security patches: first review
- argon2 hashing limited to two at a time, so login floods can't exhaust memory
- CI keeps the token out of .git/config, npm installs run without scripts, third-party actions pinned to commits
- Portainer importer refuses redirects; login limiter counts IPv6 per /64; env var names validated; tokens stripped from git URLs in the log; log filter regex made linear
Reasons for changes: Security and code quality review before trying Fleetling on a real server.

2026.10.05 - Feature: containers, Portainer import, networks, images and volumes
- Containers: actions, live logs with frame stripping, web shell, inspect, stats
- Portainer import through its API or its data folder, relative path fix-up, retire button
- Networks with bridge, macvlan and ipvlan; image and volume lists, removal and prunes with previews
Reasons for changes: Phases 3 to 5 of the build plan, bringing Fleetling to parity with what Portainer did on this server.

2026.10.04 - Initial build
- First-run setup, login, settings, engine endpoint test, stack list with On disk and External
- Stack lifecycle: create, edit with diff, deploy, update, restart, recreate, start, stop, down, live output, action log, created_paths tracking
Reasons for changes: Phases 1 and 2 of the build plan.
