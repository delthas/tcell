// Copyright 2021 The TCell Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use file except in compliance with the License.
// You may obtain a copy of the license at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build windows

package tcell

import (
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// conPty is an implementation of the Tty API based upon the Windows PseudoConsole (PTY) API.
type conPty struct {
	in         syscall.Handle
	out        syscall.Handle
	cancelflag syscall.Handle

	oimode uint32
	oomode uint32

	w    int
	h    int
	dimL sync.RWMutex

	cb    func()
	stopQ chan struct{}

	wg sync.WaitGroup
	l  sync.Mutex

	input chan byte
}

func (tty *conPty) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	var ok bool
	b[0], ok = <-tty.input
	if !ok {
		return 0, io.EOF
	}
	var n int
	for n = 1; n < len(b); n++ {
		select {
		case c, ok := <-tty.input:
			if !ok {
				return n, io.EOF
			}
			b[n] = c
		default:
			break
		}
	}
	return n, nil
}

func (tty *conPty) Write(b []byte) (int, error) {
	return os.Stdout.Write(b)
}

func (tty *conPty) Close() error {
	return nil
}

func (tty *conPty) Start() error {
	tty.l.Lock()
	defer tty.l.Unlock()

	in, e := syscall.Open("CONIN$", syscall.O_RDWR, 0)
	if e != nil {
		return e
	}
	tty.in = in
	out, e := syscall.Open("CONOUT$", syscall.O_RDWR, 0)
	if e != nil {
		syscall.Close(tty.in)
		return e
	}
	tty.out = out

	cf, _, e := procCreateEvent.Call(
		uintptr(0),
		uintptr(1),
		uintptr(0),
		uintptr(0))
	if cf == uintptr(0) {
		return e
	}
	tty.cancelflag = syscall.Handle(cf)

	procGetConsoleMode.Call(
		uintptr(tty.in),
		uintptr(unsafe.Pointer(&tty.oimode)))
	procGetConsoleMode.Call(
		uintptr(tty.out),
		uintptr(unsafe.Pointer(&tty.oomode)))

	rv, _, err := procSetConsoleMode.Call(
		uintptr(tty.in),
		uintptr(modeVtInput|modeResizeEn|modeMouseEn))
	if rv == 0 {
		return err
	}
	rv, _, err = procSetConsoleMode.Call(
		uintptr(tty.out),
		uintptr(modeVtOutput|modeNoAutoNL|modeCookedOut))
	if rv == 0 {
		return err
	}

	var info consoleInfo
	procGetConsoleScreenBufferInfo.Call(
		uintptr(tty.out),
		uintptr(unsafe.Pointer(&info)))
	tty.w = int((info.win.right - info.win.left) + 1)
	tty.h = int((info.win.bottom - info.win.top) + 1)

	tty.input = make(chan byte, 16384)
	tty.stopQ = make(chan struct{})
	tty.wg.Add(1)
	go func(stopQ chan struct{}) {
		defer tty.wg.Done()
		for {
			select {
			case <-stopQ:
				return
			default:
			}
			if e := tty.getConsoleInput(); e != nil {
				return
			}
		}
	}(tty.stopQ)
	return nil
}

func (tty *conPty) getConsoleInput() error {
	// cancelFlag comes first as WaitForMultipleObjects returns the lowest index
	// in the event that both events are signalled.
	waitObjects := []syscall.Handle{tty.cancelflag, tty.in}
	// As arrays are contiguous in memory, a pointer to the first object is the
	// same as a pointer to the array itself.
	pWaitObjects := unsafe.Pointer(&waitObjects[0])

	rv, _, er := procWaitForMultipleObjects.Call(
		uintptr(len(waitObjects)),
		uintptr(pWaitObjects),
		uintptr(0),
		w32Infinite)
	// WaitForMultipleObjects returns WAIT_OBJECT_0 + the index.
	switch rv {
	case w32WaitObject0: // tty.cancelFlag
		return errors.New("cancelled")
	case w32WaitObject0 + 1: // tty.in
		records := make([]inputRecord, 128)
		var nrec int32
		rv, _, er := procReadConsoleInput.Call(
			uintptr(tty.in),
			uintptr(unsafe.Pointer(&records[0])),
			uintptr(len(records)),
			uintptr(unsafe.Pointer(&nrec)))
		if rv == 0 {
			return er
		}
		for _, rec := range records[:nrec] {
			switch rec.typ {
			case keyEvent:
				krec := &keyRecord{}
				krec.isdown = geti32(rec.data[0:])
				krec.repeat = getu16(rec.data[4:])
				krec.kcode = getu16(rec.data[6:])
				krec.scode = getu16(rec.data[8:])
				krec.ch = getu16(rec.data[10:])
				krec.mod = getu32(rec.data[12:])

				if krec.isdown == 0 || krec.repeat < 1 {
					// it's a key release event, ignore it
					break
				}
				if krec.ch == 0 {
					// it's not a VT key event, but a windows specific key, eg caps lock press/release
					// ignore it.
					break
				}
				for ; krec.repeat > 0; krec.repeat-- {
					runes := utf16.Decode([]uint16{krec.ch})
					for _, r := range runes {
						buf := make([]byte, 4)
						n := utf8.EncodeRune(buf, r)
						for _, b := range buf[:n] {
							tty.input <- b
						}
					}
				}
				break
			case resizeEvent:
				var rrec resizeRecord
				rrec.x = geti16(rec.data[0:])
				rrec.y = geti16(rec.data[2:])

				tty.dimL.Lock()
				tty.w = int(rrec.x)
				tty.h = int(rrec.y)
				tty.dimL.Unlock()

				tty.l.Lock()
				cb := tty.cb
				tty.l.Unlock()
				if cb != nil {
					cb()
				}
			}
		}
	default:
		return er
	}

	return nil
}

func (tty *conPty) Drain() error {
	return nil
}

func (tty *conPty) Stop() error {
	tty.l.Lock()

	procSetEvent.Call(uintptr(tty.cancelflag))

	rv, _, err := procSetConsoleMode.Call(
		uintptr(tty.in),
		uintptr(tty.oimode))
	if rv == 0 {
		tty.l.Unlock()
		return err
	}
	rv, _, err = procSetConsoleMode.Call(
		uintptr(tty.out),
		uintptr(tty.oomode))
	if rv == 0 {
		tty.l.Unlock()
		return err
	}

	close(tty.stopQ)
	tty.l.Unlock()

	tty.wg.Wait()
	close(tty.input)

	return nil
}

func (tty *conPty) WindowSize() (int, int, error) {
	tty.dimL.RLock()
	defer tty.dimL.RUnlock()

	return tty.w, tty.h, nil
}

func (tty *conPty) NotifyResize(cb func()) {
	tty.l.Lock()
	tty.cb = cb
	tty.l.Unlock()
}

// NewConPty opens the Console PTY.
func NewConPty() (Tty, error) {
	return &conPty{}, nil
}
