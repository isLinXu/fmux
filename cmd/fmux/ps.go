package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// processesResponse mirrors /admin/processes.
type processesResponse struct {
	Processes []struct {
		Name      string    `json:"name"`
		State     string    `json:"state"`
		PID       int       `json:"pid"`
		Restarts  int       `json:"restarts"`
		LastUsed  time.Time `json:"last_used"`
		LastError string    `json:"last_error"`
	} `json:"processes"`
	Resident []struct {
		Backend string `json:"backend"`
		Model   string `json:"model"`
		SizeMB  int    `json:"size_mb"`
		Pins    int    `json:"pins"`
	} `json:"resident"`
	Budget struct {
		LimitMB int `json:"limit_mb"`
		UsedMB  int `json:"used_mb"`
	} `json:"budget"`
}

// newPsCmd reports the live state of the gateway's supervised processes.
//
// It talks to a running gateway rather than reading the config, because process
// state only exists inside the serving process.
func newPsCmd() *cobra.Command {
	var (
		cfgPath string
		gateway string
	)
	c := &cobra.Command{
		Use:   "ps",
		Short: "Show supervised backends and memory residency",
		RunE: func(cmd *cobra.Command, args []string) error {
			if gateway == "" {
				cfg, err := loadConfig(cfgPath)
				if err != nil {
					return err
				}
				gateway = "http://" + cfg.Listen
			}
			gateway = strings.TrimRight(gateway, "/")

			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Get(gateway + "/admin/processes")
			if err != nil {
				return fmt.Errorf("gateway not reachable at %s: %w", gateway, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("gateway returned %s", resp.Status)
			}
			var out processesResponse
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				return err
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "BACKEND\tSTATE\tPID\tRESTARTS\tLAST USED\tNOTE")
			if len(out.Processes) == 0 {
				fmt.Fprintln(w, "(none)\t\t\t\t\tall backends are externally managed")
			}
			for _, p := range out.Processes {
				last := "-"
				if !p.LastUsed.IsZero() {
					last = p.LastUsed.Format("15:04:05")
				}
				note := p.LastError
				if note == "" && p.State == "stopped" {
					note = "starts on first request"
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n", p.Name, p.State, p.PID, p.Restarts, last, note)
			}
			if len(out.Resident) > 0 {
				fmt.Fprintln(w, "\t\t\t\t\t")
				fmt.Fprintln(w, "RESIDENT MODEL\tBACKEND\tSIZE\tIN USE\t\t")
				for _, r := range out.Resident {
					size := "unknown"
					if r.SizeMB > 0 {
						size = fmt.Sprintf("%d MB", r.SizeMB)
					}
					// A pinned model cannot be evicted, so it is worth
					// showing: it explains why the budget cannot be reclaimed.
					inUse := "-"
					if r.Pins > 0 {
						inUse = fmt.Sprintf("%d req", r.Pins)
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\t\n", r.Model, r.Backend, size, inUse)
				}
			}
			if out.Budget.LimitMB > 0 {
				fmt.Fprintf(w, "\t\t\t\t\t\nbudget: %d / %d MB\n", out.Budget.UsedMB, out.Budget.LimitMB)
			}
			return w.Flush()
		},
	}
	c.Flags().StringVar(&cfgPath, "config", "", "config file (default ~/.fmux/config.yaml)")
	c.Flags().StringVar(&gateway, "gateway", "", "gateway base URL (default from config listen)")
	return c
}
