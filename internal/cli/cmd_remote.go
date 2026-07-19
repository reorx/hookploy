package cli

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/reorx/hookploy/internal/api"
	"github.com/reorx/hookploy/internal/apiclient"
)

// Remote commands talk to main's status API via HOOKPLOY_URL +
// HOOKPLOY_ADMIN_TOKEN. --json prints the API body verbatim, so CLI JSON
// output is identical to the API by construction.

func client(ctx *Context) (*apiclient.Client, int) {
	c, err := apiclient.FromEnv()
	if err != nil {
		fmt.Fprintf(ctx.Stderr, "%v\n", err)
		return nil, 1
	}
	return c, 0
}

func fail(ctx *Context, err error) int {
	fmt.Fprintf(ctx.Stderr, "error: %v\n", err)
	return 1
}

func cmdStatus(ctx *Context, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	asJSON := fs.Bool("json", false, "output JSON")
	if _, ok := parseInterleaved(fs, args); !ok {
		return 2
	}
	c, code := client(ctx)
	if code != 0 {
		return code
	}
	servers, _, err := c.Servers()
	if err != nil {
		return fail(ctx, err)
	}
	services, _, err := c.Services()
	if err != nil {
		return fail(ctx, err)
	}
	if *asJSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(api.Status{Servers: servers, Services: services})
		return 0
	}
	mainVersion := ""
	for _, s := range servers {
		if s.Local {
			mainVersion = s.Version
		}
	}
	fmt.Fprintln(ctx.Stdout, "SERVERS")
	for _, s := range servers {
		kind := "edge"
		if s.Local {
			kind = "local"
		}
		extra := ""
		if s.Version != "" {
			extra = "  " + s.Version
			if !s.Local && mainVersion != "" && s.Version != mainVersion {
				extra += " (outdated)"
			}
		}
		if s.ConnectedAt != nil {
			extra += fmt.Sprintf("  connected %s", humanAge(*s.ConnectedAt))
		}
		fmt.Fprintf(ctx.Stdout, "  %-16s %-6s %s%s\n", s.Name, kind, s.Status, extra)
	}
	fmt.Fprintln(ctx.Stdout, "SERVICES")
	for _, s := range services {
		last := "-"
		if s.LastDeploy != nil {
			last = fmt.Sprintf("%s  %s  %s", s.LastDeploy.Status, s.LastDeploy.ID, humanAge(s.LastDeploy.CreatedAt))
		}
		hook := ""
		if !s.Webhook {
			hook = "  (webhook off)"
		}
		fmt.Fprintf(ctx.Stdout, "  %-16s %s%s\n", s.Name, last, hook)
	}
	return 0
}

func cmdDeploys(ctx *Context, args []string) int {
	fs := flag.NewFlagSet("deploys", flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	asJSON := fs.Bool("json", false, "output JSON")
	pos, ok := parseInterleaved(fs, args)
	if !ok {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(ctx.Stderr, "usage: hookploy deploys <service> [--json]")
		return 2
	}
	c, code := client(ctx)
	if code != 0 {
		return code
	}
	deploys, raw, err := c.ServiceDeploys(pos[0])
	if err != nil {
		return fail(ctx, err)
	}
	if *asJSON {
		ctx.Stdout.Write(raw)
		return 0
	}
	for _, d := range deploys {
		line := fmt.Sprintf("%s  %-11s %-7s %s", d.ID, d.Status, d.Kind, humanAge(d.CreatedAt))
		if d.Error != "" {
			line += "  " + d.Error
		}
		fmt.Fprintln(ctx.Stdout, line)
	}
	return 0
}

func cmdLogs(ctx *Context, args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	follow := fs.Bool("f", false, "follow until the deploy finishes")
	asJSON := fs.Bool("json", false, "output NDJSON log lines")
	pos, ok := parseInterleaved(fs, args)
	if !ok {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(ctx.Stderr, "usage: hookploy logs <deploy-id> [-f] [--json]")
		return 2
	}
	c, code := client(ctx)
	if code != 0 {
		return code
	}
	body, err := c.Logs(pos[0], *follow, *asJSON)
	if err != nil {
		return fail(ctx, err)
	}
	defer body.Close()

	if !*follow {
		io.Copy(ctx.Stdout, body)
		return 0
	}
	// follow mode: NDJSON stream; print log data, exit by final status
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if *asJSON {
			fmt.Fprintln(ctx.Stdout, sc.Text())
		}
		var probe struct {
			Done   bool   `json:"done"`
			Status string `json:"status"`
			Data   string `json:"data"`
		}
		if err := json.Unmarshal(sc.Bytes(), &probe); err != nil {
			continue
		}
		if probe.Done {
			if !*asJSON {
				fmt.Fprintf(ctx.Stderr, "deploy finished: %s\n", probe.Status)
			}
			if probe.Status != "succeeded" {
				return 1
			}
			return 0
		}
		if !*asJSON {
			io.WriteString(ctx.Stdout, probe.Data)
		}
	}
	if err := sc.Err(); err != nil {
		return fail(ctx, err)
	}
	return 0
}

func cmdDeploy(ctx *Context, args []string) int {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	payload := fs.String("payload", "", "JSON payload object")
	asJSON := fs.Bool("json", false, "output JSON")
	pos, ok := parseInterleaved(fs, args)
	if !ok {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(ctx.Stderr, "usage: hookploy deploy <service> [--payload '{}'] [--json]")
		return 2
	}
	c, code := client(ctx)
	if code != 0 {
		return code
	}
	acc, raw, err := c.TriggerDeploy(pos[0], payloadBytes(*payload))
	if err != nil {
		return fail(ctx, err)
	}
	return printAccepted(ctx, acc, raw, *asJSON)
}

func cmdTask(ctx *Context, args []string) int {
	fs := flag.NewFlagSet("task", flag.ContinueOnError)
	fs.SetOutput(ctx.Stderr)
	instance := fs.String("instance", "", "target instance (required for multi-instance services)")
	payload := fs.String("payload", "", "JSON payload object")
	asJSON := fs.Bool("json", false, "output JSON")
	pos, ok := parseInterleaved(fs, args)
	if !ok {
		return 2
	}
	if len(pos) != 2 {
		fmt.Fprintln(ctx.Stderr, "usage: hookploy task <service> <name> [--instance <i>] [--json]")
		return 2
	}
	c, code := client(ctx)
	if code != 0 {
		return code
	}
	acc, raw, err := c.TriggerTask(pos[0], pos[1], *instance, payloadBytes(*payload))
	if err != nil {
		return fail(ctx, err)
	}
	return printAccepted(ctx, acc, raw, *asJSON)
}

func payloadBytes(s string) []byte {
	if s == "" {
		return []byte("{}")
	}
	return []byte(s)
}

func printAccepted(ctx *Context, acc *api.Accepted, raw []byte, asJSON bool) int {
	if asJSON {
		ctx.Stdout.Write(raw)
		return 0
	}
	fmt.Fprintf(ctx.Stdout, "accepted: %s\nfollow with: hookploy logs %s -f\n", acc.DeployID, acc.DeployID)
	return 0
}

func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
