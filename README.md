# Fleetling

Fleetling is a single-user web app for managing Docker and Podman compose stacks on one homelab server, with every stack's compose file kept in its own folder under `/opt`.

It replaces the parts of Portainer CE that actually get used: stacks, containers, networks, images, volumes and builds. No Swarm, no Kubernetes runtime, no remote agents.

## Status

Every phase of the build plan is built. What works:

- First-run setup, login, sessions, "log out everywhere", password change.
- Settings: stack root, ignored folders, Docker and Podman endpoints with a Test button.
- The stack list: every folder under the root with a compose file, plus every running Compose project the engines report.
- Stacks: create, edit with a side-by-side diff, Deploy, Update, Restart, Recreate, Start, Stop and Down, each with live output in the browser.
- Manage: one click turns an On disk folder into a managed stack.
- Remove with folders: `down`, optionally with the stack's volumes and images, then only the folders you tick, each checked against seven safety rules first.
- Containers: list with stack and state filters, Start, Stop, Restart, Kill, Remove (with the same folder options for its own bind mounts), and Recreate for containers in a managed stack.
- Logs for one container or a whole stack, with follow, tail, since, timestamps, a filter, wrap and download.
- A web shell into any running container, plus Inspect and live CPU, memory and network stats.
- Networks: list with subnets and attached containers, create with bridge, macvlan, ipvlan or any other driver, connect and disconnect with a static IP and aliases, remove.
- Images: list with size and the containers using each, pull with live output, remove, and prune with a preview.
- Volumes: list with the containers using each, remove, and prune with a preview.
- The Portainer importer, through Portainer's API or straight from its data folder, plus a button to retire Portainer afterwards.
- The action log: every command Fleetling ran, every file it wrote and every shell opened, kept for 90 days.
- Self-management: Update and Restart on Fleetling's own stack, run by a helper container that rolls back to the old image if the new one doesn't come up healthy.
- Podman: the create form suggests the engine from the compose file and says which line made it think so, stacks move between engines, and the endpoint test checks `podman-restart.service`.
- Builds: a Build button for stacks with `build:`, a standalone builder for a folder or a git URL, and saved recipes.
- The converter: Podman Quadlet units through podlet and Kubernetes manifests through kompose, with a lint that lists what won't survive the trip.
- Auto-update per stack on a schedule, an "update available" check per image, and a skip for any stack whose files changed outside Fleetling.

Checking it every month or so is in [`maintenance.md`](maintenance.md). What changed and when is in [`changes.md`](changes.md).

## Prerequisites

- Docker with the Compose plugin, or rootful Podman with `podman.socket` and `podman-restart.service` enabled.
- A folder that holds your stacks. I use `/opt`, so in my case each app has `/opt/<app>/compose.yaml`. Yours could be `/containers` or `/root/containers`, see below.

Compose builds the image straight from this repo, so there's nothing to clone.

## Install

1. Make the folder Fleetling lives in: `mkdir -p /opt/fleetling`
2. Save this as `/opt/fleetling/compose.yaml`:

   ```yaml
   services:
     fleetling:
       build: https://github.com/NullAngst/Fleetling.git#main
       image: fleetling:local
       container_name: fleetling
       restart: unless-stopped
       ports:
         - "8420:8420"
       volumes:
         - /var/run/docker.sock:/var/run/docker.sock
         - /opt:/opt
       environment:
         - FLEETLING_ROOT=/opt
         - FLEETLING_DATA=/opt/fleetling/data
         # Your server's LAN IP and name, so the certificate covers them:
         # - FLEETLING_TLS_HOSTS=192.168.1.10,server.lan
   ```

   On Podman, add `- /run/podman/podman.sock:/run/podman/podman.sock` under `volumes`.
   Want schedules in your own time zone? Add `- TZ=America/New_York` under `environment`, or whichever zone is yours. Without it, schedules run in UTC.
