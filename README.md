# lidawake

Keep a MacBook awake with the lid closed — once. Reopening the lid re-enables normal sleep.

## How it works

- Root launchd daemon (`dev.lidawake.daemon`) owns all `pmset` calls.
- CLI talks to it over `/var/run/lidawake.sock` (mode 0600, owned by the installing user, so no `sudo` needed for start/stop/status).
- Session: `start` → `pmset -a disablesleep 1` → poll `AppleClamshellState` every 5 s → lid closed (recorded) → lid opened → `pmset -a disablesleep 0`.
- Safety deadline (`-max`, default 12h, cap 72h) ends the session even if the lid never reopens.
- Time-only mode (`-ignore-lid`) skips lid tracking: the session runs until the deadline, battery cutoff, or `stop`.
- Battery cutoff (`-min-battery`, default 20, `0` disables) ends the session when running on battery below that percent. `start` refuses if already below it.
- State persisted to `/var/db/lidawake/state.json` on every transition.

Crash / restart behaviour (daemon start):

| Persisted state | Action |
|---|---|
| no active session | `disablesleep 0` (clears stale flag) |
| active, different or unknown boot | end session (`reboot`), `disablesleep 0` |
| active, deadline passed | end session (`deadline`), `disablesleep 0` |
| active, same boot | resume, re-apply `disablesleep 1`, keep "lid was closed" flag |

`KeepAlive` restarts the daemon after a crash. Writes are ordered so a crash between "save state" and "pmset" always recovers safely.

## Install

Requires Go 1.24+.

With `go install`:

```sh
go install github.com/ame180/lidawake@latest
sudo "$(go env GOPATH)/bin/lidawake" install
```

Use the full path: `sudo` won't find `~/go/bin`. If you set `GOBIN`, use that directory instead. To upgrade, run both again.

From a clone:

```sh
make build
sudo ./dist/lidawake install
```

`install` puts binary in `/usr/local/bin/lidawake`, plist in `/Library/LaunchDaemons/`, bootstraps it.

## Use

```sh
lidawake start            # 12h safety deadline
lidawake start -max 3h
lidawake start -min-battery 40   # end below 40% on battery; 0 disables
lidawake start -ignore-lid -max 2h   # stay awake 2h, lid open or closed
lidawake status
lidawake stop
```

Log: `/var/log/lidawake.log`.

## Uninstall

```sh
sudo lidawake uninstall   # bootout, remove files, disablesleep 0
```

## Notes

- Daemon clears `disablesleep` on every start when no session is active. A manual `sudo pmset -a disablesleep 1` will be reset at next boot/daemon restart.
- Lid is polled every 5 s. Closing and reopening within one poll goes unnoticed; the session then runs until `stop` or the deadline.
- Closed lid traps heat. On battery, a session ends below the `-min-battery` cutoff (if `pmset -g batt` can't be read, only the deadline applies).
- `uninstall` removes `/usr/local/bin/lidawake` — run it from that path or from the build.
- Installed binary is root-owned, but `/usr/local/bin` may be user-writable (e.g. Intel Homebrew). Moving the daemon binary to a root-only path is planned.

## Development

```sh
make test     # go vet + go test
make lint     # golangci-lint (brew install golangci-lint)
make build    # dist/lidawake, version from git describe
```

## License

MIT, see [LICENSE](LICENSE).
