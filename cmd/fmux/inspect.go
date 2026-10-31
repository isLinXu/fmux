package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/linxu/fmux/internal/adapter"
	"github.com/linxu/fmux/internal/build"
	"github.com/linxu/fmux/internal/config"
	"github.com/linxu/fmux/internal/router"
	"github.com/spf13/cobra"
)

// loadConfig resolves --config, falling back to the default path.
func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		path = config.DefaultPath()
	}
	return config.Load(path)
}

func newBackendsCmd() *cobra.Command {
	var cfgPath string
	c := &cobra.Command{
		Use:   "backends",
		Short: "List configured backends and their health",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return err
			}
			reg, err := build.Registry(cfg)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tTYPE\tKIND\tHEALTH\tCAPS\tURL")
			for _, ad := range reg.All() {
				health := "ok"
				if err := ad.Health(ctx); err != nil {
					health = "down"
				}
				caps := ad.Caps()
				flags := make([]string, 0, 5)
				if caps.Stream {
					flags = append(flags, "stream")
				}
				if caps.Vision {
					flags = append(flags, "vision")
				}
				if caps.JSONSchema {
					flags = append(flags, "schema")
				}
				if caps.Tools {
					flags = append(flags, "tools")
				}
				url := ""
				if b, ok := cfg.BackendByName(ad.Name()); ok {
					url = b.URL
				}
				kind := config.KindOf(ad.Type())
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					ad.Name(), ad.Type(), kind, health, strings.Join(flags, ","), url)
			}
			return w.Flush()
		},
	}
	c.Flags().StringVar(&cfgPath, "config", "", "config file (default ~/.fmux/config.yaml)")
	return c
}

func newModelsCmd() *cobra.Command {
	var (
		cfgPath string
		all     bool
	)
	c := &cobra.Command{
		Use:   "models",
		Short: "List model aliases (and optionally every backend model)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return err
			}
			reg, err := build.Registry(cfg)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ALIAS\tBACKEND\tMODEL\tPROFILE")

			names := make([]string, 0, len(cfg.Aliases))
			for n := range cfg.Aliases {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				a := cfg.Aliases[n]
				backend, model := a.Backend, a.Model
				if backend == "" {
					backend = "(fallback: " + strings.Join(a.Fallback, ", ") + ")"
				}
				if model == "" {
					model = n
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n, backend, model, a.Profile)
			}

			if all {
				fmt.Fprintln(w, "\t\t\t")
				fmt.Fprintln(w, "BACKEND MODEL\tBACKEND\t\t")
				for _, ad := range reg.All() {
					models, err := ad.Models(ctx)
					if err != nil {
						fmt.Fprintf(w, "(%s unreachable)\t%s\t\t\n", ad.Name(), ad.Name())
						continue
					}
					for _, m := range models {
						fmt.Fprintf(w, "%s\t%s\t\t\n", m.ID, ad.Name())
					}
				}
			}
			return w.Flush()
		},
	}
	c.Flags().StringVar(&cfgPath, "config", "", "config file (default ~/.fmux/config.yaml)")
	c.Flags().BoolVar(&all, "all", false, "also list every model each backend reports")
	return c
}

// doctorCheck is one line of the diagnostic report.
type doctorCheck struct {
	name   string
	status string // ok | warn | fail
	detail string
}

func newDoctorCmd() *cobra.Command {
	var cfgPath string
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose configuration, backends and fm integration",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return err
			}
			checks := runDoctor(cmd.Context(), cfg)

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			failures := 0
			for _, chk := range checks {
				if chk.status == "fail" {
					failures++
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", strings.ToUpper(chk.status), chk.name, chk.detail)
			}
			_ = w.Flush()

			if failures > 0 {
				return fmt.Errorf("%d check(s) failed", failures)
			}
			return nil
		},
	}
	c.Flags().StringVar(&cfgPath, "config", "", "config file (default ~/.fmux/config.yaml)")
	return c
}

func runDoctor(ctx context.Context, cfg *config.Config) []doctorCheck {
	var out []doctorCheck
	add := func(name, status, detail string) {
		out = append(out, doctorCheck{name: name, status: status, detail: detail})
	}

	// 1. config file
	if _, err := os.Stat(cfg.SourcePath); err == nil {
		add("config", "ok", cfg.SourcePath)
	} else {
		add("config", "warn", cfg.SourcePath+" not found, using defaults")
	}

	// 2. backends
	reg, err := build.Registry(cfg)
	if err != nil {
		add("backends", "fail", err.Error())
		return out
	}
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, ad := range reg.All() {
		if err := ad.Health(hctx); err != nil {
			add("backend:"+ad.Name(), "fail", ad.Type()+" unreachable: "+err.Error())
		} else {
			add("backend:"+ad.Name(), "ok", ad.Type()+" reachable")
		}
	}

	// 3. aliases resolve
	bad := 0
	for name := range cfg.Aliases {
		if _, err := resolveAlias(cfg, reg, name); err != nil {
			bad++
		}
	}
	if bad > 0 {
		add("aliases", "fail", fmt.Sprintf("%d alias(es) do not resolve", bad))
	} else {
		add("aliases", "ok", fmt.Sprintf("%d alias(es) resolve", len(cfg.Aliases)))
	}

	// 4. listen address
	if ln, err := net.Listen("tcp", cfg.Listen); err != nil {
		add("listen", "warn", cfg.Listen+" is already in use (gateway may be running)")
	} else {
		ln.Close()
		add("listen", "ok", cfg.Listen+" is free")
	}

	// 5. fm integration
	fmPath, err := exec.LookPath("fm")
	if err != nil {
		add("fm", "warn", "fm CLI not found; the gateway still serves curl and SDKs")
	} else {
		add("fm", "ok", fmPath)
		if detail, status := checkFmProvider(cfg.Listen); status != "" {
			add("fm-provider", status, detail)
		}
	}

	return out
}

// checkFmProvider inspects ~/.fm/config.json for a provider pointing at us.
func checkFmProvider(listen string) (string, string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ""
	}
	path := filepath.Join(home, ".fm", "config.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return path + " not found; run: fm config add model-provider fmux http://" + listen + "/v1", "warn"
	}
	var cfg struct {
		DefaultModel         string `json:"defaultModel"`
		DefaultModelProvider string `json:"defaultModelProvider"`
		ModelProviders       []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"modelProviders"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "could not parse " + path, "warn"
	}

	port := listen
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		port = listen[i+1:]
	}
	for _, p := range cfg.ModelProviders {
		if strings.Contains(p.URL, ":"+port) {
			if cfg.DefaultModelProvider == p.Name {
				return "fm default provider is " + p.Name + " (" + p.URL + "), model " + cfg.DefaultModel, "ok"
			}
			return "provider " + p.Name + " exists but is not the default", "warn"
		}
	}
	return "no fm provider points at port " + port + "; run: fm config add model-provider fmux http://" + listen + "/v1", "warn"
}

// resolveAlias is a thin wrapper so doctor can validate without exporting more.
func resolveAlias(cfg *config.Config, reg *adapter.Registry, name string) (string, error) {
	p, err := router.New(cfg, reg).Resolve(name)
	if err != nil {
		return "", err
	}
	if len(p.Targets) == 0 {
		return "", fmt.Errorf("no targets")
	}
	return p.Targets[0].Backend, nil
}
