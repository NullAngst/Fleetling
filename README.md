# Fleetling

Fleetling is a single-user web app for managing Docker and Podman compose stacks on one homelab server, with every stack's compose file kept in its own folder under `/opt`.

It replaces the parts of Portainer CE that actually get used: stacks, containers, networks, images, volumes and builds. No Swarm, no Kubernetes runtime, no remote agents.

## Status

This is phase 5 of 11. What works right now:

- First-run setup, login, sessions, "log out everywhere", password change.
- Settings: stack root, ignored folders, Docker and Podman endpoints with a Test button.
- The stack list: every folder under the root with a compose file, plus every running Compose project the engines report.
- Stacks: create, edit with a side-by-side diff, Deploy, Update, Restart, Recreate, Start, Stop and Down, each with live output in the browser.
- Manage: one click turns an On disk folder into a managed stack.
- Containers: list with stack and state filters, Start, Stop, Restart, Kill, Remove, and Recreate for containers in a managed stack.
- Logs for one container or a whole stack, with follow, tail, since, timestamps, a filter, wrap and download.
- A web shell into any running container, plus Inspect and live CPU, memory and network stats.
- Networks: list with subnets and attached containers, create with bridge, macvlan, ipvlan or any other driver, connect and disconnect with a static IP and aliases, remove.
- Images: list with size and the containers using each, pull with live output, remove, and prune with a preview.
- Volumes: list with the containers using each, remove, and prune with a preview.
- The Portainer importer, through Portainer's API or straight from its data folder, plus a button to retire Portainer afterwards.
- The action log: every command Fleetling ran, every file it wrote and every shell opened, kept for 90 days.

After this phase Fleetling covers everything Portainer did on this server. "Remove with folders", the safety-railed delete for a stack and its data, arrives in phase 6. The full plan is in the build spec.

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
   ```

   On Podman, add `- /run/podman/podman.sock:/run/podman/podman.sock` under `volumes`.
3. Build and start it from that folder: `cd /opt/fleetling && docker compose up -d --build`
4. Grab the one-time setup token from the log: `docker logs fleetling`
5. Open `http://<server>:8420`, paste the token, pick a password of at least 12 characters, and confirm the stack root.

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

Then open `http://localhost:8420`.

### Behind a reverse proxy

Five failed logins from one address in 15 minutes locks that address out for 15 minutes. Behind a proxy, every request comes from the proxy's address. So one bad guess streak locks out everyone, including you.

Fix it by telling Fleetling which addresses are your proxy:

```yaml
    environment:
      - FLEETLING_TRUSTED_PROXIES=172.17.0.1
```

That takes IPs or CIDRs, comma-separated. Use whatever address your proxy connects from: `172.17.0.1` if it reaches Fleetling through the host's Docker bridge, the proxy container's IP if they share a network, could be something else, you'll have to check. Fleetling then reads the client address from `X-Forwarded-For` and believes `X-Forwarded-Proto: https` for the Secure cookie flag. It only trusts those headers from the addresses you list.

The proxy also needs to pass the original `Host` header, which most do by default.

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

## Updating

Once phase 7 lands, this is the Update button. For now, from `/opt/fleetling`:

```
docker compose build --pull && docker compose up -d
```

## Settings and environment

| Variable | Default | What it does |
| --- | --- | --- |
| `FLEETLING_ROOT` | `/opt` | Pre-fills the stack root on first run |
| `FLEETLING_DATA` | `<root>/fleetling/data` | Where `fleetling.db` lives: password hash, session secret, settings |
| `FLEETLING_ADDR` | `:8420` | Listen address |
| `FLEETLING_TRUSTED_PROXIES` | empty | Proxy IPs or CIDRs whose forwarding headers are trusted |

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

A few things worth knowing:

- Your compose file is saved byte for byte, comments and all. Fleetling never adds labels, `x-` keys or a `name:`. The project name lives in `.fleetling.toml` and goes to Compose with `-p`. Browsers send CRLF line endings from text boxes; those are turned back into LF unless the file already used CRLF.
- Saving runs `docker compose config -q` first and refuses a file Compose rejects. Tick "Save anyway" to keep a half-finished file.
- `.env` is saved at mode 600. On the Env tab, values for keys containing PASS, SECRET, TOKEN or KEY are blurred until you click them. If `.env` is a symlink to another file in the stack folder, the edit goes to that file and the link stays.
- Files written into an existing folder keep that folder's owner. `/opt/jellyfin` stays owned by whoever owned it.
- On the first deploy, Fleetling notes which bind sources don't exist yet, and records the ones Docker created as `created_paths` in `.fleetling.toml`. Docker makes those as root-owned folders, and they are what the "remove with folders" option in phase 6 cleans up.
- If a compose file mounts the stack folder itself, like `- /opt/copyparty:/cfg`, the stack page says so. The container can read `.env` and edit the compose file. Most apps never touch them, so it's a heads-up, not a block.
- Fleetling's own stack can be edited but not deployed, stopped or restarted from inside. The process would die halfway through. Self-updates arrive in phase 7; until then, update it from the host.

## Containers, logs and the shell

Container actions go through the Engine API, the same calls `docker stop` and friends make. The confirm shows the equivalent command, like `docker rm -f gitea`, and that line goes in the action log. Remove is `rm -f`: it stops the container and deletes it, and never touches named volumes or anything on disk. Recreate on a container in a managed stack runs `docker compose ... up -d --force-recreate --no-deps <service>`, so it picks up compose file changes the way Compose would.

Logs keep their colors. Containers without a TTY send stdout and stderr interleaved with 8-byte frame headers; Fleetling splits those properly, and stderr lines show in red. The stack Logs tab runs `docker compose logs`, so every line carries its service name. Following stops the moment you leave the page.

The shell starts `/bin/bash` if the image has it and `/bin/sh` if not, in one exec. Set Command to run something else (it's split on spaces, not run through a shell) and User to run as someone other than the image default. Resizing the browser resizes the terminal. Distroless and scratch images have no shell at all, and the terminal says so instead of hanging. Each session goes in the action log as `docker exec -it ...` with its exit code. What you type is never logged.

Fleetling's own container can't be stopped, restarted, killed or removed from its own page. Logs, shell and inspect work.

## Networks, images and volumes

The network form matches Portainer's: name, driver, subnet, gateway, IP range, parent interface, ipvlan mode, internal, attachable, IPv6 with its own subnet, labels and driver options. It sends the same request `docker network create` does, and the confirm shows that exact command, like:

```
docker network create -d macvlan --subnet 192.168.1.0/24 --gateway 192.168.1.1 --ip-range 192.168.1.192/27 -o parent=eth0 lan
```

Why is there a container flashing up when the form opens? Fleetling's own container can't see the host's interfaces, so it fills the parent dropdown by running a throwaway copy of its own image with `--network host` and reading `/sys/class/net`. There's a free-text box next to it in case that fails or your interface is missing. Could be eth0, could be enp3s0, you'll have to check.

A network with containers attached can't be removed; the page lists them. Networks Compose made for a stack are marked with the stack's name, and removing one warns you that Compose recreates it on the next deploy. Docker's built-in bridge, host and none can't be removed at all.

Every prune shows the list of what will go before it runs. Image prune comes in two sizes: dangling only, or every image no container uses. Volume prune does unused anonymous volumes by default, like `docker volume prune` does since Docker 23; "Prune all unused" adds named volumes too. A named volume holds data, so read that list.

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

Skipping step 1 still builds, the pages just load without JavaScript.

Release binaries for linux amd64 and arm64 are attached to each `v*` tag by the build workflow.

### Dependencies

Fleetling always runs on the newest release of everything. The deps workflow runs every Monday: it updates every Go module and npm package, runs the tests, rebuilds the image with `--pull`, and opens a PR. The tradeoff is that a bad upstream release can break a build without warning. Tests gate every publish, so a broken release blocks the PR instead of reaching your server.

After the first push, run the deps workflow once by hand from the Actions tab. That commits `go.sum`. Until then, the Dockerfile and CI create it with `go mod tidy`, which checks every module against the public checksum database. The workflow needs "Allow GitHub Actions to create and approve pull requests" switched on under Settings, Actions, General.

## License

GPL-3.0. See `LICENSE`.

Now Fleetling is running on port 8420, listing every stack under your root next to everything the engines are running, and changing nothing until you tell it to.
