package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/agentswitch-org/ax/internal/config"
	"github.com/agentswitch-org/ax/internal/follow"
	hostreg "github.com/agentswitch-org/ax/internal/hosts"
	"github.com/agentswitch-org/ax/internal/session"
)

type remoteReadOnceFunc func(context.Context, config.Host, []string) ([]readRow, error)
type remoteReadStreamFunc func(context.Context, config.Host, []string, chan<- federatedReadEvent)

var runRemoteReadOnce remoteReadOnceFunc = defaultRemoteReadOnce
var streamRemoteRead remoteReadStreamFunc = defaultStreamRemoteRead

type federatedReadEvent struct {
	host  string
	event follow.Event
	err   error
}

func readFederated(args []string) bool {
	for _, arg := range args {
		if arg == "--hosts" || arg == "--federated" {
			return true
		}
	}
	return false
}

func stripReadFederatedFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--hosts" || arg == "--federated" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func readHasHostFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--host" {
			return true
		}
	}
	return false
}

func (a App) readFederated(args []string) {
	args = stripReadFederatedFlags(args)
	if readHasHostFlag(args) {
		fmt.Fprintln(os.Stderr, "ax: read --hosts cannot be combined with --host")
		os.Exit(2)
	}
	group, rest := GroupArg(args)
	if group == "" {
		fmt.Fprintln(os.Stderr, "ax: read --hosts requires --run")
		os.Exit(2)
	}
	o, id := parseReadArgs(rest)
	if id != "" {
		fmt.Fprintln(os.Stderr, "ax: read --hosts reads runs; pass --run without a positional id")
		os.Exit(2)
	}
	if o.format == "text" {
		fmt.Fprintln(os.Stderr, "ax: read --hosts requires --format json")
		os.Exit(2)
	}

	cfg, _ := config.Load()
	hosts := hostreg.Merge(cfg.Hosts)
	remoteArgs := append([]string{"--run", group}, rest...)
	if o.follow {
		a.readFederatedFollow(cfg, group, o, remoteArgs, hosts)
		return
	}
	remoteArgs = append(remoteArgs, "--identity")
	a.readFederatedOnce(cfg, group, o, remoteArgs, hosts)
}

func (a App) readFederatedOnce(cfg config.Config, group string, o readOpts, remoteArgs []string, hosts []config.Host) {
	rows, err := readRows(cfg, session.Index(cfg), "", group, o)
	if errors.Is(err, errReadNoMatch) {
		rows = nil
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "ax:", err)
		os.Exit(1)
	}
	for i := range rows {
		stampReadRow("local", group, &rows[i])
	}

	type result struct {
		idx  int
		rows []readRow
		err  error
	}
	results := make(chan result, len(hosts))
	for i, h := range hosts {
		i, h := i, h
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), remoteVerbTimeout)
			defer cancel()
			rr, err := runRemoteReadOnce(ctx, h, remoteArgs)
			results <- result{idx: i, rows: rr, err: err}
		}()
	}
	remote := make([][]readRow, len(hosts))
	for range hosts {
		r := <-results
		if r.err != nil {
			fmt.Fprintf(os.Stderr, "ax: host %s: read failed: %v\n", hosts[r.idx].Name, r.err)
			continue
		}
		remote[r.idx] = r.rows
	}
	for i, rr := range remote {
		for _, row := range rr {
			stampReadRow(hosts[i].Name, group, &row)
			rows = append(rows, row)
		}
	}

	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "ax: no matching session")
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return
		}
	}
}