3. Build and start it from that folder: `cd /opt/fleetling && docker compose up -d --build`
4. Grab the one-time setup token from the log: `docker logs fleetling`
5. Open `https://<server>:8420`. Your browser warns about the certificate, since Fleetling made it itself. Before accepting, compare its SHA-256 fingerprint (click the warning's details, or the padlock) with the `sha256=` line in `docker logs fleetling`. If they match, accept it.
6. Paste the token, pick a password of at least 12 characters, and confirm the stack root.

The setup token exists because whoever finishes setup first owns the Docker socket. Without it, anyone on your LAN who opened the page before you did would get root on the host.

## The stack root

Every stack lives in its own folder under the root. The root has to be mounted at the identical path inside and outside the container: `/opt:/opt`, never `/opt:/stacks`. Why? Compose runs inside the container but the engine resolves bind mounts on the host. A path in a compose file has to mean the same thing to both of them.

Fleetling checks its own mounts when you save the root and refuses one that doesn't line up, showing the exact volume line to add.

Using a different root, like `/containers`? Change both sides of the `/opt:/opt` line, set `FLEETLING_ROOT=/containers` and `FLEETLING_DATA=/containers/fleetling/data`, and keep the compose file in `/containers/fleetling`. Changing the root later in Settings moves nothing. Stacks under the old root show as External, and the confirm page lists them before it saves.

DO NOT ADD `:z` OR `:Z` TO THE ROOT MOUNT. On an SELinux host that relabels everything under the root, every app's data, in one go. Leave it off.

## Keep it on your LAN

Access to the Docker socket is root on the host, and Fleetling has full access to it. So treat the web UI like a root shell.

I beg you not to port-forward it. Put it behind your reverse proxy with TLS for LAN use. For remote access, use an SSH tunnel:

```
ssh -L 8420:localhost:8420 you@server
```

Then open `https://localhost:8420`.

Two things that catch people out:

- Docker publishes ports by writing its own iptables rules, and those skip UFW entirely. A `ufw deny 8420` does nothing for `"8420:8420"`. If your reverse proxy runs on the same host (NPMPlus with `network_mode: host` does), publish the port on loopback only, `"127.0.0.1:8420:8420"`, and point the proxy at `127.0.0.1:8420`. Then nothing but the proxy can reach Fleetling.
- Turning TLS off (below) puts the password and the session cookie on the wire in cleartext, and anyone who captures the cookie is root on the host for seven days or until you use "Log out everywhere". Only turn it off with the port bound to `127.0.0.1` behind a proxy on the same host.

### TLS

Fleetling serves HTTPS by default. On first start it makes a self-signed certificate in `<data>/tls/`, logs its SHA-256 fingerprint, and reuses it on every restart. It renews itself 30 days before it expires, roughly once a year, and logs the new fingerprint when it does. A self-signed certificate means one browser warning per browser; check the fingerprint once, accept it, and you're done until the next renewal.

Why no HSTS header? Because with a self-signed certificate, HSTS would turn the browser's warning into a hard block you can't click through.

| Variable | What it does |
| --- | --- |
| `FLEETLING_TLS_HOSTS` | Extra names and IPs for the generated certificate, comma separated: your server's LAN IP and DNS name, like `192.168.1.10,server.lan`. The container can't see those itself. Changing this list makes a new certificate. |
| `FLEETLING_TLS_CERT`, `FLEETLING_TLS_KEY` | Use your own certificate and key instead, PEM files mounted into the container. Replacing the files takes effect without a restart. |
| `FLEETLING_TLS` | `off` serves plain HTTP. Only for running behind a reverse proxy on the same host with the port bound to `127.0.0.1`. |

Behind NPMPlus, either set the proxy host's scheme to `https` (NPMPlus doesn't verify upstream certificates, so the self-signed one is fine), or set `FLEETLING_TLS=off` and publish the port as `"127.0.0.1:8420:8420"`.

### Behind a reverse proxy

Five failed logins from one address in 15 minutes locks that address out for 15 minutes. Behind a proxy, every request comes from the proxy's address. So one bad guess streak locks out everyone, including you.

Fix it by telling Fleetling which addresses are your proxy:

```yaml
    environment:
      - FLEETLING_TRUSTED_PROXIES=172.17.0.1
```

That takes IPs or CIDRs, comma-separated. Use whatever address your proxy connects from: `172.17.0.1` if it reaches Fleetling through the host's Docker bridge, the proxy container's IP if they share a network, could be something else, you'll have to check. Fleetling then reads the client address from `X-Forwarded-For` and believes `X-Forwarded-Proto: https` for the Secure cookie flag. It only trusts those headers from the addresses you list.

The proxy also needs to pass the original `Host` header, which most do by default. If yours rewrites it, every page loads but buttons fail with a cross-origin error and live output never connects, since both checks compare the browser's `Origin` with `Host`. Tell Fleetling the address you open it at:

```yaml
    environment:
      - FLEETLING_ORIGINS=https://fleetling.example.com
```

Exact origins, scheme and port included, comma separated. Only those are let in on top of the normal same-host rule. Don't list anything you don't open Fleetling from.

## Coming from Portainer

