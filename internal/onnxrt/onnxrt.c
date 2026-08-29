// Implementation of the minimal ORT C-API shim. See onnxrt.h for the contract.
//
// Ownership summary:
//   - The single OrtEnv is a process-global, created lazily and never freed
//     (it is safe to leak for the life of the process; ORT documents the env
//     as shareable across sessions).
//   - Each ort_session owns its OrtSession plus cached name strings and the
//     OrtMemoryInfo; freed by ort_close.
//   - ort_run_f32 mallocs the outputs array and each tensor's data buffer; the
//     caller frees both via ort_free.
#include "onnxrt.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "onnxruntime_c_api.h"

// --------------------------------------------------------------------------
// Portable thread-local storage
// --------------------------------------------------------------------------
// C11 _Thread_local works on GCC ≥4.9 and Clang, which covers both the Linux
// host toolchain and mingw-w64 cross-compiler. MSVC would need
// __declspec(thread), but we don't target MSVC (CGO on Windows uses mingw).
#if defined(_MSC_VER)
  #define ORT_TLS __declspec(thread)
#else
  #define ORT_TLS _Thread_local
#endif

// --------------------------------------------------------------------------
// Error reporting (thread-local)
// --------------------------------------------------------------------------
static ORT_TLS char g_err[1024];

static void set_err(const char* fmt, const char* detail) {
  if (detail) {
    snprintf(g_err, sizeof(g_err), fmt, detail);
  } else {
    snprintf(g_err, sizeof(g_err), "%s", fmt);
  }
}

const char* ort_last_error(void) { return g_err; }

// --------------------------------------------------------------------------
// API table + global env
// --------------------------------------------------------------------------
static const OrtApi* g_api = NULL;
static OrtEnv*       g_env = NULL;

static int check_status(OrtStatus* st, const char* what) {
  if (st == NULL) return 0;
  const char* msg = g_api->GetErrorMessage(st);
  char buf[896];
  snprintf(buf, sizeof(buf), "%s: %s", what, msg ? msg : "(no message)");
  set_err("%s", buf);
  g_api->ReleaseStatus(st);
  return -1;
}

int ort_global_init(void) {
  if (g_api != NULL && g_env != NULL) return 0; // already initialised
  g_api = OrtGetApiBase()->GetApi(ORT_API_VERSION);
  if (g_api == NULL) {
    set_err("%s", "OrtGetApiBase()->GetApi returned NULL (version mismatch?)");
    return -1;
  }
  OrtStatus* st = g_api->CreateEnv(ORT_LOGGING_LEVEL_WARNING, "recogn", &g_env);
  return check_status(st, "CreateEnv");
}

// --------------------------------------------------------------------------
// Path conversion for Windows (ORTCHAR_T is wchar_t on Windows, char on Linux)
// --------------------------------------------------------------------------
#ifdef _WIN32
#include <wchar.h>

// Convert a UTF-8 narrow path to a wide-character path for the Windows ORT
// API. Returns a malloc'd wchar_t* on success (caller frees), NULL on failure.
static wchar_t* path_to_wide(const char* path) {
  // mbstowcs with NULL first arg returns the required buffer size (in wchars,
  // excluding the null terminator).
  size_t len = mbstowcs(NULL, path, 0);
  if (len == (size_t)-1) return NULL;
  wchar_t* wpath = (wchar_t*)malloc((len + 1) * sizeof(wchar_t));
  if (!wpath) return NULL;
  mbstowcs(wpath, path, len + 1);
  return wpath;
}
#endif

// --------------------------------------------------------------------------
// Session
// --------------------------------------------------------------------------
struct ort_session {
  OrtSession*    session;
  OrtMemoryInfo* mem_info;
  OrtAllocator*  allocator; // default allocator for name strings
  char**         input_names;
  int64_t        n_inputs;
  char**         output_names;
  int64_t        n_outputs;
};