func (a App) readFederatedFollow(cfg config.Config, group string, o readOpts, remoteArgs []string, hosts []config.Host) {
	ctx := context.Background()
	var cancel context.CancelFunc = func() {}
	if o.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
	}
	defer cancel()

	events := make(chan federatedReadEvent, len(hosts)+1)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		local := a.readFollowEvents(ctx, cfg, session.Index(cfg), "", group, o)
		for ev := range local {
			select {
			case events <- federatedReadEvent{host: "local", event: ev}:
			case <-ctx.Done():
				return
			}
		}
	}()

	for _, h := range hosts {
		h := h
		wg.Add(1)
		go func() {
			defer wg.Done()
			streamRemoteRead(ctx, h, remoteArgs, events)
		}()
	}

	go func() {
		wg.Wait()
		close(events)
	}()

	enc := json.NewEncoder(os.Stdout)
	n := 0
	canceling := false
	for item := range events {
		if item.err != nil {
			if !canceling && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "ax: host %s: read failed: %v\n", item.host, item.err)
			}
			continue
		}
		if canceling {
			continue
		}
		ev := item.event
		stampFollowEvent(item.host, group, &ev)
		if err := enc.Encode(ev); err != nil {
			canceling = true
			cancel()
			continue
		}
		n++
		if o.limit > 0 && n >= o.limit {
			canceling = true
			cancel()
		}
	}
}

func stampReadRow(host, group string, row *readRow) {
	row.Host = host
	if row.Group == "" {
		row.Group = group
	}
	row.ID = qualifyFederatedID(host, row.ID)
}

func stampFollowEvent(host, group string, ev *follow.Event) {
	ev.Host = host
	if ev.Group == "" {
		ev.Group = group
	}
	ev.ID = qualifyFederatedID(host, ev.ID)
}

func qualifyFederatedID(host, id string) string {
	if host == "" || host == "local" || strings.Contains(id, "/") {
		return id
	}
	return host + "/" + id
}

func defaultRemoteReadOnce(ctx context.Context, h config.Host, args []string) ([]readRow, error) {
	if strings.TrimSpace(h.Transport) == "" {
		return nil, fmt.Errorf("offline")
	}
	prog, argv := remoteArgv(h, "read", args, false)
	cmd := exec.CommandContext(ctx, prog, argv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if isNoMatchingRead(err, out, stderr.Bytes()) {
			return nil, nil
		}
		return nil, remoteReadError(ctx, stderr.Bytes(), err)
	}
	return decodeReadRows(out)
}

func defaultStreamRemoteRead(ctx context.Context, h config.Host, args []string, out chan<- federatedReadEvent) {
	sendErr := func(err error) {
		select {
		case out <- federatedReadEvent{host: h.Name, err: err}:
		case <-ctx.Done():
		}
	}
	if strings.TrimSpace(h.Transport) == "" {
		sendErr(fmt.Errorf("offline"))
		return
	}

	// Use the one-shot argv shape so ssh transports get BatchMode and
	// ConnectTimeout, but let the caller's context control the stream lifetime.
	prog, argv := remoteArgv(h, "read", args, false)
	cmd := exec.CommandContext(ctx, prog, argv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sendErr(err)
		return
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		sendErr(err)
		return
	}

	dec := json.NewDecoder(stdout)
	for {
		var ev follow.Event
		if err := dec.Decode(&ev); err != nil {
			if err != io.EOF && ctx.Err() == nil {
				sendErr(err)
			}
			break
		}
		select {
		case out <- federatedReadEvent{host: h.Name, event: ev}:
		case <-ctx.Done():
			return
		}
	}
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		if isNoMatchingRead(err, nil, stderr.Bytes()) {
			return
		}
		sendErr(remoteReadError(ctx, stderr.Bytes(), err))
	}
}

func decodeReadRows(out []byte) ([]readRow, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	var rows []readRow
	for {
		var row readRow
		if err := dec.Decode(&row); err != nil {
			if err == io.EOF {
				return rows, nil
			}
			return nil, err
		}
		rows = append(rows, row)
	}
}

func isNoMatchingRead(err error, stdout, stderr []byte) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 || len(bytes.TrimSpace(stdout)) != 0 {
		return false
	}
	return strings.Contains(string(stderr), "no matching session")
}

func remoteReadError(ctx context.Context, stderr []byte, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("transport timed out")
	}
	msg := strings.TrimSpace(string(stderr))
	if msg != "" {
		return fmt.Errorf("%s", msg)
	}
	return err
}