Open Import in the sidebar. Nothing is redeployed: the importer writes files, and the running containers are picked up by project name, which stays exactly the Portainer stack name.

1. Make an API token in Portainer under My account, Access tokens. A username and password work too.
2. Enter Portainer's URL, like `https://192.168.1.10:9443`, and the token. Leave Skip TLS verify ticked, since Portainer ships a self-signed certificate. The credentials stay in memory for that import only and never touch the disk.
3. Check the preview. For each compose stack it shows the project name, the folder it will go in, the env var count and any warnings. Swarm and Kubernetes stacks are listed as skipped. Stacks deployed from git are flagged, and the repo URL lands in `.fleetling.toml`.
4. Fix any folder the guess got wrong. The guess is the `/opt/<folder>` the compose file mounts most; a tie or no match falls back to the stack name and says so. A folder that already holds a stack is refused.
5. Approve the relative-path rewrites, if any are listed. Portainer resolved `./data` against its own folder, so the running container really uses something like `/opt/portainer/compose/3/data`. The preview shows that diff, and the rewrite keeps the stack pointed at the same data once it moves.
6. Write. Each stack gets `compose.yaml` exactly as Portainer returned it, `.env` from Portainer's env vars at mode 600, and `.fleetling.toml`. If the compose file reads Portainer's `stack.env`, the vars go there and `.env` becomes a symlink to it, since Compose only reads `.env` for `${VARIABLE}` interpolation.
7. Check the result page. Every imported stack should show Running with no deploy. One that says the project name didn't match means its containers are still External: stop and look before going further.
8. Retire Portainer from the same page. It stops and removes the Portainer container only. Its data stays, so you can bring it back.

DO NOT REMOVE STACKS INSIDE PORTAINER TO CLEAN UP. That runs `docker compose down` and deletes their containers, which are now Fleetling's.

### Offline import

If Portainer's API is down or the password is gone, point the offline import at Portainer's data folder (default `/opt/portainer`). It has to be visible inside the Fleetling container at the same path, which it is if it sits under the root. It reads `compose/<id>/docker-compose.yml` and maps each ID to a project through the running containers' `working_dir` label. A stack with no container pointing at it is skipped, never guessed.

Env vars come from `stack.env` when Portainer left one. Otherwise they can't be recovered exactly, so the preview lists every variable set on the containers that their images didn't set, and you tick the ones that belong in `.env`. Anything also set under `environment:` in the compose file starts unticked, since it probably came from there.

Networks need nothing; Portainer created them in Docker, so they already exist. Portainer-only data like access control and templates is dropped.

## Updating Fleetling itself

Open Fleetling's own stack and click Update. It needs the folder managed and its files reviewed, like any other stack: an unreviewed change to Fleetling's own compose file blocks its own update.

Fleetling can't run `docker compose up -d` on itself, since Compose replaces the container halfway through and the process doing the work dies with it. So Update starts a short-lived helper container from Fleetling's own image, with the socket and the root mounted at the same paths:

```
docker run --rm -d --name fleetling-updater -v /var/run/docker.sock:/var/run/docker.sock -v /opt:/opt \
  -e FLEETLING_ROOT=/opt -e FLEETLING_DATA=/opt/fleetling/data -e DOCKER_HOST=unix:///var/run/docker.sock \
  sha256:<fleetling's own image> updater --dir /opt/fleetling --mode update
```

The confirm shows that exact line. The image is given by ID, not by tag, since the build moves the tag and the helper has to keep running the old code. The helper then:

1. Records the image ID each service runs now, and tags it `<name>:fleetling-previous`, so `fleetling:fleetling-previous` for the install compose file. Why? With Docker's containerd image store, the default since Docker 29, an image is deleted as soon as its name moves to a new build and no container uses it, which is exactly what `up -d` does. The extra tag keeps the old image around to roll back to. It stays after a good update too, as your manual rollback point. If this tag can't be made, the update stops before changing anything.
2. Runs `build --pull` for services with `build:` (the install compose file builds from this repo) and `pull --ignore-buildable` for the rest.
3. Runs `up -d`.
4. Waits up to 60 seconds for every container to report healthy through the image's `HEALTHCHECK`.
5. If that doesn't happen, re-tags the recorded image IDs to their names and runs `up -d --no-build`, which puts the old version back.

The tradeoff of the extra tag: one previous image per service stays on disk. "Prune all unused" on the Images page removes it like any other unused image.

Every step goes to `<data>/last-update.log`. The browser shows "Updating, reconnecting", polls `/healthz` until a new process answers, then opens that log. A second helper is refused while `fleetling-updater` exists. Restart goes through the helper too, for the same reason.

