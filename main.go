package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	HostEnvVar      string // empty if no HTTP host
	ProxyPath       string // optional
	DefaultRoute    bool
	Command         string
	WorkDir         string // optional
	ShutdownTimeout time.Duration
	Noop            bool

	// runtime (protected by mu)
	AssignedPort int
	State        ServiceState
	Pid          int
	mu           sync.Mutex
	cmd          *exec.Cmd
	procDone     chan struct{} // closed when current process's Wait() returns
	stopCh       chan struct{} // closed to prevent auto-restart
}

var registry = []*Service{
	{
		Name:            "ralph-plans",
		PortEnvVar:      "RALPH_PLANS_PORT",
		HostEnvVar:      "RALPH_PLANS_HOST",
		Command:         "ralph-plans",
		ShutdownTimeout: 10 * time.Second,
		Noop:            false,
	},
	{
		Name:            "ralph-shows",
		PortEnvVar:      "RALPH_SHOWS_PORT",
		HostEnvVar:      "RALPH_SHOWS_HOST",
		DefaultRoute:    true,
		Command:         "deno run -A dev.ts",
		WorkDir:         "~/.local/share/ralph-shows",
		ShutdownTimeout: 10 * time.Second,
		Noop:            true,
	},
	{
		Name:            "ralph-runs",
		Command:         "ralph-runs",
		ShutdownTimeout: 2 * time.Minute,
		Noop:            true,
	},
	{
		Name:            "ralph-logs",
		PortEnvVar:      "RALPH_LOGS_PORT",
		HostEnvVar:      "RALPH_LOGS_HOST",
		Command:         "ralph-logs",
		ShutdownTimeout: 10 * time.Second,
		Noop:            true,
	},
	{
		Name:            "ralph-counts",
		PortEnvVar:      "RALPH_COUNTS_PORT",
		HostEnvVar:      "RALPH_COUNTS_HOST",
		Command:         "ralph-counts",
		ShutdownTimeout: 10 * time.Second,
		Noop:            true,
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
	for _, svc := range registry {
		if svc.PortEnvVar != "" {
			offset++
			svc.AssignedPort = base + offset
		}
	}
}

func findService(name string) *Service {
	for _, svc := range registry {
		if svc.Name == name {
			return svc
		}
	}
	return nil
}

func buildCmd(svc *Service) *exec.Cmd {
	parts := strings.Fields(svc.Command)
	cmd := exec.Command(parts[0], parts[1:]...)
	env := os.Environ()

	host := os.Getenv("RALPH_HERDS_HOST")
	if host == "" {
		host = "localhost"
	}

	if svc.PortEnvVar != "" {
		env = append(env, fmt.Sprintf("%s=%d", svc.PortEnvVar, svc.AssignedPort))
	}
	if svc.HostEnvVar != "" {
		env = append(env, fmt.Sprintf("%s=%s", svc.HostEnvVar, host))
	}

	// ralph-plans uses RALPH_SHOWS_PORT for CORS configuration
	if svc.Name == "ralph-plans" {
		if shows := findService("ralph-shows"); shows != nil && shows.AssignedPort != 0 {
			env = append(env, fmt.Sprintf("RALPH_SHOWS_PORT=%d", shows.AssignedPort))
		}
	}

	cmd.Env = env
	if svc.WorkDir != "" {
		cmd.Dir = svc.WorkDir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

func startProcess(svc *Service) {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	if svc.State == StateRunning {
		fmt.Printf("%s is already running\n", svc.Name)
		return
	}

	cmd := buildCmd(svc)
	if err := cmd.Start(); err != nil {
		fmt.Printf("failed to start %s: %v\n", svc.Name, err)
		return
	}

	procDone := make(chan struct{})
	stopCh := make(chan struct{})

	svc.cmd = cmd
	svc.Pid = cmd.Process.Pid
	svc.State = StateRunning
	svc.procDone = procDone
	svc.stopCh = stopCh

	now := time.Now()
	fmt.Printf("%s started at %s on PID %d\n", svc.Name, now.Format("3:04:05 PM"), svc.Pid)

	go manageProcess(svc, stopCh)
}

// manageProcess watches the current process and auto-restarts on crash.
// It reads svc.cmd and svc.procDone at the top of each iteration.
func manageProcess(svc *Service, stopCh chan struct{}) {
	backoff := time.Second

	for {
		svc.mu.Lock()
		cmd := svc.cmd
		procDone := svc.procDone
		svc.mu.Unlock()

		if cmd == nil {
			return
		}

		exitErr := cmd.Wait()
		close(procDone)

		svc.mu.Lock()
		svc.State = StateStopped
		svc.cmd = nil
		svc.Pid = 0
		svc.mu.Unlock()

		select {
		case <-stopCh:
			return
		default:
		}

		if exitErr != nil {
			fmt.Printf("\n%s crashed (%v), restarting in %s\n", svc.Name, exitErr, backoff)
		} else {
			fmt.Printf("\n%s exited, restarting in %s\n", svc.Name, backoff)
		}

		// Inner loop: backoff and retry until restart succeeds or stop is requested.
		for {
			timer := time.NewTimer(backoff)
			select {
			case <-stopCh:
				timer.Stop()
				return
			case <-timer.C:
			}
			backoff = min(backoff*2, 30*time.Second)

			newCmd := buildCmd(svc)
			newProcDone := make(chan struct{})

			if err := newCmd.Start(); err != nil {
				fmt.Printf("failed to restart %s: %v, retrying in %s\n", svc.Name, err, backoff)
				continue
			}

			svc.mu.Lock()
			svc.cmd = newCmd
			svc.Pid = newCmd.Process.Pid
			svc.State = StateRunning
			svc.procDone = newProcDone
			svc.mu.Unlock()

			now := time.Now()
			fmt.Printf("herds> %s restarted at %s on PID %d\n", svc.Name, now.Format("3:04:05 PM"), svc.Pid)

			break // back to outer loop to Wait on new process
		}
	}
}

func stopProcess(svc *Service) {
	svc.mu.Lock()
	if svc.State != StateRunning {
		fmt.Printf("%s is not running\n", svc.Name)
		svc.mu.Unlock()
		return
	}
	close(svc.stopCh)
	cmd := svc.cmd
	procDone := svc.procDone
	svc.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGINT)
	}

	select {
	case <-procDone:
	case <-time.After(svc.ShutdownTimeout):
		svc.mu.Lock()
		if svc.cmd != nil && svc.cmd.Process != nil {
			_ = svc.cmd.Process.Kill()
		}
		svc.mu.Unlock()
		<-procDone
	}

	fmt.Printf("%s stopped\n", svc.Name)
}

func cmdStart(name string) {
	svc := findService(name)
	if svc == nil {
		fmt.Printf("unknown service: %s\n", name)
		return
	}
	if svc.Noop {
		if svc.PortEnvVar != "" {
			host := os.Getenv("RALPH_HERDS_HOST")
			if host == "" {
				host = "localhost"
			}
			fmt.Printf("[noop] would start %s: %s=%s %s=%d %s", svc.Name, svc.HostEnvVar, host, svc.PortEnvVar, svc.AssignedPort, svc.Command)
		} else {
			fmt.Printf("[noop] would start %s: %s", svc.Name, svc.Command)
		}
		if svc.WorkDir != "" {
			fmt.Printf(" (in %s)", svc.WorkDir)
		}
		fmt.Println()
		return
	}
	startProcess(svc)
}

func cmdStop(name string) {
	svc := findService(name)
	if svc == nil {
		fmt.Printf("unknown service: %s\n", name)
		return
	}
	if svc.Noop {
		fmt.Printf("[noop] would stop %s (timeout: %s)\n", svc.Name, svc.ShutdownTimeout)
		return
	}
	stopProcess(svc)
}

func cmdRestart(name string) {
	svc := findService(name)
	if svc == nil {
		fmt.Printf("unknown service: %s\n", name)
		return
	}
	if svc.Noop {
		fmt.Printf("[noop] would restart %s\n", svc.Name)
		return
	}
	stopProcess(svc)
	startProcess(svc)
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
	fmt.Printf("%-16s %-10s %s\n", "SERVICE", "STATE", "PORT")
	fmt.Println(strings.Repeat("-", 40))
	for _, svc := range registry {
		svc.mu.Lock()
		state := svc.State
		pid := svc.Pid
		port := svc.AssignedPort
		noop := svc.Noop
		svc.mu.Unlock()

		portStr := "-"
		if port != 0 {
			portStr = strconv.Itoa(port)
		}

		if noop {
			fmt.Printf("%-16s %-10s %s\n", svc.Name, "noop", portStr)
		} else if state == StateRunning {
			fmt.Printf("%-16s %-10s %s (PID %d)\n", svc.Name, state, portStr, pid)
		} else {
			fmt.Printf("%-16s %-10s %s\n", svc.Name, state, portStr)
		}
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

func stopAllRunning() {
	for _, svc := range registry {
		if svc.Noop {
			continue
		}
		svc.mu.Lock()
		running := svc.State == StateRunning
		svc.mu.Unlock()
		if running {
			stopProcess(svc)
		}
	}
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
		stopAllRunning()
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println()
		stopAllRunning()
		os.Exit(0)
	}()

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
