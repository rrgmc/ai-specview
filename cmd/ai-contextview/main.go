// Command ai-contextview shows the spec and the code side by side while Claude
// elaborates a spec. Subcommands: mcp (the per-Claude stdio MCP bridge),
// serve (the web viewer daemon), fakeapp (a terminal test double for serve),
// version.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rrgmc/ai-contextview/internal/appconn"
	"github.com/rrgmc/ai-contextview/internal/daemon"
	"github.com/rrgmc/ai-contextview/internal/detach"
	"github.com/rrgmc/ai-contextview/internal/fakeapp"
	"github.com/rrgmc/ai-contextview/internal/hook"
	"github.com/rrgmc/ai-contextview/internal/identity"
	"github.com/rrgmc/ai-contextview/internal/protocol"
	"github.com/rrgmc/ai-contextview/internal/resolve"
	"github.com/rrgmc/ai-contextview/internal/tools"
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage: ai-contextview <command> [flags]

commands:
  mcp       run the stdio MCP server (Claude Code starts this)
  serve     run the web viewer daemon (--detach, --restart, --stop, --addr, --idle-exit)
  fakeapp   run a terminal stand-in for the daemon (testing)
  hook      Claude Code hook: block the built-in question tool while a spec is open or ask mode is on; report written documents and started skills
  version   print the version
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "mcp":
		err = runMCP()
	case "serve":
		err = runServe(os.Args[2:])
	case "fakeapp":
		err = runFakeapp(os.Args[2:], protocol.SocketPath(), os.Stdin, os.Stdout, os.Stderr)
	case "hook":
		// A Claude Code PreToolUse hook: exit 2 blocks the tool.
		os.Exit(hook.Run(os.Stdin, os.Stdout, os.Stderr, protocol.SocketPath(), identity.AncestorPIDs(16)))
	case "version", "--version", "-v":
		fmt.Println("ai-contextview", version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ai-contextview:", err)
		os.Exit(1)
	}
}

// ------------------------------------------------------------------- mcp

func runMCP() error {
	logger := log.New(os.Stderr, "ai-contextview mcp: ", log.LstdFlags)
	id, err := identity.New()
	if err != nil {
		return err
	}
	askWait := 10 * time.Minute
	if v := os.Getenv("AI_CONTEXTVIEW_ASK_WAIT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			askWait = d
		} else {
			logger.Printf("ignoring bad AI_CONTEXTVIEW_ASK_WAIT=%q", v)
		}
	}
	daemonCmd, err := daemonCommand()
	if err != nil {
		return err
	}
	var bridge *tools.Bridge
	client := appconn.New(appconn.Options{
		Socket:    protocol.SocketPath(),
		DaemonCmd: daemonCmd,
		Hello: protocol.Hello{
			WS: id.WS, Cwd: id.Cwd, ClaudePID: id.ClaudePID, BridgePID: id.BridgePID, BridgePPID: id.BridgePPID,
			BridgeVersion: version, ProjectName: id.ProjectName,
		},
		Logf: logger.Printf,
		OnDocShown: func(d protocol.DocShown) {
			if bridge != nil {
				bridge.DocShown(d)
			}
		},
		OnSessionCwd: func(s protocol.SessionCwd) {
			if bridge != nil {
				bridge.SessionCwd(s)
			}
		},
	})
	defer client.Close()

	bridge = &tools.Bridge{
		Cwd: id.Cwd, Daemon: client, Resolver: resolve.New(), AskWait: askWait,
		Open: tools.OpenBrowser, OpenIn: tools.OpenInFromEnv(), StayInViewer: tools.AwayFromEnv(), Logf: logger.Printf,
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "ai-contextview", Version: version}, &mcp.ServerOptions{
		Instructions: "ai-contextview shows the spec and the code beside the chat. Call spec_show when a spec is created or " +
			"edited, and ask every clarifying question with ask, quoting verbatim the spec passage it is about and pointing at the code.",
	})
	tools.Register(server, bridge)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if tools.AskModeFromEnv() {
		// Ask mode for every session: tell the daemon at once, so the hook
		// blocks the built-in question tool before the first tool call.
		go func() {
			if _, _, err := bridge.AskModeTool(ctx, nil, protocol.AskModeInput{}); err != nil {
				logger.Printf("ask mode: %v", err)
			}
		}()
	}
	// The MCP server starts at once; the daemon connection is made lazily on
	// the first tool call (or now, in ask mode). Run returns when stdin
	// closes (Claude exited).
	err = server.Run(ctx, &mcp.StdioTransport{})
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return err
}