On Fleetling's own stack, Stop, Down and Remove are greyed out with the reason next to each. Use the host for those.

If the Update button itself is broken, from `/opt/fleetling`:

```
docker compose build --pull && docker compose up -d
```

## Settings and environment

| Variable | Default | What it does |
| --- | --- | --- |
| `FLEETLING_ROOT` | `/opt` | Pre-fills the stack root on first run |
| `FLEETLING_DATA` | `<root>/fleetling/data` | Where `fleetling.db` lives: password hash, session secret, settings |
| `FLEETLING_ADDR` | `:8420` | Listen address |
| `FLEETLING_TLS` | on | `off` for plain HTTP behind a local proxy; see TLS above |
| `FLEETLING_TLS_HOSTS` | empty | Extra names for the self-signed certificate |
| `FLEETLING_TLS_CERT`, `FLEETLING_TLS_KEY` | empty | Your own certificate and key |
| `FLEETLING_TRUSTED_PROXIES` | empty | Proxy IPs or CIDRs whose forwarding headers are trusted |
| `FLEETLING_ORIGINS` | empty | Exact origins a Host-rewriting reverse proxy serves Fleetling under; see "Behind a reverse proxy" |
| `TZ` | UTC | Time zone for auto-update schedules |

Everything else is set in the UI. The database holds settings only. Stack state lives in the stack folders and in the engines, and Fleetling rediscovers it on every page load.

## Working with stacks

Every action runs the real Compose binary against the stack's folder, like this:

```
DOCKER_HOST=unix:///var/run/docker.sock \
  docker compose -p gitea --project-directory /opt/gitea -f /opt/gitea/compose.yaml --ansi never --progress plain up -d
```

The confirm dialog shows that exact line before anything runs. Output streams to the page while it runs, and the exit code plus the last 200 lines land in the action log. One action per stack at a time.

| Button | Runs |
| --- | --- |
| Deploy | `up -d` |
| Update | `pull`, then `up -d` |
| Restart | `restart` |
| Recreate | `up -d --force-recreate` |
| Start, Stop | `start`, `stop` |
| Down | `down` |
| Save and redeploy | `up -d --remove-orphans` |
| Build | `build`, with `--pull` and `--no-cache` if you tick them. Only on stacks where a service has `build:` |
| Pull and redeploy now | `pull --ignore-buildable`, then `up -d` only if an image ID changed. See "Auto-update" below |
| Remove | `down`, with `-v` and `--rmi all` if you tick them, then the folders you tick. See "Removing a stack" below |

A few things worth knowing:

- Your compose file is saved byte for byte, comments and all. Fleetling never adds labels, `x-` keys or a `name:`. The project name lives in `.fleetling.toml` and goes to Compose with `-p`. Browsers send CRLF line endings from text boxes; those are turned back into LF unless the file already used CRLF.
- Saving runs `docker compose config -q` first and refuses a file Compose rejects. Tick "Save anyway" to keep a half-finished file.
- `.env` is saved at mode 600. On the Env tab, values for keys containing PASS, SECRET, TOKEN or KEY are blurred until you click them. If `.env` is a symlink to another file in the stack folder, the edit goes to that file and the link stays.
- Files written into an existing folder keep that folder's owner. `/opt/jellyfin` stays owned by whoever owned it.
- On the first deploy, Fleetling notes which bind sources don't exist yet, and records the ones Docker created as `created_paths` in `.fleetling.toml`. Docker makes those as root-owned folders, and they are what Remove offers to clean up.
- If a compose file mounts the stack folder itself, like `- /opt/copyparty:/cfg`, the stack page says so. A compromised container that can write its own `compose.yaml` could add `privileged: true` or a `/:/host` mount. Fleetling catches that (see "Changes made outside Fleetling" below), but moving the app's data into a subfolder like `/opt/copyparty/cfg` means it can't happen in the first place. The same goes for any container that mounts `/opt` itself: it can also read `fleetling.db` in `/opt/fleetling/data`, which holds the session secret. Set `FLEETLING_DATA` to a path outside the root, on its own volume, if you run one.
- Fleetling's own stack runs Update and Restart through the update helper (see "Updating Fleetling itself"). Save and redeploy on it does the same.

## Changes made outside Fleetling

Fleetling keeps a record of every managed stack's deployment files: the compose file, `.env`, `.fleetling.toml`, every `env_file` a service names, every file pulled in with `include:` or `extends:`, and every local build context with its Dockerfile. The record is updated whenever Fleetling writes those files itself (create, save, Manage, import, the first deploy's path tracking) and when you approve a review. It lives in Fleetling's database, not in the stack folder, so a container that can write its own folder can't rewrite it too.

