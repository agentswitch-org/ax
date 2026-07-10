//go:build windows

package conpty

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Pty is a live pseudoconsole with one program attached: the Windows
// equivalent of the pty master the unix holder owns. Read streams the
// program's rendered output; Write feeds it keystrokes; Resize is the
// TIOCSWINSZ analog (there is no SIGWINCH: ConPTY itself repaints the
// program on a size change).
type Pty struct {
	hpc  windows.Handle // the pseudoconsole (HPCON)
	in   *os.File       // holder -> console: client keystrokes
	out  *os.File       // console -> holder: rendered program output
	proc windows.Handle // the attached program's process handle
	job  windows.Handle // kill-on-close job confining the program's tree (0 if unavailable)
	pid  int

	mu         sync.Mutex
	rows, cols uint16
	closed     bool
}

// Options tunes Start. Zero values mean: inherit this process's environment
// and working directory, seed the console at the holder's default 120x40.
type Options struct {
	Dir        string
	Env        []string // nil inherits; non-nil is the exact environment
	Rows, Cols uint16
}

// Start creates a pseudoconsole sized per opts and spawns argv attached to it
// (STARTUPINFOEX carrying PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, the only way a
// process attaches to a ConPTY; os/exec cannot express it). The returned Pty
// owns the program the way the unix holder owns the harness under its pty.
func Start(argv []string, opts Options) (*Pty, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("conpty: empty argv")
	}
	rows, cols := opts.Rows, opts.Cols
	if rows == 0 {
		rows = defaultRows
	}
	if cols == 0 {
		cols = defaultCols
	}

	// Two anonymous pipes: ConPTY reads the program's input from one end and
	// writes rendered output to the other; the holder keeps the far ends.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("conpty: input pipe: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, fmt.Errorf("conpty: output pipe: %w", err)
	}
	closeAll := func() {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
	}

	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(coord(rows, cols), windows.Handle(inR.Fd()), windows.Handle(outW.Fd()), 0, &hpc); err != nil {
		closeAll()
		return nil, fmt.Errorf("conpty: CreatePseudoConsole: %w", err)
	}

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		closeAll()
		return nil, fmt.Errorf("conpty: attribute list: %w", err)
	}
	defer attrs.Delete()
	// The attribute value IS the HPCON itself (passed as the pointer, per the
	// CreatePseudoConsole sample), not a pointer to it; the reinterpret keeps
	// the uintptr->Pointer conversion out of vet's sight.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&hpc)), unsafe.Sizeof(hpc)); err != nil {
		windows.ClosePseudoConsole(hpc)
		closeAll()
		return nil, fmt.Errorf("conpty: pseudoconsole attribute: %w", err)
	}

	siEx := new(windows.StartupInfoEx)
	siEx.Cb = uint32(unsafe.Sizeof(*siEx))
	siEx.ProcThreadAttributeList = attrs.List()
	// USESTDHANDLES with the handles left null. Without it, a holder whose own
	// stdio is redirected (sshd, CI: no console) hits CreateProcess's std-handle
	// duplication, and the program's stdio is the holder's pipes instead of the
	// pseudoconsole (microsoft/terminal#11276); its output then bypasses ConPTY
	// entirely. Null handles suppress the duplication, and the program's console
	// init binds stdio to the ConPTY.
	siEx.Flags |= windows.STARTF_USESTDHANDLES

	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(argv))
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		closeAll()
		return nil, fmt.Errorf("conpty: command line: %w", err)
	}
	var dir *uint16
	if opts.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(opts.Dir); err != nil {
			windows.ClosePseudoConsole(hpc)
			closeAll()
			return nil, fmt.Errorf("conpty: dir: %w", err)
		}
	}
	block, err := envBlock(opts.Env)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		closeAll()
		return nil, err
	}
	var env *uint16
	if block != nil {
		env = &block[0]
	}

	var pi windows.ProcessInformation
	// CREATE_SUSPENDED: the program is confined to the kill-on-close job below
	// BEFORE its first instruction, so nothing it spawns can escape the job by
	// racing the assignment. Children join the job automatically.
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)
	if err := windows.CreateProcess(nil, cmdline, nil, nil, false, flags, env, dir, &siEx.StartupInfo, &pi); err != nil {
		windows.ClosePseudoConsole(hpc)
		closeAll()
		return nil, fmt.Errorf("conpty: CreateProcess: %w", err)
	}
	// The job object is the teardown authority for the program's whole tree:
	// KILL_ON_JOB_CLOSE means the tree dies when the last job handle closes,
	// which the OS does for us even when this holder is TerminateProcess'd (an
	// `ax kill`) and no defer ever runs — the Windows twin of the unix wrapper's
	// process-group SIGKILL, with no orphan window. Best-effort: without a job
	// (assignment denied by policy) the run still works, ConPTY teardown alone
	// takes the console-attached processes down.
	job := killOnCloseJob()
	if job != 0 {
		if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
			windows.CloseHandle(job)
			job = 0
		}
	}
	windows.ResumeThread(pi.Thread)
	windows.CloseHandle(pi.Thread)
	// ConPTY holds its own references to the console-side pipe ends; ours
	// must go now or the holder never sees EOF after Close.
	inR.Close()
	outW.Close()

	return &Pty{
		hpc:  hpc,
		in:   inW,
		out:  outR,
		proc: pi.Process,
		pid:  int(pi.ProcessId),
		job:  job,
		rows: rows,
		cols: cols,
	}, nil
}

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
)

