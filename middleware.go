package command

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

var (
	_ caddy.Module                = (*Middleware)(nil)
	_ caddy.Provisioner           = (*Middleware)(nil)
	_ caddy.Validator             = (*Middleware)(nil)
	_ caddyhttp.MiddlewareHandler = (*Middleware)(nil)
)

func init() {
	caddy.RegisterModule(Middleware{})
}

// Middleware implements an HTTP handler that runs shell command.
type Middleware struct {
	Cmd
}

// CaddyModule returns the Caddy module information.
func (Middleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.exec",
		New: func() caddy.Module { return new(Middleware) },
	}
}

// Provision implements caddy.Provisioner.
func (m *Middleware) Provision(ctx caddy.Context) error { return m.Cmd.provision(ctx, m) }

// Validate implements caddy.Validator
func (m Middleware) Validate() error { return m.Cmd.validate() }

// ServeHTTP implements caddyhttp.MiddlewareHandler.
func (m Middleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	// replace per-request placeholders
	argv := make([]string, len(m.Args))
	for index, argument := range m.Args {
		argv[index] = repl.ReplaceAll(argument, "")
	}

	if !m.Stream {
		// If foreground mode, collect all output and return it
		if m.Foreground {
			return m.runAndCollectOutput(w, r, argv, next)
		}

		err := m.run(argv)

		if m.PassThru {
			if err != nil {
				m.log.Error(err.Error())
			}

			return next.ServeHTTP(w, r)
		}

		var resp struct {
			Status string `json:"status,omitempty"`
			Error  string `json:"error,omitempty"`
		}

		if err == nil {
			resp.Status = "success"
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			resp.Error = err.Error()
		}

		w.Header().Add("content-type", "application/json")
		return json.NewEncoder(w).Encode(resp)
	}

	// The rest of the function is the new SSE logic
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		m.log.Error("streaming unsupported")
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return nil
	}

	ctx := r.Context()
	if m.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, m.Command, argv...)
	cmd.Dir = m.Directory

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.log.Error("getting stdout pipe", zap.Error(err))
		return err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		m.log.Error("getting stderr pipe", zap.Error(err))
		return err
	}

	err = cmd.Start()
	if err != nil {
		m.log.Error("starting command", zap.String("command", m.Command), zap.Strings("args", argv), zap.Error(err))
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine for stdout
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			fmt.Fprintf(w, "event: stdout\ndata: %s\n\n", scanner.Text())
			flusher.Flush()
		}
	}()

	// Goroutine for stderr
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			fmt.Fprintf(w, "event: stderr\ndata: %s\n\n", scanner.Text())
			flusher.Flush()
		}
	}()

	wg.Wait()

	err = cmd.Wait()
	if err != nil {
		m.log.Error("command finished with error", zap.Error(err))
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
		flusher.Flush()
	}

	// Send a final event to signal completion
	fmt.Fprintf(w, "event: close\ndata: Command finished\n\n")
	flusher.Flush()

	return nil
}

// runAndCollectOutput runs the command in foreground mode, collects all output,
// and returns it to the client in a single response.
func (m Middleware) runAndCollectOutput(w http.ResponseWriter, r *http.Request, argv []string, next caddyhttp.Handler) error {
	if m.PassThru {
		// In pass-thru mode, just run and continue
		err := m.run(argv)
		if err != nil {
			m.log.Error(err.Error())
		}
		return next.ServeHTTP(w, r)
	}

	ctx := r.Context()
	if m.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, m.Command, argv...)
	cmd.Dir = m.Directory

	// Create buffers to collect output
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	// Start and wait for command to complete
	err := cmd.Run()

	// Prepare response with collected output
	var resp struct {
		Status   string `json:"status"`
		Error    string `json:"error,omitempty"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		resp.Error = err.Error()
		resp.Status = "error"
		if exitError, ok := err.(*exec.ExitError); ok {
			resp.ExitCode = exitError.ExitCode()
		} else {
			resp.ExitCode = -1
		}
	} else {
		resp.Status = "success"
		resp.ExitCode = 0
	}

	// Add collected output
	resp.Stdout = stdoutBuf.String()
	resp.Stderr = stderrBuf.String()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	return json.NewEncoder(w).Encode(resp)
}

// Cleanup implements caddy.Cleanup
// TODO: ensure all running processes are terminated.
func (m *Middleware) Cleanup() error {
	return nil
}
