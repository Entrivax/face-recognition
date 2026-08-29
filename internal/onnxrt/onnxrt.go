// Package onnxrt is a minimal Go binding to the ONNX Runtime C API, sufficient
// to run recogn's two single-input float models (SCRFD detector and ArcFace
// embedder) in-process. It requires CGO and the ONNX Runtime C library/header
// (fetched into third_party/onnxruntime by `make ort`).
//
// The include/lib paths are supplied via CGO_CFLAGS / CGO_LDFLAGS (the
// Makefile sets them). A relative -L here is a fallback for in-package builds.
package onnxrt

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/onnxruntime/include
#cgo LDFLAGS: -L${SRCDIR}/../../third_party/onnxruntime/lib -lonnxruntime -Wl,-rpath,$ORIGIN/third_party/onnxruntime/lib -Wl,-rpath,${SRCDIR}/../../third_party/onnxruntime/lib
#include <stdlib.h>
#include "onnxrt.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// Tensor is one float32 output tensor from a Run.
type Tensor struct {
	Data []float32
	Dims []int64
}

// Session is an open ONNX model. Not safe for concurrent Run calls; callers
// (the engine) serialize. Close must be called to release native resources.
type Session struct {
	s *C.ort_session
}

func lastErr() error {
	msg := C.GoString(C.ort_last_error())
	if msg == "" {
		return errors.New("onnxruntime error")
	}
	return errors.New(msg)
}

// Open loads a model from path into a new Session.
func Open(path string) (*Session, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	s := C.ort_open(cpath)
	if s == nil {
		return nil, fmt.Errorf("open %s: %w", path, lastErr())
	}
	return &Session{s: s}, nil
}

// Close releases the session's native resources. Safe to call once.
func (s *Session) Close() {
	if s.s != nil {
		C.ort_close(s.s)
		s.s = nil
	}
}

// Run executes the model on a single float32 input tensor of the given shape
// (row-major) and returns all output tensors. The input slice is not modified.
func (s *Session) Run(input []float32, shape []int64) ([]Tensor, error) {
	if s.s == nil {
		return nil, errors.New("session closed")
	}
	if len(shape) == 0 || len(shape) > 8 {
		return nil, fmt.Errorf("bad shape ndim %d", len(shape))
	}
	var n int64 = 1
	for _, d := range shape {
		if d <= 0 {
			return nil, fmt.Errorf("bad dim %d", d)
		}
		n *= d
	}
	if n != int64(len(input)) {
		return nil, fmt.Errorf("shape implies %d elements, got %d", n, len(input))
	}

	cdims := make([]C.int64_t, len(shape))
	for i, d := range shape {
		cdims[i] = C.int64_t(d)
	}

	var outputs *C.ort_tensor
	var nOutputs C.int64_t
	rc := C.ort_run_f32(
		s.s,
		(*C.float)(unsafe.Pointer(&input[0])),
		&cdims[0], C.int64_t(len(shape)),
		&outputs, &nOutputs,
	)
	if rc != 0 {
		return nil, fmt.Errorf("run: %w", lastErr())
	}
	if outputs == nil || nOutputs == 0 {
		return nil, errors.New("run produced no outputs")
	}
	// Free the C outputs array when done; each tensor's data is copied out first.
	defer C.ort_free(unsafe.Pointer(outputs))

	count := int(nOutputs)
	carr := unsafe.Slice(outputs, count)
	res := make([]Tensor, 0, count)
	for i := 0; i < count; i++ {
		ct := carr[i]
		n := int(ct.count)
		// Copy data out of the C buffer, then free the C buffer. C.float and
		// float32 are identical in memory, so we reinterpret the source slice.
		var data []float32
		if n > 0 && ct.data != nil {
			data = make([]float32, n)
			src := unsafe.Slice((*C.float)(ct.data), n)
			copy(data, *(*[]float32)(unsafe.Pointer(&src)))
		}
		if ct.data != nil {
			C.ort_free(unsafe.Pointer(ct.data))
		}
		nd := int(ct.ndim)
		dims := make([]int64, nd)
		for d := 0; d < nd; d++ {
			dims[d] = int64(ct.dims[d])
		}
		res = append(res, Tensor{Data: data, Dims: dims})
	}
	return res, nil
}
