# Fleetling

Fleetling is a single-user web app for managing Docker and Podman compose stacks on one homelab server, with every stack's compose file kept in its own folder under `/opt`.

It replaces the parts of Portainer CE that actually get used: stacks, containers, networks, images, volumes and builds. No Swarm, no Kubernetes runtime, no remote agents.

## Status

This is phase 1 of 11. What works right now:

- First-run setup: admin password and stack root.
- Login, sessions, "log out everywhere", password change.
- Settings: stack root, ignored folders, Docker and Podman endpoints with a Test button.
- A read-only stack list. Every folder under the root with a compose file shows up, along with every running Compose project the engines report.

It changes nothing on your server yet. Deploy, stop, edit and the rest arrive in phase 2. The full plan is in the build spec.

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

The importer arrives in phase 4. It reads each stack's compose text and env vars through Portainer's API, writes them into `/opt/<folder>`, and picks up the running containers by project name with zero restarts.

Until then, your Portainer stacks show up in Fleetling as External, and nothing about them changes. Do not remove stacks inside Portainer to "clean up". That runs `down` and deletes their containers.

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

## How stacks are listed

Fleetling scans the direct children of the root for `compose.yaml`, `compose.yml`, `docker-compose.yml` or `docker-compose.yaml`, in the order Compose itself picks them. Hidden folders, symlinks and names on the ignore list (`containerd` by default) are skipped.

| Status | Meaning |
| --- | --- |
| Running, Partial, Stopped | A managed stack with a `.fleetling.toml`, and how many of its services are up |
| On disk | A folder with a compose file and no `.fleetling.toml` yet. "Manage" arrives in phase 2 |
| External | A running Compose project with no folder under the root, Portainer's stacks for example |
| Unknown | The stack's engine didn't answer |

A folder is matched to running containers by the `com.docker.compose.project.working_dir` label first, then by project name. Its project name comes from `.fleetling.toml`, then `COMPOSE_PROJECT_NAME` in `.env`, then a top-level `name:`, then the folder name, same as Compose.

One thing to know now: if a folder has a `compose.override.yaml`, Fleetling flags it. Fleetling runs Compose with `-f compose.yaml`, which means the override file is not applied.

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