// daemonCommand is $AI_CONTEXTVIEW_DAEMON (run with sh -c) or this executable with
// `serve --detach`.
func daemonCommand() ([]string, error) {
	if v := os.Getenv("AI_CONTEXTVIEW_DAEMON"); v != "" {
		return []string{"/bin/sh", "-c", v}, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, "serve", "--detach"}, nil
}

// ------------------------------------------------------------------- serve

// ping asks a running daemon for its URL.
func ping(sock string, typ string) (*protocol.PingResult, error) {
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	env, _ := protocol.NewEnvelope(typ, "p1", "", "", map[string]any{})
	b, _ := json.Marshal(env)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64*1024), protocol.MaxLine)
	if !sc.Scan() {
		return nil, errors.New("no reply from daemon")
	}
	var reply protocol.Envelope
	if err := json.Unmarshal(sc.Bytes(), &reply); err != nil {
		return nil, err
	}
	var res protocol.PingResult
	_ = reply.Decode(&res)
	return &res, nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	defAddr := "127.0.0.1:7878"
	if v := os.Getenv("AI_CONTEXTVIEW_ADDR"); v != "" {
		defAddr = v
	}
	addr := fs.String("addr", defAddr, "HTTP listen address (loopback); the next 9 ports are tried when taken")
	detach := fs.Bool("detach", false, "start in the background and exit once the socket is listening")
	restart := fs.Bool("restart", false, "replace a running daemon (or start one), in the background")
	stop := fs.Bool("stop", false, "stop a running daemon and exit")
	idleExit := fs.Duration("idle-exit", 0, "exit after this long with no bridges and no pages (0 = never)")
	_ = fs.Parse(args)

	sock := protocol.SocketPath()
	if *stop {
		if _, err := ping(sock, protocol.TypePing); err != nil {
			fmt.Println("ai-contextview is not running")
			return nil
		}
		_, _ = ping(sock, protocol.TypeShutdown)
		fmt.Println("ai-contextview stopped")
		return nil
	}
	oldPID := 0
	if *restart {
		if old, err := ping(sock, protocol.TypePing); err == nil {
			oldPID = old.PID
			_, _ = ping(sock, protocol.TypeShutdown)
			// A running Claude session may start a new daemon as soon as the
			// old one exits. Wait until the old process is gone: either nothing
			// answers (start one below) or a different process does.
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				res, err := ping(sock, protocol.TypePing)
				if err != nil {
					break
				}
				if res.PID != old.PID {
					fmt.Printf("ai-contextview restarted (pid %d, was %d): %s\n", res.PID, old.PID, res.URL)
					fmt.Println("a running Claude session started it; it uses the binary that session runs")
					return nil
				}
				time.Sleep(100 * time.Millisecond)
			}
			if res, err := ping(sock, protocol.TypePing); err == nil && res.PID == old.PID {
				return fmt.Errorf("the running daemon (pid %d) did not stop", old.PID)
			}
		}
	}
	if res, err := ping(sock, protocol.TypePing); err == nil {
		fmt.Printf("ai-contextview %s is already running: %s\n", res.DaemonVersion, res.URL)
		if res.DaemonVersion != version {
			fmt.Printf("this binary is %s; run `ai-contextview serve --restart` to switch\n", version)
		}
		return nil
	}
	// --restart always starts the new daemon in the background, so it never
	// takes over the terminal it was run from.
	if *detach || *restart {
		res, err := detachServe(sock, *addr, *idleExit)
		if err != nil {
			return err
		}
		switch {
		case *restart && oldPID != 0:
			fmt.Printf("ai-contextview restarted (pid %d, was %d): %s\n", res.PID, oldPID, res.URL)
		case *restart:
			fmt.Printf("ai-contextview started (pid %d): %s\n", res.PID, res.URL)
		default:
			fmt.Println(res.URL)
		}
		return nil
	}
	return serve(sock, *addr, *idleExit)
}

