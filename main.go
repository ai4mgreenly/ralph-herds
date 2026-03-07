package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type ServiceState int

const (
	StateStopped ServiceState = iota
	StateRunning
)

func (s ServiceState) String() string {
	switch s {
	case StateRunning:
		return "running"
	default:
		return "stopped"
	}
}

type Service struct {
	Name            string
	PortEnvVar      string // empty if no HTTP port
	ProxyPath       string // optional
	DefaultRoute    bool
	Command         string
	WorkDir         string // optional
	ShutdownTimeout time.Duration

	// runtime
	AssignedPort int
	State        ServiceState
}

var registry = []Service{
	{
		Name:            "ralph-plans",
		PortEnvVar:      "RALPH_PLANS_PORT",
		Command:         "ralph-plans",
		ShutdownTimeout: 10 * time.Second,
	},
	{
		Name:            "ralph-shows",
		PortEnvVar:      "RALPH_SHOWS_PORT",
		DefaultRoute:    true,
		Command:         "deno run -A dev.ts",
		WorkDir:         "~/.local/share/ralph-shows",
		ShutdownTimeout: 10 * time.Second,
	},
	{
		Name:            "ralph-runs",
		Command:         "ralph-runs",
		ShutdownTimeout: 2 * time.Minute,
	},
	{
		Name:            "ralph-logs",
		PortEnvVar:      "RALPH_LOGS_PORT",
		Command:         "ralph-logs",
		ShutdownTimeout: 10 * time.Second,
	},
	{
		Name:            "ralph-counts",
		PortEnvVar:      "RALPH_COUNTS_PORT",
		Command:         "ralph-counts",
		ShutdownTimeout: 10 * time.Second,
	},
}

func allocatePorts() {
	baseStr := os.Getenv("RALPH_HERDS_PORT")
	base := 5000
	if baseStr != "" {
		if n, err := strconv.Atoi(baseStr); err == nil {
			base = n
		}
	}

	offset := 0
	for i := range registry {
		if registry[i].PortEnvVar != "" {
			offset++
			registry[i].AssignedPort = base + offset
		}
	}
}

func findService(name string) *Service {
	for i := range registry {
		if registry[i].Name == name {
			return &registry[i]
		}
	}
	return nil
}

func cmdStart(name string) {
	svc := findService(name)
	if svc == nil {
		fmt.Printf("unknown service: %s\n", name)
		return
	}
	if svc.PortEnvVar != "" {
		fmt.Printf("[stub] would start %s: %s=%d %s", svc.Name, svc.PortEnvVar, svc.AssignedPort, svc.Command)
	} else {
		fmt.Printf("[stub] would start %s: %s", svc.Name, svc.Command)
	}
	if svc.WorkDir != "" {
		fmt.Printf(" (in %s)", svc.WorkDir)
	}
	fmt.Println()
}

func cmdStop(name string) {
	svc := findService(name)
	if svc == nil {
		fmt.Printf("unknown service: %s\n", name)
		return
	}
	fmt.Printf("[stub] would stop %s (timeout: %s)\n", svc.Name, svc.ShutdownTimeout)
}

func cmdRestart(name string) {
	svc := findService(name)
	if svc == nil {
		fmt.Printf("unknown service: %s\n", name)
		return
	}
	fmt.Printf("[stub] would restart %s\n", svc.Name)
}

func cmdStartAll() {
	for _, svc := range registry {
		cmdStart(svc.Name)
	}
}

func cmdStopAll() {
	for _, svc := range registry {
		cmdStop(svc.Name)
	}
}

func cmdStatus() {
	fmt.Printf("%-16s %-8s %s\n", "SERVICE", "STATE", "PORT")
	fmt.Println(strings.Repeat("-", 36))
	for _, svc := range registry {
		port := "-"
		if svc.AssignedPort != 0 {
			port = strconv.Itoa(svc.AssignedPort)
		}
		fmt.Printf("%-16s %-8s %s\n", svc.Name, svc.State, port)
	}
}

func cmdHelp() {
	fmt.Println("Available commands:")
	fmt.Println("  start all         start all services")
	fmt.Println("  stop all          stop all services")
	fmt.Println("  start <name>      start a service")
	fmt.Println("  stop <name>       stop a service")
	fmt.Println("  restart <name>    restart a service")
	fmt.Println("  status            show service states and ports")
	fmt.Println("  help              show this help")
	fmt.Println("  quit              stop all services and exit")
}

func handleLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return true
	}

	parts := strings.Fields(line)
	cmd := parts[0]

	switch cmd {
	case "help":
		cmdHelp()
	case "status":
		cmdStatus()
	case "quit":
		fmt.Println("[stub] would stop all services")
		return false
	case "start":
		if len(parts) < 2 {
			fmt.Println("usage: start <name> | start all")
			return true
		}
		if parts[1] == "all" {
			cmdStartAll()
		} else {
			cmdStart(parts[1])
		}
	case "stop":
		if len(parts) < 2 {
			fmt.Println("usage: stop <name> | stop all")
			return true
		}
		if parts[1] == "all" {
			cmdStopAll()
		} else {
			cmdStop(parts[1])
		}
	case "restart":
		if len(parts) < 2 {
			fmt.Println("usage: restart <name>")
			return true
		}
		cmdRestart(parts[1])
	default:
		fmt.Printf("unrecognized command: %s (type 'help' for available commands)\n", cmd)
	}
	return true
}

func main() {
	allocatePorts()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("herds> ")
		if !scanner.Scan() {
			break
		}
		if !handleLine(scanner.Text()) {
			break
		}
	}
}