ort_session* ort_open(const char* path) {
  if (ort_global_init() != 0) return NULL;

  OrtSessionOptions* opts = NULL;
  OrtStatus* st = g_api->CreateSessionOptions(&opts);
  if (check_status(st, "CreateSessionOptions")) return NULL;
  // Deterministic, single-stream CPU inference.
  OrtStatus* st_opt;
  st_opt = g_api->SetIntraOpNumThreads(opts, 1);
  if (st_opt) g_api->ReleaseStatus(st_opt);
  st_opt = g_api->SetSessionGraphOptimizationLevel(opts, ORT_ENABLE_ALL);
  if (st_opt) g_api->ReleaseStatus(st_opt);

  OrtSession* session = NULL;
#ifdef _WIN32
  // On Windows, ORTCHAR_T is wchar_t — convert the path.
  wchar_t* wpath = path_to_wide(path);
  if (!wpath) {
    set_err("%s", "failed to convert model path to wide chars");
    g_api->ReleaseSessionOptions(opts);
    return NULL;
  }
  st = g_api->CreateSession(g_env, wpath, opts, &session);
  free(wpath);
#else
  st = g_api->CreateSession(g_env, path, opts, &session);
#endif
  g_api->ReleaseSessionOptions(opts);
  if (check_status(st, "CreateSession")) return NULL;

  ort_session* s = (ort_session*)calloc(1, sizeof(ort_session));
  if (!s) {
    set_err("%s", "out of memory");
    g_api->ReleaseSession(session);
    return NULL;
  }
  s->session = session;

  st = g_api->CreateCpuMemoryInfo(OrtDeviceAllocator, OrtMemTypeDefault, &s->mem_info);
  if (check_status(st, "CreateCpuMemoryInfo")) { ort_close(s); return NULL; }

  st = g_api->GetAllocatorWithDefaultOptions(&s->allocator);
  if (check_status(st, "GetAllocatorWithDefaultOptions")) { ort_close(s); return NULL; }

  size_t n_in = 0, n_out = 0;
  st = g_api->SessionGetInputCount(session, &n_in);
  if (check_status(st, "SessionGetInputCount")) { ort_close(s); return NULL; }
  st = g_api->SessionGetOutputCount(session, &n_out);
  if (check_status(st, "SessionGetOutputCount")) { ort_close(s); return NULL; }
  s->n_inputs = (int64_t)n_in;
  s->n_outputs = (int64_t)n_out;

  s->input_names = (char**)calloc(n_in, sizeof(char*));
  s->output_names = (char**)calloc(n_out, sizeof(char*));
  if (!s->input_names || !s->output_names) {
    set_err("%s", "out of memory");
    ort_close(s);
    return NULL;
  }
  for (size_t i = 0; i < n_in; i++) {
    st = g_api->SessionGetInputName(session, i, s->allocator, &s->input_names[i]);
    if (check_status(st, "SessionGetInputName")) { ort_close(s); return NULL; }
  }
  for (size_t i = 0; i < n_out; i++) {
    st = g_api->SessionGetOutputName(session, i, s->allocator, &s->output_names[i]);
    if (check_status(st, "SessionGetOutputName")) { ort_close(s); return NULL; }
  }
  return s;
}

void ort_close(ort_session* s) {
  if (!s) return;
  if (s->allocator) {
    OrtStatus* st_free;
    if (s->input_names) {
      for (int64_t i = 0; i < s->n_inputs; i++)
        if (s->input_names[i]) {
          st_free = g_api->AllocatorFree(s->allocator, s->input_names[i]);
          if (st_free) g_api->ReleaseStatus(st_free);
        }
    }
    if (s->output_names) {
      for (int64_t i = 0; i < s->n_outputs; i++)
        if (s->output_names[i]) {
          st_free = g_api->AllocatorFree(s->allocator, s->output_names[i]);
          if (st_free) g_api->ReleaseStatus(st_free);
        }
    }
  }
  free(s->input_names);
  free(s->output_names);
  if (s->mem_info) g_api->ReleaseMemoryInfo(s->mem_info);
  if (s->session) g_api->ReleaseSession(s->session);
  free(s);
}

void ort_free(void* p) { free(p); }

