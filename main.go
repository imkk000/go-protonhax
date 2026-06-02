// Package main implements the protonhax CLI for running commands in Proton game contexts.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
)

func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "protonhax")
	}
	return fmt.Sprintf("/run/user/%d/protonhax", os.Getuid())
}

func appDir(phd, appid string) string {
	return filepath.Join(phd, appid)
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // intentional: reads app-specific state files
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\n"), nil
}

func loadEnv(envFile string) ([]string, error) {
	b, err := os.ReadFile(envFile) //nolint:gosec // intentional: reads app-specific state files
	if err != nil {
		return nil, err
	}
	var env []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		env = append(env, line)
	}
	return stripLD32(env), nil
}

// stripLD32 removes 32-bit Steam overlay entries from LD_PRELOAD to suppress
// "wrong ELF class: ELFCLASS32" noise when running 64-bit processes.
func stripLD32(env []string) []string {
	for i, line := range env {
		if !strings.HasPrefix(line, "LD_PRELOAD=") {
			continue
		}
		val := strings.TrimPrefix(line, "LD_PRELOAD=")
		var kept []string
		for _, p := range strings.Split(val, ":") {
			if !strings.Contains(p, "ubuntu12_32") {
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 {
			return append(env[:i], env[i+1:]...)
		}
		env[i] = "LD_PRELOAD=" + strings.Join(kept, ":")
		return env
	}
	return env
}

// withDebugEnv injects Proton/Wine debug vars that aren't already set.
func withDebugEnv(env []string) []string {
	for _, kv := range []string{"PROTON_LOG=1", "WINEDEBUG=+err,+warn"} {
		key := kv[:strings.IndexByte(kv, '=')]
		found := false
		for _, e := range env {
			if strings.HasPrefix(e, key+"=") {
				found = true
				break
			}
		}
		if !found {
			env = append(env, kv)
		}
	}
	return env
}

func requireApp(phd, appid string) (string, bool) {
	dir := appDir(phd, appid)
	if _, err := os.Stat(dir); err != nil {
		fmt.Fprintf(os.Stderr, "No app running with appid %q\n", appid)
		return "", false
	}
	return dir, true
}

// execReplace replaces the current process via execve. Use for native Linux commands (exec subcommand).
func execReplace(argv []string, env []string) error {
	path, err := exec.LookPath(argv[0]) //nolint:gosec // intentional: tool purpose is to exec arbitrary commands
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	if err := syscall.Exec(path, argv, env); err != nil { //nolint:gosec
		return fmt.Errorf("exec: %w", err)
	}
	return nil // unreachable
}

// spawnWithEnv starts a command and returns immediately without waiting.
// Used for Proton-based subcommands (run/cmd) where Proton blocks on wineserver
// until the game itself exits — waiting here would require Ctrl+C.
func spawnWithEnv(argv []string, env []string) error {
	c := exec.Command(argv[0], argv[1:]...) //nolint:gosec
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = env
	if err := c.Start(); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}
	go c.Wait() //nolint:errcheck // reap child in background; return is intentionally ignored
	return nil
}

// steamAppsDir returns ~/.steam/steam/steamapps.
func steamAppsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".steam/steam/steamapps")
}

// setEnv replaces or appends a KEY=val entry in an env slice.
func setEnv(env []string, key, val string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}

// resolveProtonWine returns the wine binary for the given appid.
// It reads compatdata/<appid>/config_info looking for an embedded Proton dir name,
// then falls back to the newest Proton install under steamapps/common/ by mod time.
func resolveProtonWine(appid, steamApps string) (string, error) {
	configInfo := filepath.Join(steamApps, "compatdata", appid, "config_info")
	if data, err := os.ReadFile(configInfo); err == nil { //nolint:gosec
		// config_info may be binary; tokenise on common separators and look for Proton paths.
		for _, tok := range strings.FieldsFunc(string(data), func(r rune) bool {
			return r == '\n' || r == '\r' || r == '\x00'
		}) {
			tok = strings.TrimSpace(tok)
			if !strings.Contains(tok, "Proton") {
				continue
			}
			for _, candidate := range []string{
				filepath.Join(tok, "files/bin/wine"),
				filepath.Join(steamApps, "common", tok, "files/bin/wine"),
			} {
				if _, err := os.Stat(candidate); err == nil {
					return candidate, nil
				}
			}
		}
	}
	// Fallback: scan installed Proton versions and pick the newest by dir mod time.
	matches, _ := filepath.Glob(filepath.Join(steamApps, "common/Proton */files/bin/wine"))
	if len(matches) == 0 {
		return "", fmt.Errorf("no Proton installation found under %s", filepath.Join(steamApps, "common"))
	}
	newest := matches[0]
	var newestMod time.Time
	for _, m := range matches {
		protonDir := filepath.Dir(filepath.Dir(filepath.Dir(m)))
		if info, err := os.Stat(protonDir); err == nil && info.ModTime().After(newestMod) {
			newestMod = info.ModTime()
			newest = m
		}
	}
	return newest, nil
}

