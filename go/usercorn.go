package main

import (
	"errors"
	"fmt"
	uc "github.com/unicorn-engine/unicorn/bindings/go/unicorn"
	"os"
	"path/filepath"
	"strings"

	"./arch"
	"./loader"
	"./models"
	"./syscalls"
)

type Usercorn struct {
	*Unicorn
	loader       models.Loader
	interpLoader models.Loader

	base       uint64
	interpBase uint64
	entry      uint64
	binEntry   uint64

	StackBase   uint64
	DataSegment models.Segment
	Verbose     bool
	TraceSys    bool
	TraceMem    bool
	TraceExec   bool
	TraceReg    bool
	LoadPrefix  string
	status      models.StatusDiff
	stacktrace  models.Stacktrace

	// deadlock detection
	lastBlock uint64
	lastCode  uint64
	deadlock  int
}

func NewUsercorn(exe string, prefix string) (*Usercorn, error) {
	l, err := loader.LoadFile(exe)
	if err != nil {
		return nil, err
	}
	a, os, err := arch.GetArch(l.Arch(), l.OS())
	if err != nil {
		return nil, err
	}
	unicorn, err := NewUnicorn(a, os, l.ByteOrder())
	if err != nil {
		return nil, err
	}
	ds, de := l.DataSegment()
	u := &Usercorn{
		Unicorn:     unicorn,
		loader:      l,
		LoadPrefix:  prefix,
		DataSegment: models.Segment{ds, de},
	}
	u.status = models.StatusDiff{U: u, Color: true}
	u.interpBase, u.entry, u.base, u.binEntry, err = u.mapBinary(u.loader)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (u *Usercorn) Run(args []string, env []string) error {
	if err := u.addHooks(); err != nil {
		return err
	}
	if err := u.setupStack(); err != nil {
		return err
	}
	if u.OS.Init != nil {
		if err := u.OS.Init(u, args, env); err != nil {
			return err
		}
	}
	if u.Verbose {
		fmt.Fprintf(os.Stderr, "[entry @ 0x%x]\n", u.entry)
		dis, err := u.Disas(u.entry, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		} else {
			fmt.Fprintln(os.Stderr, dis)
		}
		sp, err := u.RegRead(u.arch.SP)
		if err != nil {
			return err
		}
		buf := make([]byte, u.StackBase+STACK_SIZE-sp)
		if err := u.MemReadInto(buf, sp); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "[stack @ 0x%x]\n", sp)
		fmt.Fprintf(os.Stderr, "%s\n", HexDump(sp, buf[:], u.arch))
	}
	if u.Verbose || u.TraceReg {
		u.status.Changes().Print("", true, false)
	}
	if u.Verbose {
		fmt.Fprintln(os.Stderr, "=====================================")
		fmt.Fprintln(os.Stderr, "==== Program output begins here. ====")
		fmt.Fprintln(os.Stderr, "=====================================")
	}
	if u.TraceReg || u.TraceExec {
		sp, _ := u.RegRead(u.arch.SP)
		u.stacktrace.Update(u.entry, sp)
	}
	err := u.Unicorn.Start(u.entry, 0xffffffffffffffff)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Registers:")
		u.status.Changes().Print("", true, false)
		fmt.Fprintln(os.Stderr, "Stacktrace:")
		u.stacktrace.Print(u)
	}
	return err
}

func (u *Usercorn) Loader() models.Loader {
	return u.loader
}

func (u *Usercorn) InterpBase() uint64 {
	// points to interpreter base or 0
	return u.interpBase
}

func (u *Usercorn) Base() uint64 {
	// points to program base
	return u.base
}

func (u *Usercorn) Entry() uint64 {
	// points to effective program entry: either an interpreter or the binary
	return u.entry
}

func (u *Usercorn) BinEntry() uint64 {
	// points to binary entry, even if an interpreter is used
	return u.binEntry
}

func (u *Usercorn) PosixInit(args, env []string, auxv []byte) error {
	// end marker
	if err := u.Push(0); err != nil {
		return err
	}
	// auxv
	if err := u.PushBytes(auxv); err != nil {
		return err
	}
	// envp
	envp, err := u.pushStrings(env...)
	if err != nil {
		return err
	}
	if err := u.pushAddrs(envp); err != nil {
		return err
	}
	// argv
	argv, err := u.pushStrings(args...)
	if err != nil {
		return err
	}
	if err := u.pushAddrs(argv); err != nil {
		return err
	}
	// argc
	return u.Push(uint64(len(args)))
}

func (u *Usercorn) PrefixPath(path string, force bool) string {
	if filepath.IsAbs(path) && u.LoadPrefix != "" {
		target := filepath.Join(u.LoadPrefix, path)
		_, err := os.Stat(target)
		exists := !os.IsNotExist(err)
		if force || exists {
			return target
		}
	}
	return path
}