// detachServe starts the daemon in the background and waits until it answers.
func detachServe(sock, addr string, idle time.Duration) (*protocol.PingResult, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logDir := protocol.StateDir()
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, "ai-contextview.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	args := []string{"serve", "--addr", addr}
	if idle > 0 {
		args = append(args, "--idle-exit", idle.String())
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = detach.SysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	_ = cmd.Process.Release()
	for i := 0; i < 100; i++ {
		if res, err := ping(sock, protocol.TypePing); err == nil {
			return res, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon did not start; see %s", logFile.Name())
}

func serve(sock, addr string, idle time.Duration) error {
	logger := log.New(os.Stderr, "ai-contextview serve: ", log.LstdFlags)
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return err
	}
	// Only one daemon per socket. Several bridges may start one at the same
	// moment (after a crash, say): the ones that lose the lock exit here,
	// before they take a port or touch the socket.
	release, err := daemon.Lock(sock)
	if errors.Is(err, daemon.ErrLocked) {
		logger.Printf("another daemon holds the lock for %s; exiting", sock)
		return nil
	}
	if err != nil {
		return err
	}
	defer release()

	d := daemon.New(version)
	d.Logf = logger.Printf

	hln, err := daemon.ListenHTTP(addr)
	if err != nil {
		return err
	}
	d.BaseURL = "http://" + hln.Addr().String()
	sln, err := daemon.ListenSocket(sock)
	if err != nil {
		_ = hln.Close()
		return err
	}
	defer os.Remove(sock)

	srv := &http.Server{Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(hln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("http: %v", err)
			d.RequestShutdown()
		}
	}()
	go func() {
		if err := d.ServeSocket(sln); err != nil {
			logger.Printf("socket: %v", err)
			d.RequestShutdown()
		}
	}()
	logger.Printf("ai-contextview %s listening on %s (socket %s)", version, d.BaseURL, sock)
	fmt.Println(d.BaseURL)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	var idleTick <-chan time.Time
	if idle > 0 {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		idleTick = t.C
	}
loop:
	for {
		select {
		case <-sigs:
			break loop
		case <-d.Shutdown():
			break loop
		case <-idleTick:
			if d.Idle() > idle {
				logger.Printf("idle for %s; exiting", idle)
				break loop
			}
		}
	}
	d.RequestShutdown()
	_ = sln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	return nil
}

// ------------------------------------------------------------------- fakeapp

// errFakeappLocked means a daemon already serves the socket.
var errFakeappLocked = errors.New("a daemon already serves the socket; stop it first (ai-contextview serve --stop)")

func runFakeapp(args []string, sock string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("fakeapp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: ai-contextview fakeapp\n\n"+
			"Runs a terminal stand-in for the daemon on %s.\n"+
			"It refuses to start while a daemon serves that socket.\n", sock)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("fakeapp takes no arguments: %q", fs.Args())
	}
	// Like serve, take the daemon lock before touching the socket: Listen
	// removes the socket file, which would cut a running daemon off from its
	// bridges.
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		return err
	}
	release, err := daemon.Lock(sock)
	if errors.Is(err, daemon.ErrLocked) {
		return errFakeappLocked
	}
	if err != nil {
		return err
	}
	defer release()

	s := &fakeapp.Server{
		Socket: sock, BaseURL: "http://fakeapp", Viewers: 1, Out: stdout,
		OnAsk: fakeapp.TerminalAnswerer(stdin, stdout),
	}
	if err := s.Listen(); err != nil {
		return err
	}
	defer os.Remove(s.Socket)
	fmt.Fprintln(stdout, "fakeapp listening on", s.Socket)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return s.Serve(ctx)
}