Before any compose action, Deploy, Update, Restart, Recreate, Start, Stop or Down, the files on disk are compared with that record. If anything differs, nothing runs. The stack is flagged on the stacks page and its own page, and the action takes you to a review page with a side-by-side diff of every changed file. Approve, and the current files become the record; then run the action again. Editing a flagged stack also goes through the review first, since the editor would otherwise load the changed text and approve it on save without anyone looking.

Why does Stop count? Because the project name lives in `.fleetling.toml`, and a changed project name would aim Stop or Down at a different stack.

Did you edit a compose file over SSH yourself? Then the review is just you confirming your own change, one click. Stacks managed before this check existed show "not reviewed yet" and need that one click once.

The scheduled auto-update runs the same check and skips a flagged stack instead of deploying it.

Build contexts are walked the way the builder sends them:

- `.dockerignore` is honored (`<Dockerfile>.dockerignore` first, the way BuildKit does it). The Dockerfile and `.dockerignore` are always recorded.
- Anything the stack bind-mounts is skipped. App data changes every day and would flag the stack every day. The Dockerfile is recorded even when it sits in a bind-mounted folder.
- Only the Dockerfile's content is kept for the diff. Every other context file is recorded by hash, so the review says which files changed but can't show how.
- Symlinks are recorded by where they point, not followed.
- The walk stops at 5000 files or 256 MB. Past that, the stack page says the build context is only partly covered, and files past the cap aren't checked at all. A `.dockerignore` that drops `node_modules`, `.git` and build output usually brings a context well under it.

Updating from an older Fleetling flags every stack that builds from a local folder once, since its context wasn't recorded before. One review each.

Not covered: git build contexts (fetched fresh on every build), and files behind `configs:` and `secrets:`, which apps often rewrite themselves.

## Removing a stack

Remove sits next to Down on the stack page and opens a page of its own, since it can delete data. Everything except the first line is off until you tick it:

| Option | Runs |
| --- | --- |
| Containers and stack networks | `down`, always |
| Named volumes | `down -v`. Each volume is listed by name; external ones are never touched |
| Images | `down --rmi all`, each one listed |
| Paths this stack created | Deletes each `created_paths` entry, with its size |
| Other bind sources under the root | Deletes each, with its size |
| The stack folder itself | Deletes `/opt/<folder>`, compose file and all |

Ticking volumes or any folder means typing the project name to confirm. The page shows the exact `down` line it will run and the list of what it deletes, in order.

It runs `down` first. If that fails, nothing is deleted. Then it checks every ticked path again, against a fresh look at every stack and container, and deletes them one at a time, deepest first and the stack folder last. Every deleted path gets its own line in the action log with its size, and so does every path it kept and why.

The rules each path has to pass, every one of them tested:

1. It's under the stack root, and it isn't the root itself.
2. With symlinks resolved, it's still under the root. A link pointing out of the root is refused.
3. No other stack's compose file names it, nothing inside it, and nothing it sits inside. Same for every mount of every other container on every engine. Fleetling runs `docker compose config` on every stack on disk to build that list, so `${VARIABLES}` count.
4. It isn't a mount point and has none inside it. Fleetling reads the kernel's mount table and also compares device IDs, so your NAS at `/opt/media` survives.
5. Deleting never follows a symlink. A link is removed as a link, and its target stays.
6. Bind sources outside the root, like `/dev/dri` or the Docker socket, are never offered. The page lists them as "never offered" so you know they were seen.
7. You see the full list with sizes before anything happens.

Fleetling's own data folder and anything on the ignore list are never deleted either.

A few things worth knowing:

- `created_paths` lives in `.fleetling.toml`, which a container that mounts its stack folder can write. So Remove reads it from the last approved copy of the file, a changed `.fleetling.toml` sends you to review first like any other action, and every entry still has to pass all seven rules. Listing `/etc` or another stack's folder there gets you nothing.
- If an engine you have configured doesn't answer, no folder can be ticked, since its containers might use any of them. `down` alone still works. Turn the endpoint off in Settings if you don't use it.
- Fleetling only sees mounts that existed when its container started, since Docker gives bind mounts private propagation by default. If you mount shares under the root after boot, restart Fleetling afterwards, or add `rslave` to the root mount so new mounts show up inside: `- /opt:/opt:rslave`. That's propagation, not an SELinux label, so the `:z` warning above doesn't apply to it.
- Fleetling's own stack and its own container can't be removed from inside.
- Try it on a copy of a stack or two first, in a VM if you can. This is the one part of Fleetling that deletes data.