func (u *Usercorn) Symbolicate(addr uint64) (string, error) {
	var symbolicate = func(addr uint64, symbols []models.Symbol) (result models.Symbol, distance uint64) {
		if len(symbols) == 0 {
			return
		}
		nearest := make(map[uint64][]models.Symbol)
		var min int64 = -1
		for _, sym := range symbols {
			dist := int64(addr - sym.Start)
			if dist > 0 && (sym.Start+uint64(dist) <= sym.End || sym.End == 0) && sym.Name != "" {
				if dist < min || min == -1 {
					min = dist
				}
				nearest[uint64(dist)] = append(nearest[uint64(dist)], sym)
			}
		}
		if len(nearest) > 0 {
			sym := nearest[uint64(min)][0]
			return sym, uint64(min)
		}
		return
	}
	symbols, _ := u.loader.Symbols()
	var interpSym []models.Symbol
	if u.interpLoader != nil {
		interpSym, _ = u.interpLoader.Symbols()
	}
	sym, sdist := symbolicate(addr-u.base, symbols)
	isym, idist := symbolicate(addr-u.interpBase, interpSym)
	if idist < sdist && isym.Name != "" || sym.Name == "" {
		sym = isym
		sdist = idist
	}
	if sym.Name != "" {
		return fmt.Sprintf("%s+0x%x", sym.Name, sdist), nil
	}
	return "", nil
}

func (u *Usercorn) Brk(addr uint64) (uint64, error) {
	// TODO: this is linux specific
	s := u.DataSegment
	if addr > 0 {
		u.MemMap(s.End, addr)
		s.End = addr
	}
	return s.End, nil
}

func (u *Usercorn) addHooks() error {
	if u.TraceExec || u.TraceReg {
		u.HookAdd(uc.HOOK_BLOCK, func(_ uc.Unicorn, addr uint64, size uint32) {
			if sp, err := u.RegRead(u.arch.SP); err == nil {
				u.stacktrace.Update(addr, sp)
			}
			indent := strings.Repeat("  ", u.stacktrace.Len())
			blockIndent := indent[:len(indent)-2]
			sym, _ := u.Symbolicate(addr)
			if sym != "" {
				sym = " (" + sym + ")"
			}
			blockLine := fmt.Sprintf("\n"+blockIndent+"+ block%s @0x%x", sym, addr)
			if !u.TraceExec && u.TraceReg && u.deadlock == 0 {
				changes := u.status.Changes()
				if changes.Count() > 0 {
					fmt.Fprintln(os.Stderr, blockLine)
					changes.Print(indent, true, true)
				}
			} else {
				fmt.Fprintln(os.Stderr, blockLine)
			}
			u.lastBlock = addr
		})
	}
	if u.TraceExec {
		u.HookAdd(uc.HOOK_CODE, func(_ uc.Unicorn, addr uint64, size uint32) {
			indent := strings.Repeat("  ", u.stacktrace.Len())
			var changes *models.Changes
			if addr == u.lastCode || u.TraceReg && u.TraceExec {
				changes = u.status.Changes()
			}
			if u.TraceExec {
				dis, _ := u.Disas(addr, uint64(size))
				fmt.Fprintf(os.Stderr, "%s", indent+dis)
				if !u.TraceReg || changes.Count() == 0 {
					fmt.Fprintln(os.Stderr)
				} else {
					dindent := ""
					// TODO: I can count the max dis length in the block and reuse it here
					pad := 40 - len(dis)
					if pad > 0 {
						dindent = strings.Repeat(" ", pad)
					}
					changes.Print(dindent, true, true)
				}
			}
			if addr == u.lastCode {
				u.deadlock++
				if changes.Count() > 0 {
					if u.TraceReg {
						changes.Print(indent, true, true)
					}
					u.deadlock = 0
				}
				if u.deadlock > 2 {
					sym, _ := u.Symbolicate(addr)
					if sym != "" {
						sym = " (" + sym + ")"
					}
					fmt.Fprintf(os.Stderr, "FATAL: deadlock detected at 0x%x%s\n", addr, sym)
					changes.Print(indent, true, false)
					u.Stop()
				}
			} else {
				u.deadlock = 0
			}
			u.lastCode = addr
		})
	}
	if u.TraceMem {
		hexFmt := fmt.Sprintf("0x%%0%dx", u.Bsz*2)
		memFmt := fmt.Sprintf("%%s %s %%d %s\n", hexFmt, hexFmt)
		u.HookAdd(uc.HOOK_MEM_READ|uc.HOOK_MEM_WRITE, func(_ uc.Unicorn, access int, addr uint64, size int, value int64) {
			indent := strings.Repeat("  ", u.stacktrace.Len()-1)
			var letter string
			if access == uc.MEM_WRITE {
				letter = "W"
			} else {
				letter = "R"
			}
			fmt.Fprintf(os.Stderr, indent+memFmt, letter, addr, size, value)
		})
	}
	invalid := uc.HOOK_MEM_READ_INVALID | uc.HOOK_MEM_WRITE_INVALID | uc.HOOK_MEM_FETCH_INVALID
	u.HookAdd(invalid, func(_ uc.Unicorn, access int, addr uint64, size int, value int64) bool {
		switch access {
		case uc.MEM_WRITE_INVALID:
			fmt.Fprintf(os.Stderr, "invalid write")
		case uc.MEM_READ_INVALID:
			fmt.Fprintf(os.Stderr, "invalid prot")
		case uc.MEM_FETCH_INVALID:
			fmt.Fprintf(os.Stderr, "invalid fetch")
		default:
			fmt.Fprintf(os.Stderr, "unknown memory error")
		}
		fmt.Fprintf(os.Stderr, ": @0x%x, 0x%x = 0x%x\n", addr, size, value)
		return false
	})
	u.HookAdd(uc.HOOK_INTR, func(_ uc.Unicorn, intno uint32) {
		u.OS.Interrupt(u, intno)
	})
	return nil
}