// runBlocking runs argv with the given env, inheriting stdin/stdout/stderr,
// waits for the child to finish, and propagates its exit code via os.Exit.
func runBlocking(argv []string, env []string) error {
	c := exec.Command(argv[0], argv[1:]...) //nolint:gosec
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = env
	if err := c.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}

// completeAppIDs prints running app IDs for the first positional argument.
func completeAppIDs(_ context.Context, cmd *cli.Command) {
	if cmd.NArg() > 1 {
		return
	}
	entries, err := os.ReadDir(runtimeDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			fmt.Fprintln(os.Stdout, e.Name()) //nolint:errcheck // writing to stdout in completion context
		}
	}
}

func main() {
	phd := runtimeDir()
	var debug bool

	app := &cli.Command{
		Name:                  "protonhax",
		Usage:                 "Run commands in Proton game contexts",
		EnableShellCompletion: true,
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:        "debug",
				Aliases:     []string{"d"},
				Usage:       "inject Proton/Wine debug env vars (PROTON_LOG=1, WINEDEBUG=+err,+warn)",
				Destination: &debug,
			},
		},
		Commands: []*cli.Command{
			{
				Name:            "init",
				Usage:           "Initialize a game context (called by Steam with %COMMAND%)",
				ArgsUsage:       "<cmd>",
				SkipFlagParsing: true,
				Action: func(_ context.Context, cmd *cli.Command) error {
					appid := os.Getenv("SteamAppId")
					if appid == "" {
						return errors.New("SteamAppId not set")
					}
					args := cmd.Args().Slice()
					// Strip leading "--" that Steam inserts between its own args and %COMMAND%
					for len(args) > 0 && args[0] == "--" {
						args = args[1:]
					}
					if len(args) == 0 {
						return errors.New("no command given")
					}
					var protonExe string
					for _, a := range args {
						if strings.Contains(a, "/proton") {
							protonExe = a
							break
						}
					}
					if protonExe == "" {
						return errors.New("proton executable not found in args")
					}
					dir := appDir(phd, appid)
					if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // 0750: owner+group only
						return err
					}
					defer os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup on all exit paths
					if err := os.WriteFile(filepath.Join(dir, "exe"), []byte(protonExe), 0o600); err != nil {
						return err
					}
					pfx := os.Getenv("STEAM_COMPAT_DATA_PATH") + "/pfx"
					if err := os.WriteFile(filepath.Join(dir, "pfx"), []byte(pfx), 0o600); err != nil { //nolint:gosec // dir is derived from SteamAppId, not arbitrary user input
						return err
					}
					if err := os.WriteFile(filepath.Join(dir, "env"), []byte(strings.Join(os.Environ(), "\n")), 0o600); err != nil {
						return err
					}
					c := exec.Command(args[0], args[1:]...) //nolint:gosec // intentional: tool purpose is to exec arbitrary commands
					c.Stdin = os.Stdin
					c.Stdout = os.Stdout
					c.Stderr = os.Stderr
					c.Env = os.Environ()
					return c.Run()
				},
			},
			{
				Name:  "flush",
				Usage: "Remove all stale app entries from the runtime directory",
				Action: func(_ context.Context, _ *cli.Command) error {
					entries, err := os.ReadDir(phd)
					if os.IsNotExist(err) {
						return nil
					}
					if err != nil {
						return err
					}
					for _, e := range entries {
						if err := os.RemoveAll(filepath.Join(phd, e.Name())); err != nil {
							fmt.Fprintln(os.Stderr, "flush:", err)
						}
					}
					return nil
				},
			},
			{
				Name:  "ls",
				Usage: "List all currently running games",
				Action: func(_ context.Context, _ *cli.Command) error {
					entries, err := os.ReadDir(phd)
					if os.IsNotExist(err) {
						return nil
					}
					if err != nil {
						return err
					}
					for _, e := range entries {
						if _, err := fmt.Fprintln(os.Stdout, e.Name()); err != nil {
							return err
						}
					}
					return nil
				},
			},
			{
				Name:          "run", //nolint:goconst // "run" is also a Proton subcommand argument, not the same semantic constant
				Usage:         "Run a Windows executable via Proton in the context of an app",
				ArgsUsage:     "<appid> <cmd> [args...]",
				ShellComplete: completeAppIDs,
				Action: func(_ context.Context, cmd *cli.Command) error {
					args := cmd.Args().Slice()
					if len(args) < 2 {
						return errors.New("usage: protonhax run <appid> <cmd> [args...]")
					}
					dir, ok := requireApp(phd, args[0])
					if !ok {
						os.Exit(2)
					}
					env, err := loadEnv(filepath.Join(dir, "env"))
					if err != nil {
						return fmt.Errorf("load env: %w", err)
					}
					if debug {
						env = withDebugEnv(env)
					}
					protonExe, err := readFile(filepath.Join(dir, "exe"))
					if err != nil {
						return fmt.Errorf("read exe: %w", err)
					}
					return spawnWithEnv(append([]string{protonExe, "run"}, args[1:]...), env)
				},
			},
			{
				Name:          "cmd",
				Usage:         "Run cmd.exe in the context of an app",
				ArgsUsage:     "<appid>",
				ShellComplete: completeAppIDs,
				Action: func(_ context.Context, cmd *cli.Command) error {
					args := cmd.Args().Slice()
					if len(args) < 1 {
						return errors.New("usage: protonhax cmd <appid>")
					}
					dir, ok := requireApp(phd, args[0])
					if !ok {
						os.Exit(2)
					}
					env, err := loadEnv(filepath.Join(dir, "env"))
					if err != nil {
						return fmt.Errorf("load env: %w", err)
					}
					if debug {
						env = withDebugEnv(env)
					}
					protonExe, err := readFile(filepath.Join(dir, "exe"))
					if err != nil {
						return fmt.Errorf("read exe: %w", err)
					}
					pfx, err := readFile(filepath.Join(dir, "pfx"))
					if err != nil {
						return fmt.Errorf("read pfx: %w", err)
					}
					return spawnWithEnv([]string{protonExe, "run", pfx + "/drive_c/windows/system32/cmd.exe"}, env)
				},
			},
			{
				Name:          "exec",
				Usage:         "Run a Linux executable in the game's environment",
				ArgsUsage:     "<appid> <cmd> [args...]",
				ShellComplete: completeAppIDs,
				Action: func(_ context.Context, cmd *cli.Command) error {
					args := cmd.Args().Slice()
					if len(args) < 2 {
						return errors.New("usage: protonhax exec <appid> <cmd> [args...]")
					}
					dir, ok := requireApp(phd, args[0])
					if !ok {
						os.Exit(2)
					}
					env, err := loadEnv(filepath.Join(dir, "env"))
					if err != nil {
						return fmt.Errorf("load env: %w", err)
					}
					if debug {
						env = withDebugEnv(env)
					}
					return execReplace(args[1:], env)
				},
			},
			{
				Name:      "build",
				Usage:     "Run a Windows executable via Proton without the game running",
				ArgsUsage: "<appid> <cmd> [args...]",
				Action: func(_ context.Context, cmd *cli.Command) error {
					args := cmd.Args().Slice()
					if len(args) < 2 {
						return errors.New("usage: protonhax build <appid> <cmd> [args...]")
					}
					appid := args[0]
					steamApps := steamAppsDir()

					pfx := filepath.Join(steamApps, "compatdata", appid, "pfx")
					if _, err := os.Stat(pfx); err != nil {
						return fmt.Errorf("no Wine prefix for appid %q (expected %s)", appid, pfx)
					}

					wine, err := resolveProtonWine(appid, steamApps)
					if err != nil {
						return err
					}

					env := setEnv(os.Environ(), "WINEPREFIX", pfx)
					env = setEnv(env, "WINEFSYNC", "1")

					return runBlocking(append([]string{wine}, args[1:]...), env)
				},
			},
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