## Containers, logs and the shell

Container actions go through the Engine API, the same calls `docker stop` and friends make. The confirm shows the equivalent command, like `docker kill gitea`, and that line goes in the action log. Remove opens the same kind of page as a stack's: it runs `docker rm -f` (stop and delete, named volumes stay) and offers the container's own bind sources under the root, with the same rules and the same typed confirm. A path another service in the same stack uses is kept. Recreate on a container in a managed stack runs `docker compose ... up -d --force-recreate --no-deps <service>`, so it picks up compose file changes the way Compose would.

Logs keep their colors. Containers without a TTY send stdout and stderr interleaved with 8-byte frame headers; Fleetling splits those properly, and stderr lines show in red. The stack Logs tab runs `docker compose logs`, so every line carries its service name. Following stops the moment you leave the page.

The shell starts `/bin/bash` if the image has it and `/bin/sh` if not, in one exec. Set Command to run something else (it's split on spaces, not run through a shell) and User to run as someone other than the image default. Resizing the browser resizes the terminal. Distroless and scratch images have no shell at all, and the terminal says so instead of hanging. Each session goes in the action log as `docker exec -it ...` with its exit code. What you type is never logged.

Fleetling's own container can't be stopped, restarted, killed or removed from its own page. Logs, shell and inspect work.

## Networks, images and volumes

The network form matches Portainer's: name, driver, subnet, gateway, IP range, parent interface, ipvlan mode, internal, attachable, IPv6 with its own subnet, labels and driver options. It sends the same request `docker network create` does, and the confirm shows that exact command, like:

```
docker network create -d macvlan --subnet 192.168.1.0/24 --gateway 192.168.1.1 --ip-range 192.168.1.192/27 -o parent=eth0 lan
```

Why is there a container flashing up once the form opens? Fleetling's own container can't see the host's interfaces, so it fills the parent dropdown by running a throwaway copy of its own image with `--network host` and reading `/sys/class/net`. The page asks for that list with a POST after it opens, so just loading the page never starts anything. The throwaway runs on whichever engine Fleetling itself runs on, since that's where its image is; both engines see the same host. There's a free-text box next to it in case that fails or your interface is missing. Could be eth0, could be enp3s0, you'll have to check.

A network with containers attached can't be removed; the page lists them. Networks Compose made for a stack are marked with the stack's name, and removing one warns you that Compose recreates it on the next deploy. Docker's built-in bridge, host and none can't be removed at all.

Every prune shows the list of what will go before it runs. Image prune comes in two sizes: dangling only, or every image no container uses. Volume prune does unused anonymous volumes by default, like `docker volume prune` does since Docker 23; "Prune all unused" adds named volumes too. A named volume holds data, so read that list.

## Podman

Fleetling talks to rootful Podman through its Docker-compatible socket and runs the same `docker compose` binary against it, with `DOCKER_HOST` pointing at the Podman socket and `DOCKER_BUILDKIT=0`, since Podman's API doesn't speak BuildKit's protocol. A stack runs on one engine, set in its `.fleetling.toml`. Docker is the default.

The new stack form reads the compose file as you type and suggests Podman when it finds something only Podman understands, with the line it found it on: "Suggested Podman: line 14 uses userns_mode: keep-id". It looks for:

| Setting | Why it means Podman |
| --- | --- |
| `userns_mode: keep-id` | Podman's user namespace mode |
| `group_add: keep-groups` | Podman only |
| `network_mode: slirp4netns` or `pasta` | Podman's rootless network stacks |
| volume option `U` | Podman chowns the source to the container user |
| `x-podman:` | Podman Compose's extension key |
| `image: localhost/...` | where Podman tags local builds |
| a bind of `podman.sock` | the stack talks to Podman's own API |

The suggestion only fills the field; pick the other engine and it stays picked. With one engine configured, the field is hidden and a Podman file gets a warning instead.

Podman has no daemon to bring containers back after a reboot. `podman-restart.service` does that, for every container with a restart policy. The Test button next to the Podman endpoint in Settings checks it's enabled, and if not, shows the command:

```
sudo systemctl enable --now podman-restart.service
```

A container can't ask the host's systemd, so the check runs a throwaway container from Fleetling's own image with `/etc/systemd/system` mounted read-only and looks for the link in `default.target.wants`.

### Moving a stack between engines

With both engines configured, the Engine line on a stack page has a move link. Moving runs `down` on the old engine, rewrites the engine in `.fleetling.toml`, then `up -d` on the new one, which pulls every image again. The confirm page shows both commands.

NAMED VOLUMES DO NOT MOVE. Their data stays inside the old engine, and the stack starts on the new one with empty volumes. Copy the data first if it matters. Bind mounts under the root are unaffected, since both engines see the same folders. A move is refused when a project with the same name already runs on the target engine.

## Builds

A stack where any service has `build:` gets a Build button. It runs `docker compose build`, with `--pull` and `--no-cache` as tick boxes; the command under the button changes with them. Building doesn't touch running containers. When it finishes, the stack page offers Redeploy.

Builds in the sidebar is for images that aren't part of a stack:

1. Pick the context: a folder under the root, like `myapp/build`, or a git URL. Only `https://` and `git@` URLs are allowed, never `file://`.
2. Fill in the Dockerfile path (relative to the context), one or more tags, and any build args, target stage or platform.
3. Click Check command. The page shows the exact `docker build` line.
4. Click Build. The log streams live and lands in the action log.

A token in an `https://user:token@...` URL is stripped from the action log and the page, and scrubbed from the stored output. Save a build as a recipe and it gets a Run button on the Builds page. Recipes live in Fleetling's database (mode 600), git URLs as typed, token included.

On Podman, builds run with BuildKit off, so `--progress` isn't passed and the output looks like the classic builder's.

## The converter

Converter in the sidebar, or the Convert tab on a stack, turns a compose file into:

| Tab | Made by |
| --- | --- |
| Normalized | `docker compose config`: variables filled in, short syntax expanded, paths absolute |
| Podman Quadlet | `podlet compose --pod`, one pane per unit file, with the three commands to install them |
| Kubernetes | `kompose convert --stdout`, with the volume type, Service type and controller as options |

A Podman stack in Fleetling needs no conversion: Fleetling runs the compose file on Podman as it is. Quadlet output is for running a stack under systemd without Fleetling.

Both tools read the normalized file, and both get a temporary copy, never your file. kompose has no flag for the Service type, so it goes in as kompose's `kompose.service.type` label on that copy. podlet's parser is pickier than Compose, so its copy has device maps turned back into `source:target` strings, `build:` dropped where `image:` is set, and `depends_on` conditions turned into plain dependencies. Every one of those is also a line in the warnings.

The warnings list is the most important part of the page. It goes through the normalized config and points at the line in your file for each of these:

| Setting | Quadlet | Kubernetes |
| --- | --- | --- |
| `network_mode: host` | works | hostNetwork at best, usually refused |
| `devices` | works as AddDevice= | dropped; needs a device plugin |
| `depends_on` with conditions | condition lost | ignored |
| `build` | not built | not built; push the image first |
| `container_name` | renamed under `--pod` | ignored |
| `healthcheck` | works | becomes a liveness probe that restarts the pod |
| `privileged`, `cap_add` | works on rootful | refused by Pod Security |
| relative paths | made absolute against the stack folder | host paths don't exist on a node |

Each file has a Copy button. Download all as .tar gets every file of one tab. Save next to stack writes them into `<root>/<folder>/quadlet/` or `<root>/<folder>/k8s/`, owned like the stack folder, and logs the write. Fleetling never installs Quadlets and never applies manifests.

A pasted file is converted as if it lived in `<root>/<project>`, so its relative paths come out the way they would for a real stack. Compose runs with a bare environment for it, so `${VARIABLES}` in pasted text can't read Fleetling's own environment.

## Auto-update

The Updates tab on a managed stack sets a schedule: off (the default), daily at 04:00, weekly on Sunday at 04:00, or any five-field cron expression. Times are in the container's time zone, and the tab says which zone that is. Set `TZ` in Fleetling's compose file to change it. A run that would have happened while Fleetling was down is skipped, not caught up.

Every run, scheduled or from the "Pull and redeploy now" button:

1. Checks the stack's files against the approved record. A stack whose files changed outside Fleetling is skipped, never updated. The skip goes in the action log with the reason, the stack keeps its review flag, and the stacks page shows "auto-update skipped".
2. Skips a stack with nothing running. An update never starts a stack you stopped or took down.
3. Records the image ID each running service uses.
4. Runs `pull --ignore-buildable`. Services that build aren't pulled.
5. Runs `up -d` only if an image ID changed, so nothing is recreated for no reason.

A failed run goes in the action log and puts "auto-update failed" on the stack in the list until the next good run. Fleetling's own stack uses the same schedule, through the update helper.

The Updates tab also checks each image against its registry: it compares the image's local `RepoDigests` with the digest the tag points at now, from a `HEAD` request, and shows "update available" per service. That request goes out anonymously, through `github.com/google/go-containerregistry`, and each result is cached for six hours. Private registries aren't checked; they show as unknown. Images you built yourself have nothing to compare and say so.

## How stacks are listed

Fleetling scans the direct children of the root for `compose.yaml`, `compose.yml`, `docker-compose.yml` or `docker-compose.yaml`, in the order Compose itself picks them. Hidden folders, symlinks and names on the ignore list (`containerd` by default) are skipped.

| Status | Meaning |
| --- | --- |
| Running, Partial, Stopped | A managed stack with a `.fleetling.toml`, and how many of its services are up |
| On disk | A folder with a compose file and no `.fleetling.toml` yet. Open it and click Manage |
| External | A running Compose project with no folder under the root, Portainer's stacks for example |
| Unknown | The stack's engine didn't answer |

A folder is matched to running containers by the `com.docker.compose.project.working_dir` label first, then by project name. Its project name comes from `.fleetling.toml`, then `COMPOSE_PROJECT_NAME` in `.env`, then a top-level `name:`, then the folder name, same as Compose.

Manage writes `.fleetling.toml` and nothing else. It takes the project name and engine from whatever is already running in that folder, so the containers are picked up as they are, with no restart.

If a folder has a `compose.override.yaml`, Fleetling flags it. Fleetling runs Compose with `-f <file>`, which means the override file is not applied. Merge it into the main file before managing the stack.

## Building from source

You need Go and Node, latest stable of each.

1. Bundle the browser code: `cd web/src && npm ci && npm run build && cd ../..`
2. Build the binary: `CGO_ENABLED=0 go build -o fleetling ./cmd/fleetling`
3. Run the tests: `go test -race ./...`

Skipping step 1 still builds; the pages then load without JavaScript and fall back to your system fonts.

Release binaries for linux amd64 and arm64 are attached to each `v*` tag by the build workflow.

### Dependencies

Fleetling always runs on the newest release of everything. The deps workflow runs every Monday: it moves the `go` line in `go.mod` to the newest Go, updates every Go module and npm package, runs the tests, `govulncheck` and `npm audit`, rebuilds the image with `--pull`, and opens a PR. The tradeoff is that a bad upstream release can break a build without warning. Tests gate every publish, so a broken release blocks the PR instead of reaching your server. `govulncheck` and `npm audit` gate it too, which means a vulnerability with no fix yet holds back the whole weekly update until one ships. The run log names it.

The image gets kompose and podlet at build time. podlet comes from `cargo install --locked podlet`, checked against crates.io. kompose can't be installed with `go install` (its `go.mod` has replace directives), so the Dockerfile downloads the latest release binary and checks it against the `SHA256_SUM` file published with that release. That catches a broken or swapped download. It can't catch a compromised release, since the sum comes from the same place.

After the first push, run the deps workflow once by hand from the Actions tab. That commits `go.sum`. Until then, the Dockerfile and CI create it with `go mod tidy`, which checks every module against the public checksum database. The workflow needs "Allow GitHub Actions to create and approve pull requests" switched on under Settings, Actions, General.

The two third-party actions, `softprops/action-gh-release` and `peter-evans/create-pull-request`, are pinned to a commit instead of a tag, since a tag can be moved to different code and the release job holds a write token. The deps workflow moves each pin to the commit of that action's newest release, so you never bump them by hand. It needs one token to do that, since GitHub never lets a workflow's built-in token change files under `.github/workflows`:

1. On GitHub, open Settings, Developer settings, Personal access tokens, Fine-grained tokens, and generate a new token.
2. Under Repository access, pick only this repository.
3. Under Permissions, set Contents, Pull requests and Workflows to Read and write.
4. In this repository, open Settings, Secrets and variables, Actions, and add it as a secret named `DEPS_TOKEN`.

Without it, the workflow still updates everything else and leaves a warning in the run when an action has a newer release. With it, the weekly PR also runs CI, which PRs opened by the built-in token don't. Fine-grained tokens expire. When one has, the deps run says "DEPS_TOKEN has expired or was revoked" and carries on with the built-in token; make a new one, as in `maintenance.md`.

## License

GPL-3.0. See `LICENSE`.

Now Fleetling is running on port 8420, listing every stack under your root next to everything the engines are running, updating what you scheduled, and changing nothing else until you tell it to.