func (u *Usercorn) mapBinary(l models.Loader) (interpBase, entry, base, realEntry uint64, err error) {
	var dynamic bool
	switch l.Type() {
	case loader.EXEC:
		dynamic = false
	case loader.DYN:
		dynamic = true
	default:
		err = errors.New("Unsupported file load type.")
		return
	}
	segments, err := l.Segments()
	if err != nil {
		return
	}
	// merge overlapping segments
	merged := make([]*models.Segment, 0, len(segments))
outer:
	for _, seg := range segments {
		addr, size := align(seg.Addr, seg.Size, true)
		s := &models.Segment{addr, addr + size}
		for _, s2 := range merged {
			if s2.Overlaps(s) {
				s2.Merge(s)
				continue outer
			}
		}
		merged = append(merged, s)
	}
	// map merged segments
	var loadBias uint64
	for _, seg := range merged {
		size := seg.End - seg.Start
		if dynamic && seg.Start == 0 && loadBias == 0 {
			loadBias, err = u.Mmap(0x1000000, size)
		} else {
			err = u.MemMap(loadBias+seg.Start, seg.End-seg.Start)
		}
		if err != nil {
			return
		}
	}
	// write segment memory
	for _, seg := range segments {
		if err = u.MemWrite(loadBias+seg.Addr, seg.Data); err != nil {
			return
		}
	}
	entry = loadBias + l.Entry()
	// load interpreter if present
	interp := l.Interp()
	if interp != "" {
		var bin models.Loader
		bin, err = loader.LoadFile(u.PrefixPath(interp, true))
		if err != nil {
			return
		}
		u.interpLoader = bin
		_, _, interpBias, interpEntry, err := u.mapBinary(bin)
		return interpBias, interpEntry, loadBias, entry, err
	} else {
		return 0, entry, loadBias, entry, nil
	}
}

func (u *Usercorn) setupStack() error {
	stack, err := u.Mmap(STACK_BASE, STACK_SIZE)
	if err != nil {
		return err
	}
	u.StackBase = stack
	if err := u.RegWrite(u.arch.SP, stack+STACK_SIZE); err != nil {
		return err
	}
	return nil
}

func (u *Usercorn) pushStrings(args ...string) ([]uint64, error) {
	// TODO: does anything case if these are actually on the stack?
	argvSize := 0
	for _, v := range args {
		argvSize += len(v) + 1
	}
	argvAddr, err := u.Mmap(0, uint64(argvSize))
	if err != nil {
		return nil, err
	}
	buf := make([]byte, argvSize)
	addrs := make([]uint64, 0, len(args)+1)
	var pos uint64
	for i := len(args) - 1; i >= 0; i-- {
		copy(buf[pos:], []byte(args[i]))
		addrs = append(addrs, argvAddr+pos)
		pos += uint64(len(args[i]) + 1)
	}
	u.MemWrite(argvAddr, buf)
	return addrs, nil
}

func (u *Usercorn) pushAddrs(addrs []uint64) error {
	if err := u.Push(0); err != nil {
		return err
	}
	for _, v := range addrs {
		if err := u.Push(v); err != nil {
			return err
		}
	}
	return nil
}

func (u *Usercorn) Syscall(num int, name string, getArgs func(n int) ([]uint64, error)) (uint64, error) {
	if name == "" {
		panic(fmt.Sprintf("Syscall missing: %d", num))
	}
	if u.TraceSys && (u.TraceExec || u.TraceReg) {
		fmt.Fprintf(os.Stderr, strings.Repeat("  ", u.stacktrace.Len()-1)+"s ")
	}
	return syscalls.Call(u, num, name, getArgs, u.TraceSys)
}
