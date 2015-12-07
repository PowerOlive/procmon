package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/yinqiwen/gotoolkit/iotools"
)

var listenFile *os.File

type ProcOutput struct {
	fname string
	log   *iotools.RotateFile
}

func (pout *ProcOutput) reopen() {
	if nil != pout.log {
		pout.log.Close()
	}
	rfile := &iotools.RotateFile{
		MaxBackupIndex:  2,
		MaxFileSize:     1024 * 1024 * 1024,
		SyncBytesPeriod: 64 * 1024,
	}
	err := rfile.Open(pout.fname, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0660)
	if nil != err {
		glog.Errorf("%v", err)
		return
	}
	pout.log = rfile
}

func (pout *ProcOutput) Write(p []byte) (int, error) {
	if nil == pout.log {
		pout.reopen()
	}
	if nil == pout.log {
		return 0, fmt.Errorf("Log file %s not open", pout.fname)
	}
	return pout.log.Write(p)
}

func (pout *ProcOutput) Close() error {
	if nil == pout.log {
		return nil
	}
	err := pout.log.Close()
	if nil == err {
		pout.log = nil
	}
	return err
}

type monitorProc struct {
	processName   string
	args          []string
	env           []string
	logDir        string
	procCmd       *exec.Cmd
	output        *ProcOutput
	autoRestart   bool
	checkCfg      checkConfig
	lastCheckTime int64
	lk            sync.Mutex
}

func (mproc *monitorProc) isRunning() bool {
	mproc.lk.Lock()
	defer mproc.lk.Unlock()
	return mproc.procCmd != nil
}

func (mproc *monitorProc) wait() bool {
	if !mproc.isRunning() {
		return false
	}
	mproc.lk.Lock()
	cmd := mproc.procCmd
	mproc.lk.Unlock()
	cmd.Wait()
	glog.Infof("Process:%s stoped.", mproc.processName)
	mproc.lk.Lock()
	defer mproc.lk.Unlock()
	if cmd == mproc.procCmd {
		mproc.procCmd = nil
	}
	return true
}

func (mproc *monitorProc) kill(wr io.Writer) {
	if mproc.isRunning() {
		mproc.lk.Lock()
		mproc.autoRestart = false
		mproc.procCmd.Process.Kill()
		mproc.lk.Unlock()
		for {
			if !mproc.isRunning() {
				io.WriteString(wr, fmt.Sprintf("Kill process:%s success.\r\n", mproc.processName))
				break
			} else {
				io.WriteString(wr, fmt.Sprintf("Process:%s not killed, wait 1 sec.\r\n", mproc.processName))
				time.Sleep(time.Second)
			}
		}
	} else {
		io.WriteString(wr, fmt.Sprintf("No running process:%s\r\n", mproc.processName))
	}
}

func (mproc *monitorProc) check(wr io.Writer) bool {
	if mproc.autoRestart && !mproc.isRunning() {
		mproc.start(&LogWriter{})
		return true
	}
	if !mproc.isRunning() && !mproc.autoRestart {
		return false
	}
	if len(mproc.checkCfg.Addr) == 0 {
		return false
	}
	now := time.Now().Unix()
	if mproc.lastCheckTime == 0 {
		mproc.lastCheckTime = now
	}
	if now-mproc.lastCheckTime >= int64(mproc.checkCfg.Period) {
		mproc.lastCheckTime = now
		c, err := net.DialTimeout("tcp", mproc.checkCfg.Addr, time.Duration(mproc.checkCfg.Timeout)*time.Second)
		if nil != err {
			mproc.procCmd.Process.Kill()
			glog.Errorf("Kill process:%s since check failed by reason:%v", mproc.processName, err)
			return true
		}
		c.Close()
	}
	return false
}

func (mproc *monitorProc) start(wr io.Writer) {
	if mproc.isRunning() {
		io.WriteString(wr, fmt.Sprintf("Process:%s already started.\r\n", mproc.processName))
		return
	}
	var err error
	mproc.lk.Lock()
	defer mproc.lk.Unlock()
	mproc.procCmd = exec.Command(mproc.processName, mproc.args...)
	mproc.procCmd.Env = append(os.Environ(), mproc.env...)

	if len(mproc.logDir) > 0 {
		var stderrpipe, stdoutpipe io.ReadCloser
		stderrpipe, err = mproc.procCmd.StderrPipe()
		if nil == err {
			stdoutpipe, err = mproc.procCmd.StdoutPipe()
		}
		if nil != err {
			glog.Errorf("%v", err)
		} else {
			outputfileName := fmt.Sprintf("%s/%s.out", mproc.logDir, filepath.Base(mproc.processName))
			if nil != mproc.output {
				mproc.output.Close()
			}
			mproc.output = &ProcOutput{outputfileName, nil}
			go func() {
				io.Copy(mproc.output, stderrpipe)
			}()
			go func() {
				io.Copy(mproc.output, stdoutpipe)
			}()
		}
	}
	err = mproc.procCmd.Start()
	if err != nil {
		mproc.procCmd = nil
		io.WriteString(wr, fmt.Sprintf("Failed to start process:%s for reason:%v\r\n", mproc.processName, err))
		return
	}
	io.WriteString(wr, fmt.Sprintf("Start process:%s %v success.\r\n", mproc.processName, mproc.args))
	mproc.autoRestart = true
	go mproc.wait()
}

