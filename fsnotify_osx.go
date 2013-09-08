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

static FSEventStreamRef EventStreamCreateRelativeToDevice(FSEventStreamContext * context, dev_t dev, CFArrayRef paths, FSEventStreamEventId since, CFTimeInterval latency) {
	return FSEventStreamCreateRelativeToDevice(NULL, (FSEventStreamCallback) fsevtCallback, context, dev, paths, since, latency, 0);
}

static FSEventStreamRef EventStreamCreate(FSEventStreamContext * context, CFArrayRef paths, FSEventStreamEventId since, CFTimeInterval latency) {
	return FSEventStreamCreate(NULL, (FSEventStreamCallback) fsevtCallback, context, paths, since, latency, 0);
}
*/
import "C"

import "unsafe"
import "path/filepath"
import "sync"
import "os"



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

/*
	extern FSEventStreamRef FSEventStreamCreate(
		CFAllocatorRef allocator,
		FSEventStreamCallback callback,
		FSEventStreamContext *context,
		CFArrayRef pathsToWatch,
		FSEventStreamEventId sinceWhen,
		CFTimeInterval latency,
		FSEventStreamCreateFlags flags);

	typedef void ( *FSEventStreamCallback )(
		ConstFSEventStreamRef streamRef,
		void *clientCallBackInfo,
		size_t numEvents,
		void *eventPaths,
		const FSEventStreamEventFlags eventFlags[],
		const FSEventStreamEventId eventIds[]);
*/

func FSEventsLatestId() uint64 {
	return uint64(C.FSEventsGetCurrentEventId())
}

func DeviceForPath(pth string) int64 {
	cStat := C.struct_stat{}
	cPath := C.CString(pth)
	defer C.free(unsafe.Pointer(cPath))

	_ = C.lstat(cPath, &cStat)
	return int64(cStat.st_dev)
}

func GetIdForDeviceBeforeTime(dev, tm int64) uint64 {
	return uint64(C.FSEventsGetLastEventIdForDeviceBeforeTime(C.dev_t(dev), C.CFAbsoluteTime(tm)))
}

func FSEventsSince(paths []string, dev int64, since uint64) []FileEvent {
	cPaths := C.ArrayCreateMutable(C.int(len(paths)))
	defer C.CFRelease(C.CFTypeRef(cPaths))

	for _, p := range paths {
		p, _ = filepath.Abs(p)
		cpath := C.CString(p)
		defer C.free(unsafe.Pointer(cpath))

		str := C.CFStringCreateWithCString(nil, cpath, C.kCFStringEncodingUTF8)
		C.CFArrayAppendValue(cPaths, unsafe.Pointer(str))
	}

	if since == 0 {
		/* If since == 0 is passed to FSEventStreamCreate it will mean 'since the beginning of time'.
		We remap to 'now'. */
		since = C.kFSEventStreamEventIdSinceNow + (1 << 64)
	}

	evtC := make(chan []FileEvent)
	context := C.FSEventStreamContext{info: unsafe.Pointer(&evtC)}

	latency := C.CFTimeInterval(1.0)
	var stream C.FSEventStreamRef
	if dev != 0 {
		stream = C.EventStreamCreateRelativeToDevice(&context, C.dev_t(dev), cPaths, C.FSEventStreamEventId(since), latency)
	} else {
		stream = C.EventStreamCreate(&context, cPaths, C.FSEventStreamEventId(since), latency)
	}

	rlref := C.CFRunLoopGetCurrent()

	go func() {
		/* Schedule the stream on the runloop, then run the runloop concurrently with starting/flushing/stopping the stream */
		C.FSEventStreamScheduleWithRunLoop(stream, rlref, C.kCFRunLoopDefaultMode)
		go func() {
			C.CFRunLoopRun()
		}()
		C.FSEventStreamStart(stream)
		C.FSEventStreamFlushSync(stream)
		C.FSEventStreamStop(stream)
		C.FSEventStreamInvalidate(stream)
		C.FSEventStreamRelease(stream)
		C.CFRunLoopStop(rlref)
		close(evtC)
	}()

	var events []FileEvent
	for evts := range evtC {
		events = append(events, evts...)
	}

	return events
}

