package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
		Name:            "ralph-logs",
		PortEnvVar:      "RALPH_LOGS_PORT",
		HostEnvVar:      "RALPH_LOGS_HOST",
		Command:         "ralph-logs",
		ShutdownTimeout: 10 * time.Second,
		Noop:            false,
	},
	{
		Name:            "ralph-counts",
		PortEnvVar:      "RALPH_COUNTS_PORT",
		HostEnvVar:      "RALPH_COUNTS_HOST",
		Command:         "ralph-counts",
		ShutdownTimeout: 10 * time.Second,
	},
	{
		Name:            "ralph-shows",
		PortEnvVar:      "RALPH_SHOWS_PORT",
		HostEnvVar:      "RALPH_SHOWS_HOST",
		DefaultRoute:    true,
		Command:         "ralph-shows",
		ShutdownTimeout: 10 * time.Second,
		Noop:            false,
	},
	{
		Name:            "ralph-runs",
		Command:         "ralph-runs --max=3",
		ShutdownTimeout: 2 * time.Minute,
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

func logsDir() string {
	dir := os.Getenv("RALPH_LOGS_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dir = filepath.Join(home, ".local", "state", "ralph", "logs")
	}
	return dir
}

func buildCmd(svc *Service) *exec.Cmd {
	parts := strings.Fields(svc.Command)
	logDir := logsDir()
	if svc.Name == "ralph-logs" {
		parts = append(parts, filepath.Join(logDir, "*.log"))
		parts = append(parts, filepath.Join(logDir, "../goals/*/ralph.log"))
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	env := os.Environ()

	host := os.Getenv("RALPH_HERDS_HOST")
	if host == "" {
		host = "localhost"
	}

	herdsPort := os.Getenv("RALPH_HERDS_PORT")
	if herdsPort == "" {
		herdsPort = "8000"
	}

	// Inject each service's own host/port for binding, plus URL env vars
	// for all services so they can reach each other by subdomain.
	for _, s := range registry {
		if s.PortEnvVar != "" && s.AssignedPort != 0 {
			env = append(env, fmt.Sprintf("%s=%d", s.PortEnvVar, s.AssignedPort))
			env = append(env, fmt.Sprintf("%s=%s", s.HostEnvVar, host))
			urlEnvVar := strings.Replace(s.PortEnvVar, "_PORT", "_URL", 1)
			env = append(env, fmt.Sprintf("%s=http://%s.localhost:%s", urlEnvVar, s.Name, herdsPort))
		}
	}

	env = append(env, fmt.Sprintf("RALPH_HERDS_URL=http://localhost:%s", herdsPort))
	env = append(env, fmt.Sprintf("RALPH_LOGS_DIR=%s", logDir))

	cmd.Env = env
	if svc.WorkDir != "" {
		workDir := svc.WorkDir
		if strings.HasPrefix(workDir, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				workDir = filepath.Join(home, workDir[2:])
			}
		}
		cmd.Dir = workDir
	}

	if err := os.MkdirAll(logDir, 0755); err != nil {
		fmt.Printf("warning: could not create log dir %s: %v\n", logDir, err)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd
	}

	logPath := filepath.Join(logDir, svc.Name+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Printf("warning: could not open log file %s: %v\n", logPath, err)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd
	}

	cmd.Stdout = logFile
	cmd.Stderr = logFile
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
			herdsPort := os.Getenv("RALPH_HERDS_PORT")
			if herdsPort == "" {
				herdsPort = "8000"
			}
			fmt.Printf("[noop] would start %s: port=%d url=http://%s.localhost:%s %s", svc.Name, svc.AssignedPort, svc.Name, herdsPort, svc.Command)
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
	for i := len(registry) - 1; i >= 0; i-- {
		cmdStop(registry[i].Name)
	}
}

func cmdStatus() {
	basePortStr := os.Getenv("RALPH_HERDS_PORT")
	basePort := 8000
	if basePortStr != "" {
		if n, err := strconv.Atoi(basePortStr); err == nil {
			basePort = n
		}
	}

	fmt.Printf("%-16s %-10s %-8s %-6s %s\n", "SERVICE", "STATE", "PID", "PORT", "URL")
	fmt.Println(strings.Repeat("-", 60))
	for _, svc := range registry {
		svc.mu.Lock()
		state := svc.State
		pid := svc.Pid
		port := svc.AssignedPort
		noop := svc.Noop
		svc.mu.Unlock()

		stateStr := state.String()
		if noop {
			stateStr = "noop"
		}

		pidStr := "-"
		if state == StateRunning && pid != 0 {
			pidStr = strconv.Itoa(pid)
		}

		portStr := "-"
		if port != 0 {
			portStr = strconv.Itoa(port)
		}

		urlStr := "-"
		if svc.DefaultRoute {
			urlStr = fmt.Sprintf("http://localhost:%d/", basePort)
		} else if svc.PortEnvVar != "" {
			urlStr = fmt.Sprintf("http://%s.localhost:%d/", svc.Name, basePort)
		}

		fmt.Printf("%-16s %-10s %-8s %-6s %s\n", svc.Name, stateStr, pidStr, portStr, urlStr)
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
	fmt.Println("  clear             clear the terminal screen")
	fmt.Println("  help              show this help")
	fmt.Println("  quit, exit        stop all services and exit")
}

func stopAllRunning() {
	for i := len(registry) - 1; i >= 0; i-- {
		svc := registry[i]
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
	case "clear":
		fmt.Print("\033[2J\033[H")
	case "status":
		cmdStatus()
	case "quit", "exit":
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

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func proxyWebSocket(w http.ResponseWriter, r *http.Request, upstreamHost string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	upstreamConn, err := net.Dial("tcp", upstreamHost)
	if err != nil {
		fmt.Fprintf(clientConn, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer upstreamConn.Close()

	// Manually write the request line and headers to preserve hop-by-hop
	// headers (Connection, Upgrade) that r.Write() would strip.
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	if path == "" {
		path = "/"
	}
	fmt.Fprintf(upstreamConn, "%s %s HTTP/1.1\r\n", r.Method, path)
	fmt.Fprintf(upstreamConn, "Host: %s\r\n", upstreamHost)
	for key, vals := range r.Header {
		for _, val := range vals {
			fmt.Fprintf(upstreamConn, "%s: %s\r\n", key, val)
		}
	}
	fmt.Fprintf(upstreamConn, "\r\n")

	done := make(chan struct{}, 2)
	go func() {
		io.Copy(upstreamConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, upstreamConn)
		done <- struct{}{}
	}()
	<-done
}

func makeProxyHandler(proxy http.Handler, upstreamHost string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebSocketUpgrade(r) {
			proxyWebSocket(w, r, upstreamHost)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func startProxy() {
	host := os.Getenv("RALPH_HERDS_HOST")
	if host == "" {
		host = "localhost"
	}
	portStr := os.Getenv("RALPH_HERDS_PORT")
	proxyPort := 8000
	if portStr != "" {
		if n, err := strconv.Atoi(portStr); err == nil {
			proxyPort = n
		}
	}
	addr := fmt.Sprintf("%s:%d", host, proxyPort)

	// Build a map of subdomain -> handler for Host-header routing.
	handlers := make(map[string]http.Handler)
	var defaultHandler http.Handler
	for _, svc := range registry {
		if svc.AssignedPort == 0 {
			continue
		}
		upstream, err := url.Parse(fmt.Sprintf("http://%s:%d", host, svc.AssignedPort))
		if err != nil {
			continue
		}
		proxy := httputil.NewSingleHostReverseProxy(upstream)
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		}
		handler := makeProxyHandler(proxy, upstream.Host)
		if svc.DefaultRoute {
			defaultHandler = handler
		}
		// Route by subdomain: ralph-plans.localhost -> ralph-plans backend
		handlers[svc.Name+".localhost"] = handler
	}

	mainHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostname := r.Host
		if h, _, err := net.SplitHostPort(hostname); err == nil {
			hostname = h
		}
		if handler, ok := handlers[hostname]; ok {
			handler.ServeHTTP(w, r)
			return
		}
		if defaultHandler != nil {
			defaultHandler.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})

	fmt.Printf("proxy listening on http://localhost:%d/\n", proxyPort)
	go func() {
		if err := http.ListenAndServe(addr, mainHandler); err != nil {
			fmt.Printf("proxy error: %v\n", err)
		}
	}()
}

func main() {
	allocatePorts()
	startProxy()

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
