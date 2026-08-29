package engine

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
)

// sidecar manages one persistent Python inference process speaking NDJSON
// over stdin/stdout. It is safe for concurrent use: requests are serialized
// because CPU inference is single-stream anyway. A crashed or wedged process
// is transparently restarted on the next call.
type sidecar struct {
	python string
	script string
	env    []string

	mu      sync.Mutex // serializes requests AND guards cmd/in/out
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	started bool

	reqID int64
}

func newSidecar(python, script string, env []string) *sidecar {
	return &sidecar{python: python, script: script, env: env}
}

// start launches the python process. Callers must hold s.mu.
func (s *sidecar) startLocked() error {
	if s.started && s.cmd != nil && s.cmd.ProcessState == nil {
		return nil // already running
	}
	s.stopLocked()
	cmd := exec.Command(s.python, s.script)
	cmd.Env = s.env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("sidecar stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("sidecar stdout: %w", err)
	}
	// Inherit stderr so python diagnostics surface in the app's logs.
	cmd.Stderr = &stderrWriter{}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("sidecar start: %w", err)
	}
	s.cmd = cmd
	s.stdin = stdin
	// Embeddings responses are large-ish (512 floats); give the scanner room.
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	s.scanner = sc
	s.started = true
	return nil
}

// stopLocked kills the process. Callers must hold s.mu.
func (s *sidecar) stopLocked() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_, _ = s.cmd.Process.Wait()
	}
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	s.cmd, s.stdin, s.scanner = nil, nil, nil
	s.started = false
}

// Close terminates the sidecar.
func (s *sidecar) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

// do performs one request/response round-trip, restarting the sidecar once on
// transport failure. It is the single entry point used by detect/embed.
func (s *sidecar) do(req map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.startLocked(); err != nil {
			lastErr = err
			continue
		}
		id := atomic.AddInt64(&s.reqID, 1)
		req["id"] = id
		payload, err := json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		if _, err := s.stdin.Write(append(payload, '\n')); err != nil {
			lastErr = fmt.Errorf("write to sidecar: %w", err)
			s.stopLocked()
			continue
		}
		if !s.scanner.Scan() {
			err := s.scanner.Err()
			if err == nil {
				err = errors.New("sidecar closed the stream")
			}
			lastErr = fmt.Errorf("read from sidecar: %w", err)
			s.stopLocked()
			continue
		}
		var resp map[string]any
		if err := json.Unmarshal(s.scanner.Bytes(), &resp); err != nil {
			lastErr = fmt.Errorf("decode sidecar response: %w", err)
			s.stopLocked()
			continue
		}
		if e, ok := resp["error"].(string); ok && e != "" {
			return nil, errors.New(e)
		}
		return resp, nil
	}
	return nil, fmt.Errorf("sidecar unavailable: %w", lastErr)
}

// ping checks the sidecar is responsive, starting it if necessary.
func (s *sidecar) ping() error {
	_, err := s.do(map[string]any{"cmd": "ping"})
	return err
}

// detect sends an image for face detection.
func (s *sidecar) detect(imgBytes []byte) ([]Face, error) {
	resp, err := s.do(map[string]any{
		"cmd":   "detect",
		"image": base64.StdEncoding.EncodeToString(imgBytes),
	})
	if err != nil {
		return nil, err
	}
	rawFaces, _ := resp["faces"].([]any)
	faces := make([]Face, 0, len(rawFaces))
	for _, rf := range rawFaces {
		m, ok := rf.(map[string]any)
		if !ok {
			continue
		}
		f := Face{Score: num(m["score"])}
		if bb, ok := m["bbox"].([]any); ok && len(bb) == 4 {
			f.BBox = [4]float64{num(bb[0]), num(bb[1]), num(bb[2]), num(bb[3])}
		}
		if lm, ok := m["landmarks"].([]any); ok {
			for _, p := range lm {
				if pt, ok := p.([]any); ok && len(pt) == 2 {
					f.Landmarks = append(f.Landmarks, [2]float64{num(pt[0]), num(pt[1])})
				}
			}
		}
		faces = append(faces, f)
	}
	return faces, nil
}

// embed sends an aligned 112x112 image and returns its 512-d embedding.
func (s *sidecar) embed(imgBytes []byte) ([]float32, error) {
	resp, err := s.do(map[string]any{
		"cmd":   "embed",
		"image": base64.StdEncoding.EncodeToString(imgBytes),
	})
	if err != nil {
		return nil, err
	}
	raw, _ := resp["embedding"].([]any)
	emb := make([]float32, 0, len(raw))
	for _, v := range raw {
		emb = append(emb, float32(num(v)))
	}
	return emb, nil
}

func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	default:
		return 0
	}
}

// stderrWriter prefixes python stderr lines and forwards them to the process
// stderr so sidecar diagnostics (startup banner, errors) appear in app logs.
type stderrWriter struct{}

func (stderrWriter) Write(p []byte) (int, error) {
	fmt.Fprintf(os.Stderr, "[sidecar] %s", p)
	return len(p), nil
}
