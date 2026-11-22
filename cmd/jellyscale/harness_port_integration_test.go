//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// JS-171: the harness chooses a process's address (freeAddr) and the process binds it a moment later, and another socket can take
// it between the two (merge-queue run 44983050791). A start that lost its port is told by what it left: its exit, or its log,
// naming "address already in use", or a socket on its address that is not its own. The harness starts it again on a new address,
// at most launchStarts starts in all; the files here read what the kernel says about sockets.

// launchStarts is the most starts of a process whose address is taken every time before its test fails (docs/testing.md).
const launchStarts = 6

// bindGrace is how long a process may have no socket listening on its address before the harness stops waiting for one: a bind
// takes milliseconds after the process logs that it listens, so past this a process that logged it is a start that failed to bind
// and is stuck on its way out.
const bindGrace = 5 * time.Second

// binds is whether the start is a live process that is to bind its address.
func (p *proc) binds() bool { return p.spec.listen && listens(p.spec.args) && p.alive() }

// listens says whether a process started with these arguments binds --listen: every role set but a worker alone. Without --roles
// it runs them all.
func listens(args []string) bool {
	for i, arg := range args {
		value, ok := strings.CutPrefix(arg, "--roles=")
		if arg == "--roles" && i+1 < len(args) {
			value, ok = args[i+1], true
		}
		if ok {
			return value != "worker" // A worker alone is the one role set that binds nothing; a list is never "worker" alone.
		}
	}
	return true
}

// lostPort is whether the start has said that it could not bind its address.
func (p *proc) lostPort() bool {
	return p.spec.listen && listens(p.spec.args) && strings.Contains(p.log.String(), "address already in use")
}

// restart ends a start that lost its port, with every process it started, and starts the process again on a new address.
func (p *proc) restart() {
	p.t.Helper()
	pid := p.cmd.Process.Pid
	p.t.Logf("a start lost its port: %s was taken (the log has %d lines); starting again", p.addr, strings.Count(p.log.String(), "\n"))
	children := descendants(pid)
	p.kill()
	for _, child := range children {
		syscall.Kill(child, syscall.SIGKILL) //nolint:errcheck
		for end := time.Now().Add(15 * time.Second); processAlive(child) && time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		}
	}
	p.lost = append(p.lost, [2]int64{p.begun.UnixNano(), time.Now().UnixNano()})
	if len(p.lost) >= launchStarts {
		p.t.Fatalf("--listen %s was taken at each of %d starts in a row; the last start's log:\n%s", p.addr, len(p.lost), p.log)
	}
	p.begin()
}

// forgetLostStarts removes the rows in transcode_nodes of the starts that lost their port: such a start exits before it can
// delete its own, and a test that counts the nodes would read them as a process's. A row is a lost start's when it was
// written while that start ran, by a process that is gone.
func (p *proc) forgetLostStarts() {
	p.t.Helper()
	if len(p.lost) == 0 {
		return
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, p.spec.dsn)
	if err != nil {
		p.t.Logf("the rows of the starts that lost their port are not removed: %v", err)
		return
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT id FROM transcode_nodes")
	if err != nil {
		p.t.Logf("the rows of the starts that lost their port are not removed: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			p.t.Fatal(err)
		}
		pidText, nanosText, _ := strings.Cut(id, "-")
		pid, _ := strconv.Atoi(pidText)
		nanos, _ := strconv.ParseInt(nanosText, 10, 64)
		for _, window := range p.lost {
			if nanos >= window[0] && nanos <= window[1] && !processAlive(pid) {
				ids = append(ids, id)
			}
		}
	}
	rows.Close()
	for _, id := range ids {
		if _, err := conn.Exec(ctx, "DELETE FROM transcode_nodes WHERE id = $1", id); err != nil {
			p.t.Fatal(err)
		}
	}
}

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// owner says whose the socket listening on the address is, compared with this process and what it started: ownerNone when no
// socket listens there yet, ownerOwn when a process of this one's does, ownerForeign when another's does.
const (
	ownerNone = iota
	ownerOwn
	ownerForeign
)

func (p *proc) owner() int {
	_, port, _ := strings.Cut(p.addr, ":")
	number, _ := strconv.Atoi(port)
	inode := listeningInode(number)
	if inode == "" {
		return ownerNone
	}
	parents := parentsOfAll()
	result := ownerNone
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !holdsSocket(pid, inode) {
			continue
		}
		result = ownerForeign
		for at := pid; at > 1; at = parents[at] {
			if at == p.cmd.Process.Pid {
				return ownerOwn
			}
		}
	}
	return result
}

// listeningInode is the socket that listens on the port, "" when none does.
func listeningInode(port int) string {
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, _ := os.ReadFile(file)
		for _, line := range strings.Split(string(raw), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			if _, hex, _ := strings.Cut(fields[1], ":"); hex == fmt.Sprintf("%04X", port) {
				return fields[9]
			}
		}
	}
	return ""
}

func holdsSocket(pid int, inode string) bool {
	fds, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	for _, fd := range fds {
		if link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name())); err == nil && link == "socket:["+inode+"]" {
			return true
		}
	}
	return false
}

// parentsOfAll is each process's parent.
func parentsOfAll() map[int]int {
	parents := map[int]int{}
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		// "pid (comm) state ppid ...": comm may hold spaces and parentheses, so read from its last parenthesis.
		text := string(raw)
		if fields := strings.Fields(text[strings.LastIndex(text, ")")+1:]); len(fields) > 1 {
			parents[pid], _ = strconv.Atoi(fields[1])
		}
	}
	return parents
}

// descendants are the processes that pid started and that are still running.
func descendants(pid int) []int {
	parents := parentsOfAll()
	out := []int{}
	for found := true; found; {
		found = false
		for child, parent := range parents {
			if parent == pid || containsInt(out, parent) {
				if !containsInt(out, child) {
					out = append(out, child)
					found = true
				}
			}
		}
	}
	return out
}

func containsInt(list []int, n int) bool {
	for _, x := range list {
		if x == n {
			return true
		}
	}
	return false
}
