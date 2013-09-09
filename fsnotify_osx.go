// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin,cgo

package fsnotify

/*
#cgo LDFLAGS: -framework CoreServices
#include <CoreServices/CoreServices.h>
#include <sys/stat.h>

static CFArrayRef ArrayCreateMutable(int len) {
	return CFArrayCreateMutable(NULL, len, &kCFTypeArrayCallBacks);
}

extern void fsevtCallback(FSEventStreamRef p0, void * info, size_t p1, char** p2, FSEventStreamEventFlags* p3, FSEventStreamEventId* p4);

static FSEventStreamRef EventStreamCreate(FSEventStreamContext * context, CFArrayRef paths, FSEventStreamEventId since, CFTimeInterval latency) {
	return FSEventStreamCreate(NULL, (FSEventStreamCallback) fsevtCallback, context, paths, since, latency, 0);
}
*/
import "C"

import "unsafe"
import "path/filepath"
import "sync"
import "os"
import "time"



// FSEventStreamEventFlags
const (
	EventFlagNone              = C.kFSEventStreamEventFlagNone              //    00
	EventFlagMustScanSubDirs   = C.kFSEventStreamEventFlagMustScanSubDirs   //    01
	EventFlagUserDropped       = C.kFSEventStreamEventFlagUserDropped       //    02
	EventFlagKernelDropped     = C.kFSEventStreamEventFlagKernelDropped     //    04
	EventFlagEventIdsWrapped   = C.kFSEventStreamEventFlagEventIdsWrapped   //    08
	EventFlagHistoryDone       = C.kFSEventStreamEventFlagHistoryDone       //    10
	EventFlagRootChanged       = C.kFSEventStreamEventFlagRootChanged       //    20
	EventFlagMount             = C.kFSEventStreamEventFlagMount             //    40
	EventFlagUnMount           = C.kFSEventStreamEventFlagUnmount           //    80
	EventFlagItemCreated       = C.kFSEventStreamEventFlagItemCreated       //   100
	EventFlagItemRemoved       = C.kFSEventStreamEventFlagItemRemoved       //   200
	EventFlagItemInodeMetaMod  = C.kFSEventStreamEventFlagItemInodeMetaMod  //   400
	EventFlagItemRenamed       = C.kFSEventStreamEventFlagItemRenamed       //   800
	EventFlagItemModified      = C.kFSEventStreamEventFlagItemModified      //  1000
	EventFlagItemFinderInfoMod = C.kFSEventStreamEventFlagItemFinderInfoMod //  2000
	EventFlagItemChangeOwner   = C.kFSEventStreamEventFlagItemChangeOwner   //  4000
	EventFlagItemXattrMod      = C.kFSEventStreamEventFlagItemXattrMod      //  8000
	EventFlagItemIsFile        = C.kFSEventStreamEventFlagItemIsFile        // 10000
	EventFlagItemIsDir         = C.kFSEventStreamEventFlagItemIsDir         // 20000
	EventFlagItemIsSymlink     = C.kFSEventStreamEventFlagItemIsSymlink     // 40000
)

//export fsevtCallback
func fsevtCallback(stream C.FSEventStreamRef, info unsafe.Pointer, numEvents C.size_t, paths **C.char, flags *C.FSEventStreamEventFlags, ids *C.FSEventStreamEventId) {
//	events := make([]FileEvent, int(numEvents))
	evtC := *(*chan *FileEvent)(info)

	for i := 0; i < int(numEvents); i++ {
		cpaths := uintptr(unsafe.Pointer(paths)) + (uintptr(i) * unsafe.Sizeof(*paths))
		cpath := *(**C.char)(unsafe.Pointer(cpaths))

		cflags := uintptr(unsafe.Pointer(flags)) + (uintptr(i) * unsafe.Sizeof(*flags))
		cflag := *(*C.FSEventStreamEventFlags)(unsafe.Pointer(cflags))

		cids := uintptr(unsafe.Pointer(ids)) + (uintptr(i) * unsafe.Sizeof(*ids))
		cid := *(*C.FSEventStreamEventId)(unsafe.Pointer(cids))

		evtC <- &FileEvent{Name: C.GoString(cpath), flags: uint32(cflag), id: uint64(cid)}
	}
}

type FileEvent struct {
	Name  string // File name (optional)
	flags uint32
	id    uint64
}

// IsCreate reports whether the FileEvent was triggerd by a creation
func (e *FileEvent) IsCreate() bool { return (e.flags & EventFlagItemCreated) == EventFlagItemCreated}

// IsDelete reports whether the FileEvent was triggerd by a delete
func (e *FileEvent) IsDelete() bool { return (e.flags & EventFlagItemRemoved) == EventFlagItemRemoved}

// IsModify reports whether the FileEvent was triggerd by a file modification
func (e *FileEvent) IsModify() bool {
	return (e.flags & EventFlagItemModified) == EventFlagItemModified
}

