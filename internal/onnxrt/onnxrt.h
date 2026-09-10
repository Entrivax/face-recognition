// Minimal C shim over the ONNX Runtime C API.
//
// The ORT C API is accessed through a function-table (OrtApi). Centralising
// every call here keeps the Go/cgo side free of that indirection and keeps all
// ownership rules in one place. All functions return 0 on success and non-zero
// on failure; on failure the returned char* (thread-local) describes the error
// and must be copied by the caller before the next call.
#ifndef RECOGN_ONNXRT_H
#define RECOGN_ONNXRT_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

// Opaque handle to an OrtSession (+ cached input/output names).
typedef struct ort_session ort_session;

// One output tensor produced by ort_run, laid out for easy Go consumption.
// `data` points to a freshly malloc'd float buffer of `count` elements that
// the CALLER owns and must release with ort_free.
typedef struct {
  float*  data;    // owned by caller; free with ort_free
  int64_t count;   // number of float elements
  int64_t ndim;    // number of dimensions
  int64_t dims[8]; // shape (up to 8 dims)
} ort_tensor;

// Load the ONNX Runtime shared library from `path` (an exact file path, or a
// bare soname like "libonnxruntime.so.1" resolved through the normal loader
// search). Resolves the single entry point OrtGetApiBase, through which the
// whole OrtApi function table is reached. Must be called — and succeed —
// before ort_global_init/ort_open. The loaded library is deliberately never
// unloaded: it lives for the whole process (ORT keeps global state).
// Returns 0 on success.
int  ort_runtime_load(const char* path);

// Global one-time init of the shared OrtEnv. Safe to call repeatedly.
// Requires a successful ort_runtime_load first. Returns 0 on success.
int  ort_global_init(void);

// Create a session for the model at `path`. Returns NULL on failure.
ort_session* ort_open(const char* path);

// Run the model.
//   input      : float buffer (row-major), owned by caller, not modified.
//   input_dims : shape of the input tensor.
//   input_ndim : number of input dims.
//   outputs    : out-param, set to a malloc'd array of ort_tensor (caller frees
//                the array with ort_free and each tensor's data with ort_free).
//   n_outputs  : set to the number of outputs.
// Returns 0 on success.
int ort_run_f32(ort_session* s,
                const float* input, const int64_t* input_dims, int64_t input_ndim,
                ort_tensor** outputs, int64_t* n_outputs);

// Free a buffer previously returned by ort_run_f32 (tensor data or the
// outputs array). Safe on NULL.
void ort_free(void* p);

// Destroy a session created by ort_open. Safe on NULL.
void ort_close(ort_session* s);

// Returns a thread-local static string describing the most recent error.
// Never NULL; empty when there is no error. Copy before further calls.
const char* ort_last_error(void);

#ifdef __cplusplus
}
#endif

#endif // RECOGN_ONNXRT_H
