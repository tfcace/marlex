package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tfcace/marlex/internal/rex"
)

func cmdRex(e env, args []string) error {
	if len(args) == 0 {
		return rexUsage()
	}
	switch args[0] {
	case "lua":
		fmt.Print(rex.Module)
		return nil
	case "install":
		return rexInstall(args[1:])
	}
	return rexUsage()
}

func rexUsage() error {
	fmt.Fprint(os.Stderr, `usage: mx rex install [--key KEY | --no-key] [--dir DIR]
       mx rex lua

install puts mx.lua next to Rex's init.lua and loads it from there.
A key (cmd+shift+b unless --key) then blames what's on screen: mx reads
the focused terminal, finds the file:line nearest the cursor, and opens its
history in a block to the right. The action is also in the command palette
as "mx: why is this here?". lua prints the module.
`)
	return exitError(2)
}

func rexInstall(args []string) error {
	fs := newFlags("rex install", "[--key KEY | --no-key] [--dir DIR]")
	key := fs.String("key", "cmd+shift+b", "the key that blames what's on screen")
	noKey := fs.Bool("no-key", false, "bind no key; use the command palette")
	dir := fs.String("dir", "", "Rex's configuration directory (default: where Rex reads init.lua)")
	if len(parse(fs, args)) != 0 {
		return usageErr(fs)
	}
	if *dir == "" {
		*dir = rexConfigDir()
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	if b, err := filepath.EvalSymlinks(bin); err == nil {
		bin = b
	}
	module := strings.Replace(rex.Module, `bin = "mx",`, "bin = "+luaString(bin)+",", 1)
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "mx.lua"), []byte(module), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (runs %s)\n", filepath.Join(*dir, "mx.lua"), bin)

	initPath := filepath.Join(*dir, "init.lua")
	if p := os.Getenv("REX_CONFIG"); p != "" {
		initPath = p
	}
	cur, err := os.ReadFile(initPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.Contains(string(cur), `require("mx")`) {
		fmt.Printf("%s already loads it\n", initPath)
	} else {
		opts := "key = " + luaString(*key)
		if *noKey {
			opts = "key = false"
		}
		add := fmt.Sprintf("-- mx: blame anything on screen (added by mx rex install)\nrequire(\"mx\").setup{ %s }\n", opts)
		if len(cur) > 0 && !strings.HasSuffix(string(cur), "\n") {
			add = "\n" + add
		}
		if len(cur) > 0 {
			add = "\n" + add
		}
		f, err := os.OpenFile(initPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		if _, err := f.WriteString(add); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Printf("added require(\"mx\") to %s\n", initPath)
	}

	rexBin, err := exec.LookPath("rex")
	if err != nil {
		fmt.Println("reload Rex's configuration to start using it")
		return nil
	}
	if out, err := exec.Command(rexBin, "config", "reload").CombinedOutput(); err != nil {
		return fmt.Errorf("rex config reload: %v: %s", err, strings.TrimSpace(string(out)))
	}
	how := "press " + *key + " in any terminal"
	if *noKey {
		how = `run "mx: why is this here?" from the command palette`
	}
	fmt.Printf("reloaded Rex's configuration; %s\n", how)
	return nil
}

// rexConfigDir is where Rex reads init.lua.
func rexConfigDir() string {
	if p := os.Getenv("REX_CONFIG"); p != "" {
		return filepath.Dir(p)
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "rex")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "rex")
}

// luaString quotes s as a Lua string literal.
func luaString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}