type monitorProcTable struct {
	procNames    []string
	monitorProcs map[string]*monitorProc
	mlk          sync.Mutex
}

var procTable *monitorProcTable

func newMonitorProcTable() *monitorProcTable {
	mp := new(monitorProcTable)
	mp.monitorProcs = make(map[string]*monitorProc)
	return mp
}

func buildMonitorProcs() {
	procTable.mlk.Lock()
	procTable.procNames = make([]string, 0)
	for _, proc := range Cfg.Monitor {
		cmd := strings.Fields(proc.Proc)
		mproc, ok := procTable.monitorProcs[proc.Proc]
		procTable.procNames = append(procTable.procNames, cmd[0])
		if !ok {
			mproc = new(monitorProc)
			mproc.processName = cmd[0]
			procTable.monitorProcs[mproc.processName] = mproc
			mproc.autoRestart = true
		}
		mproc.args = cmd[1:]
		mproc.env = proc.Env
		mproc.logDir = proc.LogDir
		mproc.checkCfg = proc.Check
	}
	procTable.mlk.Unlock()
}

func getService(proc string) *monitorProc {
	procTable.mlk.Lock()
	defer procTable.mlk.Unlock()
	if mproc, ok := procTable.monitorProcs[proc]; ok {
		return mproc
	} else {
		return nil
	}
}

func listProcs(wr io.Writer) {
	procTable.mlk.Lock()
	defer procTable.mlk.Unlock()
	wr.Write([]byte("PID   Process	Status\r\n"))
	for _, mproc := range procTable.monitorProcs {
		pid := -1
		status := "stoped"
		if mproc.isRunning() {
			pid = mproc.procCmd.Process.Pid
			status = "running"
		}
		io.WriteString(wr, fmt.Sprintf("%d   %s	%s\r\n", pid, mproc.processName, status))
	}
}

func killService(proc string, wr io.Writer) {
	mproc := getService(proc)
	if nil != mproc {
		mproc.kill(wr)
	} else {
		io.WriteString(wr, fmt.Sprintf("No running process:%s\r\n", proc))
	}
}

var pidFile string = ".pids"

func killAll(wr io.Writer) {
	for _, proc := range procTable.procNames {
		mproc := getService(proc)
		if nil != mproc {
			mproc.kill(wr)
		}
	}
	//os.Remove(pidFile)
	os.Exit(1)
}

func restartService(proc string, wr io.Writer) {
	mproc := getService(proc)
	if nil != mproc {
		mproc.kill(wr)
	}
	mproc.start(wr)
}

func startService(proc string, wr io.Writer) {
	mproc := getService(proc)
	if nil != mproc && nil != mproc.procCmd {
		io.WriteString(wr, fmt.Sprintf("Process:%s already started.\r\n", proc))
		return
	}
	mproc.start(wr)
}

func dumpPids() {
	file, err := os.Create(pidFile)
	if nil != err {
		glog.Error(err)
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "%d\n", os.Getpid())
	procTable.mlk.Lock()
	defer procTable.mlk.Unlock()
	for _, mproc := range procTable.monitorProcs {
		if mproc.isRunning() {
			fmt.Fprintf(file, "%d\n", mproc.procCmd.Process.Pid)
		}
	}
}

func restartSelf(wr io.Writer) {
	for _, proc := range procTable.procNames {
		mproc := getService(proc)
		if nil != mproc {
			mproc.autoRestart = false
			mproc.kill(wr)
		}
	}
	path := os.Args[0]
	args := os.Args[1:]
	hasGracefulFlal := false
	for _, arg := range args {
		if arg == "-graceful" {
			hasGracefulFlal = true
			break
		}
	}
	if !hasGracefulFlal {
		args = append(args, "-graceful")
	}
	fmt.Fprintf(wr, "Restart pmond self.\n")
	cmd := exec.Command(path, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{listenFile}

	err := cmd.Start()
	if err != nil {
		fmt.Fprintf(wr, "gracefulRestart: Failed to launch, error: %v\n", err)
	}
}

func init() {
	procTable = newMonitorProcTable()
	routine := func() {
		dumpPids()
		checkTickChan := time.NewTicker(time.Millisecond * 1000).C
		for {
			select {
			case <-checkTickChan:
				changed := false
				for _, proc := range procTable.procNames {
					mproc := getService(proc)
					if nil != mproc {
						if mproc.check(&LogWriter{}) {
							changed = true
						}
					}
				}
				if changed {
					dumpPids()
				}
			}
		}
	}
	go routine()
}