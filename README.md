# proxima

A local HTTPS gateway for remote services reachable only through a SOCKS5 proxy.

## The problem it solves

You have a remote machine. On that machine, services are running on `localhost` — ports like `7777`, `3000`, etc. Those services are not exposed to the internet. The only way to reach the remote machine is through a **SOCKS5 proxy** running on your local machine at `127.0.0.1:1080` (e.g. established by an SSH connection with `-D 1080`).

You want to open `https://myapp.dev.local` in your browser and have it just work — with HTTPS, no port numbers in the URL, no manual tunnels to set up each time.

`proxima` automates the entire chain.

---

## How it works

```
Browser
  │  https://myapp.dev.local
  ▼
/etc/hosts  →  resolves myapp.dev.local to 127.0.0.1
  │
  ▼
Caddy  (HTTPS termination, port 443)
  │  reverse_proxy → http://127.0.0.1:17777
  │  sets header: Host: localhost
  ▼
gost  (TCP tunnel, port 17777)
  │  tcp://127.0.0.1:17777/localhost:7777
  │  forwards through SOCKS5
  ▼
SOCKS5 proxy  (127.0.0.1:1080)
  │
  ▼  [network boundary]
  │
Remote machine
  │  connects to localhost:7777
  ▼
Remote service
```

### Step by step

**1. DNS — `/etc/hosts`**

Browsers need a hostname to resolve to an IP. `proxima` adds a block to `/etc/hosts`:

```
>>> proxima start
127.0.0.1 myapp.dev.local
127.0.0.1 api.dev.local
<<< proxima end
```

This makes `myapp.dev.local` point to your own machine (`127.0.0.1`), where Caddy is listening.

**2. HTTPS termination — Caddy**

Caddy listens on port 443 and handles TLS automatically (self-signed cert for `.dev.local`). It receives the browser's HTTPS request and forwards it as plain HTTP to a local port where gost is listening.

The generated `~/.proxima/Caddyfile` looks like:

```caddy
https://myapp.dev.local {
    reverse_proxy http://127.0.0.1:17777 {
        header_up Host localhost
    }
}
```

The `header_up Host localhost` line is important: it sets the HTTP `Host` header to `localhost` before forwarding. Without this, the remote service would receive `Host: myapp.dev.local`, which it doesn't know about and would reject.

**3. TCP tunnel — gost**

gost creates a raw TCP tunnel between a local port and a remote port, routed through the SOCKS5 proxy. The command it runs is:

```
gost -L=tcp://127.0.0.1:17777/localhost:7777 -F=socks5://127.0.0.1:1080
```

Breaking this down:
- `-L=tcp://127.0.0.1:17777/localhost:7777` — listen on local port `17777`; when a connection arrives, connect to `localhost:7777` on the other side of the proxy
- `-F=socks5://127.0.0.1:1080` — use the SOCKS5 proxy at `127.0.0.1:1080` to make that connection

Crucially, `localhost:7777` is resolved **by the SOCKS5 proxy on the remote machine**, not locally. So it connects to port `7777` on the remote machine's own loopback interface — exactly where the remote service is listening.

**4. Port numbering**

Local ports are assigned deterministically: `local = 10000 + remote`. So:

| Service | Remote port | Local tunnel port |
|---------|-------------|-------------------|
| myapp   | 7777        | 17777             |
| api     | 3000        | 13000             |

No port conflicts, no configuration needed.

**5. Caddy persistence — launchd**

Caddy is managed by macOS `launchd` via a plist at:

```
~/Library/LaunchAgents/com.proxima.caddy.plist
```

With `KeepAlive = true`, launchd restarts Caddy if it crashes and starts it automatically on login. On subsequent `./proxima` runs, Caddy is already running and `caddy reload` is used to pick up config changes without restarting.

---

## Prerequisites

- **gost v3** — `brew install gost` or download from [github.com/go-gost/gost](https://github.com/go-gost/gost)
- **Caddy** — `brew install caddy`
- **Go 1.21+** — only needed to build; the resulting binary has no runtime dependencies
- A **SOCKS5 proxy** running at `127.0.0.1:1080` (e.g. `ssh -D 1080 user@remote-host`)

---

## Installation

```bash
git clone <repo>
cd proxima
go build -o proxima .
```

---

## Configuration

Create `~/.proxima/config.json`:

```json
{
  "services": {
    "myapp": 7777,
    "api": 3000
  }
}
```

Each key is a service name. The value is the port the service listens on **on the remote machine's localhost**. That's it — no local ports, no hostnames, no other options.

---

## Usage

Make sure your SOCKS5 proxy is running first, then:

```bash
./proxima
```

On first run it will ask for your password once (to write `/etc/hosts` via `sudo`). After that, open your browser:

```
https://myapp.dev.local
https://api.dev.local
```

Your browser will warn about the self-signed certificate on first visit. Accept it (or add Caddy's root CA to your keychain with `caddy trust`).

### Re-running

Run `./proxima` again any time you:
- Add or remove a service from the config
- Restart your machine (gost processes don't survive reboots; Caddy does via launchd)
- The SOCKS5 proxy reconnects after a drop

---

## File layout

```
~/.proxima/
├── config.json       # your service definitions
├── Caddyfile         # generated on each run
├── caddy.log         # Caddy stdout
└── caddy.err         # Caddy stderr

~/Library/LaunchAgents/
└── com.proxima.caddy.plist   # keeps Caddy running
```

---

## Troubleshooting

**Browser shows "connection refused"**
The gost tunnel isn't running. Run `./proxima` again.

**Browser shows HTTP 400**
The remote service is rejecting the request. Check that the port in `config.json` is correct and the service is actually running on the remote machine.

**Browser shows SSL error / certificate not trusted**
Run `caddy trust` once to add Caddy's local CA to your macOS keychain. You only need to do this once.

**`sudo` password prompt on every run**
This is expected — writing `/etc/hosts` requires root. You can add a sudoers rule to skip the password for this specific command:
```
your_username ALL=(ALL) NOPASSWD: /bin/cp /tmp/hosts.proxima /etc/hosts
```

**Caddy won't start**
Check `~/.proxima/caddy.err` for errors. Port 443 may be in use by another process (`sudo lsof -i :443`).

**gost exits immediately**
The SOCKS5 proxy at `127.0.0.1:1080` is not reachable. Make sure your SSH tunnel or proxy is running before starting proxima.