// IsRename reports whether the FileEvent was triggerd by a change name
func (e *FileEvent) IsRename() bool { return (e.flags & EventFlagItemRenamed) == EventFlagItemRenamed }



type Watcher struct {
	rlref  C.CFRunLoopRef
	mu            sync.Mutex          // Mutex for the Watcher itself.
	watches       map[string]C.FSEventStreamRef      // key: path; one entry per 'watch'. maps to stream that watches path
	wmut          sync.Mutex          // Protects access to watches.
	fsnFlags      map[string]uint32   // Map of watched files to flags used for filter
	fsnmut        sync.Mutex          // Protects access to fsnFlags.
	enFlags       map[string]uint32   // Map of watched files to evfilt note flags used in kqueue
	enmut         sync.Mutex          // Protects access to enFlags.
	paths         map[int]string      // Map of watched paths (key: watch descriptor)
	finfo         map[int]os.FileInfo // Map of file information (isDir, isReg; key: watch descriptor)
	pmut          sync.Mutex          // Protects access to paths and finfo.
	fileExists    map[string]bool     // Keep track of if we know this file exists (to stop duplicate create events)
	femut         sync.Mutex          // Protects access to fileExists.
	Error         chan error          // Errors are sent on this channel
	internalEvent chan *FileEvent     // Events are queued on this channel
	Event         chan *FileEvent     // Events are returned on this channel
	done          chan bool           // Channel for sending a "quit message" to the reader goroutine
	isClosed      bool                // Set to true when Close() is first called
	bufmut        sync.Mutex          // Protects access to kbuf.
}


func NewWatcher() (*Watcher, error) {
	w := &Watcher{
		watches:       make(map[string]C.FSEventStreamRef),
		fsnFlags:      make(map[string]uint32),
		enFlags:       make(map[string]uint32),
		paths:         make(map[int]string),
		finfo:         make(map[int]os.FileInfo),
		fileExists:    make(map[string]bool),
		internalEvent: make(chan *FileEvent),
		Event:         make(chan *FileEvent),
		Error:         make(chan error),
		done:          make(chan bool, 1),
	}

	w.rlref = C.CFRunLoopGetCurrent()		// XXX: accessing the 'current' runloop. This will cause problems with multiple watchers in one process

	// Start the runloop. This exists for the duration of the watcher
	go func() {
		C.CFRunLoopRun()
	}()

	return w, nil
}

func (w *Watcher) watch(path string) error {
	return w.watchPath(path, nil)
}

type Options struct {
    Recursive        bool
    WatchHidden      bool
    Pattern          string
    Throttle         bool
    ThrottleDuration time.Duration
//    Triggers         Triggers
    Verbose          bool
    // contains filtered or unexported fields
}

func (w *Watcher) watchPath(path string, options *Options) error {
	path, _ = filepath.Abs(path)

	w.wmut.Lock()
	_, found := w.watches[path]
	w.wmut.Unlock()

	if !found {
		cPaths := C.ArrayCreateMutable(C.int(1))
		defer C.CFRelease(C.CFTypeRef(cPaths))

		cpath := C.CString(path)
		defer C.free(unsafe.Pointer(cpath))

		str := C.CFStringCreateWithCString(nil, cpath, C.kCFStringEncodingUTF8)
		C.CFArrayAppendValue(cPaths, unsafe.Pointer(str))

		context := C.FSEventStreamContext{info: unsafe.Pointer(&w.internalEvent)}
		latency := C.CFTimeInterval(0)
		if options != nil && options.Throttle {
			latency = C.CFTimeInterval(options.ThrottleDuration / time.Second)
		}
		stream := C.EventStreamCreate(&context, cPaths, C.kFSEventStreamEventIdSinceNow + (1 << 64), latency)
		w.wmut.Lock()
		w.watches[path] = stream
		w.wmut.Unlock()
		C.FSEventStreamScheduleWithRunLoop(stream, w.rlref, C.kCFRunLoopDefaultMode)
		C.FSEventStreamStart(stream)
	}

	return nil;
}

func (w *Watcher) removeWatch(path string) error {
	w.wmut.Lock()
	stream, found := w.watches[path]
	w.wmut.Unlock()

	if found {
		C.FSEventStreamStop(stream)
		C.FSEventStreamInvalidate(stream)
		C.FSEventStreamRelease(stream)
		w.wmut.Lock()
		delete(w.watches, path)
		w.wmut.Unlock()
	}

	return nil;
}

// Close closes a watcher instance
// It sends a message to the reader goroutine to quit and removes all watches
// associated with the kevent instance
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.isClosed {
		w.mu.Unlock()
		return nil
	}
	w.isClosed = true
	w.mu.Unlock()

	// Send "quit" message to the reader goroutine
	w.done <- true
	w.wmut.Lock()		// I think fsnotify_bsd may have a very minor bug? It used w.pmut
	ws := w.watches
	w.wmut.Unlock()
	for path := range ws {
		w.removeWatch(path)
	}
	C.CFRunLoopStop(w.rlref)

	return nil
}

