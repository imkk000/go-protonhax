# protonhax

Run commands inside a running game's Proton context — same Wine prefix, same environment, same Proton build.

## How it works

Steam is told to wrap every game launch through `protonhax init`. On startup, `init` saves three files to a per-app directory under `$XDG_RUNTIME_DIR/protonhax/<appid>/`:

| File  | Contents |
|-------|----------|
| `exe` | Path to the Proton executable |
| `pfx` | Path to the Wine prefix (`STEAM_COMPAT_DATA_PATH/pfx`) |
| `env` | Full environment snapshot at game launch time |

After saving those files it `exec`s the real game command, so there is no wrapper process sitting in the background.

Any time after launch you can use `run`, `cmd`, or `exec` to inject another process into that same context.

## Installation

```sh
go install protonhax@latest
```

Or build from source:

```sh
git clone ...
cd protonhax
go build -o ~/.local/bin/protonhax .
```

## Steam setup

In the game's **Properties → Launch Options**, set:

```
protonhax init %COMMAND%
```

That's it. Every time Steam launches the game, `protonhax` captures its Proton context before handing off to the game.

## Commands

```
protonhax init <cmd>
```
Called by Steam. Captures the Proton exe, prefix, and environment for `$SteamAppId`, then execs the game command.

---

```
protonhax ls
```
Lists app IDs of all games currently tracked (i.e. launched via `init` and still running).

---

```
protonhax flush
```
Removes all stale entries from the runtime directory. Useful if a game crashed or was killed and its entry was left behind.

---

```
protonhax run <appid> <windows-exe> [args...]
```
Runs a Windows executable through Proton inside the game's context.

```sh
# Launch a modding tool while the game is running
protonhax run 123456 'Z:\home\user\tools\modtool.exe'
```

---

```
protonhax cmd <appid>
```
Opens `cmd.exe` inside the game's Wine prefix. Handy for poking around the Windows environment.

---

```
protonhax exec <appid> <linux-cmd> [args...]
```
Runs a native Linux executable with the game's environment variables active. Useful for debugging tools that need the game's `STEAM_*`, `PROTON_*`, `WINE*` vars, etc.

```sh
protonhax exec 123456 env | grep STEAM
protonhax exec 123456 wine --version
```

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Error (message printed to stderr) |
| 2 | App ID not found (not launched via `init`, or already exited) |

## Fish tab completion

Copy the completion file to your Fish completions directory:

```sh
cp protonhax.fish ~/.config/fish/completions/protonhax.fish
```

This gives you:
- Subcommand completion for all commands
- App ID completion (from `protonhax ls`) for `run`, `cmd`, and `exec`

## Runtime state

State is stored in `$XDG_RUNTIME_DIR/protonhax/` (falls back to `/run/user/<uid>/protonhax/`). This directory lives on a tmpfs and is wiped on logout, so stale entries don't survive reboots.
