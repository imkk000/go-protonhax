// Package main implements the protonhax CLI for running commands in Proton game contexts.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
	return env, nil
}

func requireApp(phd, appid string) (string, bool) {
	dir := appDir(phd, appid)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "No app running with appid %q\n", appid)
		return "", false
	}
	return dir, true
}

func execWithEnv(argv []string, env []string) error {
	if err := syscall.Exec(argv[0], argv, env); err != nil { //nolint:gosec // intentional: tool purpose is to exec arbitrary commands
		return fmt.Errorf("exec: %w", err)
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

	app := &cli.Command{
		Name:                  "protonhax",
		Usage:                 "Run commands in Proton game contexts",
		EnableShellCompletion: true,
		Commands: []*cli.Command{
			{
				Name:      "init",
				Usage:     "Initialize a game context (called by Steam with %COMMAND%)",
				ArgsUsage: "<cmd>",
				Action: func(_ context.Context, cmd *cli.Command) error {
					appid := os.Getenv("SteamAppId")
					if appid == "" {
						return errors.New("SteamAppId not set")
					}
					dir := appDir(phd, appid)
					if err := os.MkdirAll(dir, 0750); err != nil { //nolint:gosec // 0750: owner+group only
						return err
					}
					args := cmd.Args().Slice()
					var protonExe string
					for _, a := range args {
						if strings.Contains(a, "/proton") {
							protonExe = a
							break
						}
					}
					if err := os.WriteFile(filepath.Join(dir, "exe"), []byte(protonExe), 0600); err != nil {
						return err
					}
					pfx := os.Getenv("STEAM_COMPAT_DATA_PATH") + "/pfx"
					if err := os.WriteFile(filepath.Join(dir, "pfx"), []byte(pfx), 0600); err != nil { //nolint:gosec // dir is derived from SteamAppId, not arbitrary user input
						return err
					}
					if err := os.WriteFile(filepath.Join(dir, "env"), []byte(strings.Join(os.Environ(), "\n")), 0600); err != nil {
						return err
					}
					if len(args) == 0 {
						return errors.New("no command given")
					}
					if err := syscall.Exec(args[0], args, os.Environ()); err != nil { //nolint:gosec // intentional
						return fmt.Errorf("exec: %w", err)
					}
					// unreachable after Exec; cleanup is handled by the caller process
					return nil
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
					protonExe, err := readFile(filepath.Join(dir, "exe"))
					if err != nil {
						return fmt.Errorf("read exe: %w", err)
					}
					return execWithEnv(append([]string{protonExe, "run"}, args[1:]...), env)
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
					protonExe, err := readFile(filepath.Join(dir, "exe"))
					if err != nil {
						return fmt.Errorf("read exe: %w", err)
					}
					pfx, err := readFile(filepath.Join(dir, "pfx"))
					if err != nil {
						return fmt.Errorf("read pfx: %w", err)
					}
					return execWithEnv([]string{protonExe, "run", pfx + "/drive_c/windows/system32/cmd.exe"}, env)
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
					return execWithEnv(args[1:], env)
				},
			},
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