type FSEventStream struct {
	stream C.FSEventStreamRef
	rlref  C.CFRunLoopRef
	C      chan []FileEvent
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
	stream C.FSEventStreamRef
	rlref  C.CFRunLoopRef
	mu            sync.Mutex          // Mutex for the Watcher itself.
	watches       map[string]int      // Map of watched file diescriptors (key: path)
	wmut          sync.Mutex          // Protects access to watches.
	fsnFlags      map[string]uint32   // Map of watched files to flags used for filter
	fsnmut        sync.Mutex          // Protects access to fsnFlags.
	enFlags       map[string]uint32   // Map of watched files to evfilt note flags used in kqueue
	enmut         sync.Mutex          // Protects access to enFlags.
	paths         map[int]string      // Map of watched paths (key: watch descriptor)
	finfo         map[int]os.FileInfo // Map of file information (isDir, isReg; key: watch descriptor)
	pmut          sync.Mutex          // Protects access to paths and finfo.
	fileExists    map[string]bool     // Keep track of if we know this file exists (to stop duplicate create events)
	femut         sync.Mutex          // Proctects access to fileExists.
	Error         chan error          // Errors are sent on this channel
	internalEvent chan *FileEvent     // Events are queued on this channel
	Event         chan *FileEvent     // Events are returned on this channel
	done          chan bool           // Channel for sending a "quit message" to the reader goroutine
	isClosed      bool                // Set to true when Close() is first called
	bufmut        sync.Mutex          // Protects access to kbuf.
}


func NewWatcher(paths []string) (*Watcher, error) {
	cPaths := C.ArrayCreateMutable(C.int(len(paths)))
	defer C.CFRelease(C.CFTypeRef(cPaths))

	for _, p := range paths {
		p, _ = filepath.Abs(p)
		cpath := C.CString(p)
		defer C.free(unsafe.Pointer(cpath))

		str := C.CFStringCreateWithCString(nil, cpath, C.kCFStringEncodingUTF8)
		C.CFArrayAppendValue(cPaths, unsafe.Pointer(str))
	}

//	if since == 0 {
		/* If since == 0 is passed to FSEventStreamCreate it will mean 'since the beginning of time'.
		We remap to 'now'. */
		since := C.kFSEventStreamEventIdSinceNow
//	}

	w := &Watcher{
		watches:       make(map[string]int),
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
	context := C.FSEventStreamContext{info: unsafe.Pointer(&w.internalEvent)}

	latency := C.CFTimeInterval(1.0)
	w.stream = C.EventStreamCreate(&context, cPaths, C.FSEventStreamEventId(since), latency)

	w.rlref = C.CFRunLoopGetCurrent()

	go func() {
		/* Schedule the stream on the runloop, then run the runloop concurrently with starting/flushing/stopping the stream */
		C.FSEventStreamScheduleWithRunLoop(w.stream, w.rlref, C.kCFRunLoopDefaultMode)
		go func() {
			C.CFRunLoopRun()
		}()
		C.FSEventStreamStart(w.stream)
	}()

	return w, nil
}

func (w *Watcher) watch(path string) error {
	return nil;
}

func (w *Watcher) removeWatch(path string) error {
	return nil;
}

/*func (es *FSEventStream) Flush() {
	C.FSEventStreamFlushSync(es.stream)
}

func (es *FSEventStream) Stop() {
	C.FSEventStreamStop(es.stream)
	C.FSEventStreamInvalidate(es.stream)
	C.FSEventStreamRelease(es.stream)
	C.CFRunLoopStop(es.rlref)
	close(es.C)
}*/
