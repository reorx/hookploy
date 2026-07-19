package cli

func init() {
	register(&command{Name: "main", Summary: "run the main process (webhook + scheduler + API)", Run: cmdMain})
	register(&command{Name: "edge", Summary: "run the edge executor (connects out to main)", Run: cmdEdge})
	register(&command{Name: "validate", Summary: "statically validate hookploy.yaml", Run: cmdValidate})
	register(&command{Name: "status", Summary: "overview: servers and latest deploy per service", Run: cmdStatus})
	register(&command{Name: "deploys", Summary: "deploy history of a service", Run: cmdDeploys})
	register(&command{Name: "logs", Summary: "logs of a deploy", Run: cmdLogs})
	register(&command{Name: "deploy", Summary: "manually trigger a deploy", Run: cmdDeploy})
	register(&command{Name: "task", Summary: "run a named task of a service", Run: cmdTask})
	register(&command{Name: "token", Summary: "manage service tokens (create/rotate/revoke)", Sub: map[string]*command{
		"create": {Name: "create", Run: cmdTokenCreate},
		"rotate": {Name: "rotate", Run: cmdTokenRotate},
		"revoke": {Name: "revoke", Run: cmdTokenRevoke},
	}})
	register(&command{Name: "server", Summary: "server management (token create)", Sub: map[string]*command{
		"token": {Name: "token", Run: cmdServerToken},
	}})
	register(&command{Name: "admin-token", Summary: "manage admin tokens (create)", Sub: map[string]*command{
		"create": {Name: "create", Run: cmdAdminTokenCreate},
	}})
}
