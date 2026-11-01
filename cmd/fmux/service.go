package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/linxu/fmux/internal/config"
	"github.com/linxu/fmux/internal/launchd"
	"github.com/spf13/cobra"
)

func newServiceCmd() *cobra.Command {
	var (
		label string
		bin   string
		logp  string
		cfg   string
		keep  bool
	)

	svc := &cobra.Command{
		Use:   "service",
		Short: "Run the gateway as a login-time LaunchAgent",
		Long: "Install, inspect and remove a per-user LaunchAgent.\n\n" +
			"launchd is the only way to keep the gateway alive across logins. A shell\n" +
			"background job dies with its parent, and the gateway is supposed to\n" +
			"outlive the terminal that started it.",
	}

	install := &cobra.Command{
		Use:   "install",
		Short: "Write the plist and hand it to launchd",
		RunE: func(cmd *cobra.Command, args []string) error {
			if bin == "" {
				exe, err := os.Executable()
				if err != nil {
					return err
				}
				bin = exe
			}
			bin, err := filepath.Abs(bin)
			if err != nil {
				return err
			}
			if st, err := os.Stat(bin); err != nil || st.Mode()&0o111 == 0 {
				return fmt.Errorf("%s is not an executable file", bin)
			}
			// An empty --config would become an empty argv entry in the plist,
			// which launchd passes through and cobra rejects at startup.
			if cfg == "" {
				cfg = config.DefaultPath()
			}
			// Validate before installing: a job that crash-loops on a bad
			// config is much harder to debug than an error right here.
			if _, err := config.Load(cfg); err != nil {
				return fmt.Errorf("config does not load, refusing to install: %w", err)
			}

			path, err := launchd.Write(launchd.Options{
				Label:  label,
				Bin:    bin,
				Args:   []string{"serve", "--config", cfg},
				Log:    logp,
				KeepOn: keep,
				// fm reads ~/.fm/config.json and talks to loopback; a proxy
				// inherited from the login session would break both.
				Env: map[string]string{
					"NO_PROXY": "127.0.0.1,localhost",
					"no_proxy": "127.0.0.1,localhost",
				},
			})
			if err != nil {
				return err
			}
			fmt.Println("wrote", path)

			if err := launchd.Load(label, path); err != nil {
				fmt.Println()
				fmt.Println("launchd refused the job from this process. The plist is in place")
				fmt.Println("and will be loaded at next login; to start it now, run:")
				fmt.Println()
				fmt.Printf("  launchctl bootstrap %s %s\n", launchd.Domain(), path)
				fmt.Println()
				return err
			}

			if waitHealthy(cfg, 10*time.Second) {
				fmt.Println("gateway is up")
				return nil
			}
			return errors.New("job loaded but the gateway did not answer /healthz within 10s; see " + logp)
		},
	}
	install.Flags().StringVar(&label, "label", launchd.DefaultLabel, "launchd job label")
	install.Flags().StringVar(&bin, "bin", "", "fmux binary to run (default: this executable)")
	install.Flags().StringVar(&logp, "log", launchd.DefaultLogPath(), "combined stdout/stderr path")
	install.Flags().StringVar(&cfg, "config", "", "config file (default ~/.fmux/config.yaml)")
	install.Flags().BoolVar(&keep, "keepalive", true, "restart the gateway if it crashes")

	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the job and delete the plist",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := launchd.Unload(label); err != nil {
				return err
			}
			path, err := launchd.PlistPath(label)
			if err != nil {
				return err
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			fmt.Println("removed", path)
			return nil
		},
	}
	uninstall.Flags().StringVar(&label, "label", launchd.DefaultLabel, "launchd job label")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show the job state and whether the gateway answers",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, _ := launchd.PlistPath(label)
			st, err := launchd.Status(label)
			switch {
			case errors.Is(err, launchd.ErrNotLoaded):
				fmt.Printf("job      %s (not loaded)\n", label)
				fmt.Printf("plist    %s\n", path)
				if _, serr := os.Stat(path); serr == nil {
					fmt.Println("note     the plist exists; launchd will pick it up at next login")
				} else {
					fmt.Println("note     no plist either; run `fmux service install`")
				}
				return nil
			case err != nil:
				return err
			}

			fmt.Printf("job      %s (%s)\n", label, st.Status)
			fmt.Printf("plist    %s\n", path)
			if st.PID > 0 {
				fmt.Printf("pid      %d\n", st.PID)
			} else {
				fmt.Printf("pid      - (last exit %d)\n", st.LastExit)
			}
			fmt.Printf("health   %s\n", healthLine(cfg))
			return nil
		},
	}
	status.Flags().StringVar(&label, "label", launchd.DefaultLabel, "launchd job label")
	status.Flags().StringVar(&cfg, "config", "", "config file (default ~/.fmux/config.yaml)")

	restart := &cobra.Command{
		Use:   "restart",
		Short: "Kick the job so it reloads the config",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := launchd.Kickstart(label); err != nil {
				return err
			}
			if waitHealthy(cfg, 10*time.Second) {
				fmt.Println("gateway is up")
			}
			return nil
		},
	}
	restart.Flags().StringVar(&label, "label", launchd.DefaultLabel, "launchd job label")
	restart.Flags().StringVar(&cfg, "config", "", "config file (default ~/.fmux/config.yaml)")

	var (
		follow  bool
		logPath string
	)

	logs := &cobra.Command{
		Use:   "logs",
		Short: "Print the gateway log (tail -f with --follow)",
		RunE: func(cmd *cobra.Command, args []string) error {
			p := logPath
			if p == "" {
				p = launchd.DefaultLogPath()
			}
			if follow {
				c := exec.Command("tail", "-f", p)
				c.Stdout, c.Stderr = os.Stdout, os.Stderr
				return c.Run()
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
			if len(lines) > 40 {
				lines = lines[len(lines)-40:]
			}
			fmt.Println(strings.Join(lines, "\n"))
			return nil
		},
	}
	logs.Flags().StringVar(&logPath, "log", "", "log file (default ~/Library/Logs/fmux.log)")
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "keep reading as the file grows")

	svc.AddCommand(install, uninstall, status, restart, logs)
	return svc
}

// waitHealthy polls the gateway until it answers /healthz or the deadline
// passes. A freshly bootstrapped job takes a moment to bind its port.
func waitHealthy(cfgPath string, d time.Duration) bool {
	addr := config.DefaultListen
	if cfgPath != "" {
		if c, err := config.Load(cfgPath); err == nil {
			addr = c.Listen
		}
	} else if c, err := config.Load(config.DefaultPath()); err == nil {
		addr = c.Listen
	}
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
		resp, err := client.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func healthLine(cfgPath string) string {
	addr := config.DefaultListen
	if cfgPath != "" {
		if c, err := config.Load(cfgPath); err == nil {
			addr = c.Listen
		}
	} else if c, err := config.Load(config.DefaultPath()); err == nil {
		addr = c.Listen
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return "no answer on " + addr
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("%s returned %d", addr, resp.StatusCode)
	}
	return "ok on " + addr
}