// --------------------------------------------------------------------------
// Run
// --------------------------------------------------------------------------
int ort_run_f32(ort_session* s,
                const float* input, const int64_t* input_dims, int64_t input_ndim,
                ort_tensor** outputs, int64_t* n_outputs) {
  *outputs = NULL;
  *n_outputs = 0;
  if (!s || !s->session) { set_err("%s", "null session"); return -1; }
  if (s->n_inputs != 1) { set_err("%s", "shim supports single-input models only"); return -1; }

  // Compute input element count.
  size_t count = 1;
  for (int64_t i = 0; i < input_ndim; i++) count *= (size_t)input_dims[i];

  OrtValue* input_tensor = NULL;
  OrtStatus* st = g_api->CreateTensorWithDataAsOrtValue(
      s->mem_info, (void*)input, count * sizeof(float),
      input_dims, (size_t)input_ndim, ONNX_TENSOR_ELEMENT_DATA_TYPE_FLOAT,
      &input_tensor);
  if (check_status(st, "CreateTensorWithDataAsOrtValue")) return -1;

  OrtValue** out_vals = (OrtValue**)calloc((size_t)s->n_outputs, sizeof(OrtValue*));
  if (!out_vals) {
    set_err("%s", "out of memory");
    g_api->ReleaseValue(input_tensor);
    return -1;
  }

  st = g_api->Run(s->session, NULL,
                  (const char* const*)s->input_names, (const OrtValue* const*)&input_tensor, 1,
                  (const char* const*)s->output_names, (size_t)s->n_outputs,
                  out_vals);
  g_api->ReleaseValue(input_tensor);
  if (check_status(st, "Run")) {
    for (int64_t i = 0; i < s->n_outputs; i++)
      if (out_vals[i]) g_api->ReleaseValue(out_vals[i]);
    free(out_vals);
    return -1;
  }

  ort_tensor* arr = (ort_tensor*)calloc((size_t)s->n_outputs, sizeof(ort_tensor));
  if (!arr) {
    set_err("%s", "out of memory");
    for (int64_t i = 0; i < s->n_outputs; i++)
      if (out_vals[i]) g_api->ReleaseValue(out_vals[i]);
    free(out_vals);
    return -1;
  }

  for (int64_t i = 0; i < s->n_outputs; i++) {
    // Shape.
    OrtTensorTypeAndShapeInfo* info = NULL;
    st = g_api->GetTensorTypeAndShape(out_vals[i], &info);
    if (check_status(st, "GetTensorTypeAndShape")) goto fail;
    size_t ndim = 0;
    st = g_api->GetDimensionsCount(info, &ndim);
    if (check_status(st, "GetDimensionsCount")) { g_api->ReleaseTensorTypeAndShapeInfo(info); goto fail; }
    if (ndim > 8) ndim = 8;
    int64_t dims[8] = {0};
    st = g_api->GetDimensions(info, dims, ndim);
    size_t elem_count = 0;
    st = st ? st : g_api->GetTensorShapeElementCount(info, &elem_count);
    g_api->ReleaseTensorTypeAndShapeInfo(info);
    if (check_status(st, "GetDimensions/ElementCount")) goto fail;

    // Data.
    void* raw = NULL;
    st = g_api->GetTensorMutableData(out_vals[i], &raw);
    if (check_status(st, "GetTensorMutableData")) goto fail;

    float* buf = (float*)malloc(elem_count * sizeof(float));
    if (!buf) { set_err("%s", "out of memory"); goto fail; }
    memcpy(buf, raw, elem_count * sizeof(float));

    arr[i].data = buf;
    arr[i].count = (int64_t)elem_count;
    arr[i].ndim = (int64_t)ndim;
    for (int64_t d = 0; d < (int64_t)ndim; d++) arr[i].dims[d] = dims[d];

    g_api->ReleaseValue(out_vals[i]);
  }

  free(out_vals);
  *outputs = arr;
  *n_outputs = s->n_outputs;
  return 0;

fail:
  for (int64_t i = 0; i < s->n_outputs; i++) {
    if (arr[i].data) free(arr[i].data);
    if (out_vals[i]) g_api->ReleaseValue(out_vals[i]);
  }
  free(arr);
  free(out_vals);
  return -1;
}