// EnableCtrlC clears an inherited ctrl-c-ignore attribute on this process
// (SetConsoleCtrlHandler(NULL, FALSE)). A process created with
// CREATE_NEW_PROCESS_GROUP — which is how sshd runs a non-pty command — has
// ctrl-c disabled, and every descendant inherits that at creation, INCLUDING
// the harness this holder attaches to its pseudoconsole. With the flag
// inherited, the 0x03 an `ax send --interrupt` feeds the ConPTY raises a
// CTRL_C_EVENT every process ignores (verified on win01: interrupt worked
// from an interactive launch and silently did nothing from an ssh-exec one).
// The holder clears its own flag before spawning, so the harness starts with
// ctrl-c handling enabled no matter what launched ax.
func EnableCtrlC() {
	procSetConsoleCtrlHandler.Call(0, 0)
}

// killOnCloseJob creates an anonymous job object whose last-handle close kills
// every process still in it, or 0 when jobs are unavailable (the caller then
// runs unconfined, as before jobs were wired in).
func killOnCloseJob() windows.Handle {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0
	}
	return job
}

// Read streams the console's rendered output. EOF arrives only after Close
// (conhost keeps the pipe open across the program's exit); use Wait to learn
// the program ended.
func (p *Pty) Read(b []byte) (int, error) { return p.out.Read(b) }

// Write feeds keystrokes to the program, VT-encoded exactly as a terminal
// would send them.
func (p *Pty) Write(b []byte) (int, error) { return p.in.Write(b) }

// Pid is the attached program's process id, for the holder's liveness records.
func (p *Pty) Pid() int { return p.pid }

// Resize applies a client's terminal size to the pseudoconsole. ConPTY
// repaints the program itself on a real change; there is no SIGWINCH to relay.
func (p *Pty) Resize(rows, cols uint16) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("conpty: closed")
	}
	if err := windows.ResizePseudoConsole(p.hpc, coord(rows, cols)); err != nil {
		return fmt.Errorf("conpty: resize: %w", err)
	}
	p.rows, p.cols = rows, cols
	return nil
}

// Nudge forces a repaint when a reattach's size did not change (so Resize
// would be a no-op): a one-column wiggle and back, the ConPTY substitute for
// the unix holder's kill(-pid, SIGWINCH).
func (p *Pty) Nudge() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	wiggle := p.cols - 1
	if p.cols <= 2 {
		wiggle = p.cols + 1
	}
	windows.ResizePseudoConsole(p.hpc, coord(p.rows, wiggle))
	windows.ResizePseudoConsole(p.hpc, coord(p.rows, p.cols))
}

// Wait blocks until the program exits and returns its exit code, the ConPTY
// replacement for cmd.Wait (the process was spawned by hand, so there is no
// exec.Cmd to wait on).
func (p *Pty) Wait() (int, error) {
	if _, err := windows.WaitForSingleObject(p.proc, windows.INFINITE); err != nil {
		return -1, fmt.Errorf("conpty: wait: %w", err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(p.proc, &code); err != nil {
		return -1, fmt.Errorf("conpty: exit code: %w", err)
	}
	return int(code), nil
}

// Kill terminates the attached program and, via its job, everything it
// spawned (no signal exists to forward, and CTRL_CLOSE_EVENT is only sent by
// Close tearing the console down). Without a job it falls back to terminating
// the direct child alone.
func (p *Pty) Kill() error {
	if p.job != 0 {
		if err := windows.TerminateJobObject(p.job, 1); err == nil {
			return nil
		}
	}
	if err := windows.TerminateProcess(p.proc, 1); err != nil {
		return fmt.Errorf("conpty: kill: %w", err)
	}
	return nil
}

// Close tears the pseudoconsole down and, as a ConPTY-inherent side effect,
// terminates the attached process tree. Order per the ClosePseudoConsole
// docs: stop feeding input, close the pseudoconsole while the caller's read
// loop keeps draining (pre-24H2 it blocks until drained), then release the
// holder's pipe ends, whose EOF ends that read loop. Idempotent.
func (p *Pty) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	p.in.Close()
	windows.ClosePseudoConsole(p.hpc)
	p.out.Close()
	windows.CloseHandle(p.proc)
	if p.job != 0 {
		// Last job handle: KILL_ON_JOB_CLOSE reaps anything still alive in the
		// tree (a straggler that detached from the console), so no run leaves
		// orphans behind.
		windows.CloseHandle(p.job)
	}
	return nil
}

// coord packs a rows/cols pair into ConPTY's COORD (X is columns, Y is rows).
func coord(rows, cols uint16) windows.Coord {
	return windows.Coord{X: clampDim(cols), Y: clampDim(rows)}
}
