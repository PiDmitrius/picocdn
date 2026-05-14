package main

import (
	"flag"
	"fmt"

	"github.com/PiDmitrius/picocdn/internal/auth"
)

func runInit(args []string) error {
	var name string
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	fs.StringVar(&name, "name", "init", "name for the bootstrap root token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: picocdn init [--name name]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.RootTokens) > 0 {
		return fmt.Errorf("config already contains %d root token(s); use 'picocdn root create' instead", len(cfg.RootTokens))
	}
	plain, rt, err := auth.NewRootToken(name)
	if err != nil {
		return err
	}
	cfg.RootTokens = append(cfg.RootTokens, rt)
	if err := saveConfig(cfg); err != nil {
		return err
	}
	return printJSON(auth.CreatedRootToken{
		TokenID: rt.ID,
		Token:   plain,
		Name:    rt.Name,
	})
}

func runRoot(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing root subcommand")
	}
	switch args[0] {
	case "create":
		return runRootCreate(args[1:])
	case "list":
		return runRootList(args[1:])
	case "revoke":
		return runRootRevoke(args[1:])
	default:
		return fmt.Errorf("unknown root subcommand %q", args[0])
	}
}

func runRootCreate(args []string) error {
	var name string
	fs := flag.NewFlagSet("root create", flag.ExitOnError)
	fs.StringVar(&name, "name", "", "root token name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if name == "" || fs.NArg() != 0 {
		return fmt.Errorf("usage: picocdn root create --name <name>")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	plain, rt, err := auth.NewRootToken(name)
	if err != nil {
		return err
	}
	cfg.RootTokens = append(cfg.RootTokens, rt)
	if err := saveConfig(cfg); err != nil {
		return err
	}
	return printJSON(auth.CreatedRootToken{
		TokenID: rt.ID,
		Token:   plain,
		Name:    rt.Name,
	})
}

func runRootList(args []string) error {
	fs := flag.NewFlagSet("root list", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: picocdn root list")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	out := make([]auth.RootTokenInfo, 0, len(cfg.RootTokens))
	for _, rt := range cfg.RootTokens {
		out = append(out, auth.RootTokenInfo{
			ID:        rt.ID,
			Name:      rt.Name,
			CreatedAt: rt.CreatedAt,
		})
	}
	return printJSON(out)
}

func runRootRevoke(args []string) error {
	fs := flag.NewFlagSet("root revoke", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: picocdn root revoke <id>")
	}
	id := fs.Arg(0)
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.RootTokens) <= 1 {
		return fmt.Errorf("refusing to revoke the last root token; create a new one first")
	}
	idx := -1
	for i, rt := range cfg.RootTokens {
		if rt.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("root token %q not found", id)
	}
	cfg.RootTokens = append(cfg.RootTokens[:idx], cfg.RootTokens[idx+1:]...)
	if err := saveConfig(cfg); err != nil {
		return err
	}
	return printJSON(map[string]string{
		"status":   "revoked",
		"token_id": id,
	})
}
