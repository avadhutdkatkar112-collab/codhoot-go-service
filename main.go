package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	maxOutputSize     = 512 * 1024 // 512KB
	maxSourceSize     = 100 * 1024 // 100KB
	maxCompileTime    = 60 * time.Second
	maxExecTime       = 10 * time.Second
	maxConcurrentJobs = 8
	workspaceDir      = "/tmp/codhoot-workspace"
	cacheDir          = "/tmp/codhoot-cache"
	gocacheDir        = "/tmp/gocache"
	srcFilename       = "main.go"
)

type CompileRequest struct {
	Source string `json:"source"`
	// Files plus EntryFile enable the multi-file contract the backend uses.
	// Source is still accepted so older callers keep working.
	Files     []File `json:"files,omitempty"`
	EntryFile string `json:"entry_file,omitempty"`
}

type CompileResponse struct {
	Success         bool   `json:"success"`
	Output          string `json:"output,omitempty"`
	Error           string `json:"error,omitempty"`
	ExitCode        int    `json:"exit_code"`
	CompileTime     int64  `json:"compile_time_ms"`
	ExecuteTime     int64  `json:"execute_time_ms"`
	Timeout         bool   `json:"timeout,omitempty"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
}

type HealthResponse struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
}

// jobSem bounds concurrent compiler+runner processes so a burst of students
// cannot OOM the 512MB free-tier container (which would restart it and wipe
// every warm cache). Requests queue up to their own deadline instead.
var jobSem = make(chan struct{}, maxConcurrentJobs)

// uids hands each in-flight job its own sandbox uid so a job's working directory
// is genuinely private to it. See harden.go for why a single shared uid is not
// isolation.
var uids = newUIDPool()

// artifactCache is a tiny FIFO cache keyed by sha256(source) -> compiled binary.
// Identical lesson/tutorial code is the dominant workload; compiling it once and
// reusing the artifact turns repeat runs from seconds into milliseconds.
var (
	cacheMu    sync.Mutex
	cacheLRU   = make(map[string]string) // hash -> artifact path
	cacheOrder []string                  // FIFO eviction order
	maxCache   = 64
)

func cacheGet(hash string) (string, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	p, ok := cacheLRU[hash]
	return p, ok
}

func cachePut(hash, path string) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cacheLRU[hash] = path
	cacheOrder = append(cacheOrder, hash)
	if len(cacheOrder) > maxCache {
		old := cacheOrder[0]
		cacheOrder = cacheOrder[1:]
		if oldPath, ok := cacheLRU[old]; ok {
			os.Remove(oldPath)
			delete(cacheLRU, old)
		}
	}
}

func main() {
	// Sandboxed path: when re-invoked as the trampoline, apply rlimits and exec
	// the real compiler or runtime. Returns only if the trampoline fails.
	if sandboxExec() {
		return
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	os.MkdirAll(workspaceDir, 0755)
	os.MkdirAll(cacheDir, 0755)
	os.MkdirAll(gocacheDir, 0755)
	defer os.RemoveAll(workspaceDir)

	// Warm the Go build cache so the first student compile does not rebuild the
	// entire stdlib on a 0.1 CPU instance.
	os.Setenv("GOTMPDIR", workspaceDir)

	if _, err := exec.LookPath("go"); err != nil {
		log.Fatalf("go not found: %v", err)
	}
	log.Println("Go found, ready to compile")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /compile", handleCompile)
	mux.HandleFunc("GET /health/live", handleHealth)
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /", handleIndex)

	handler := corsMiddleware(loggingMiddleware(mux))

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: maxCompileTime + 10*time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	log.Printf("C Compiler Service listening on :%s", port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(HealthResponse{
		Status:    "healthy",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]string{
		"service": "codhoot-c-compiler",
		"version": "2.0.0",
		"usage":   "POST /compile with {\"source\": \"...\"}",
	})
}

// runCommand runs a compiler/runner, killing the whole process group on
// timeout so orphaned children (cc1, as, ld) never accumulate on the
// 512MB container.
func runCommand(parent context.Context, timeout time.Duration, dir string, uid uint32, name string, args ...string) ([]byte, int, bool) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	self, serr := selfPath()
	if serr != nil {
		return nil, -1, false
	}

	// Run the sandbox trampoline (this same binary), which applies rlimits and
	// then execs the real target. That is how rlimits get applied at all, since
	// Go's os/exec cannot set them in a child.
	full := append([]string{sandboxExecFlag, name}, args...)
	cmd := exec.CommandContext(ctx, self, full...)
	cmd.Dir = dir
	// GOCACHE comes from the image (shared and pre-warmed with the stdlib)
	// rather than being per-job: a per-job cache is cold on every request and
	// made each compile rebuild the entire standard library. GOTMPDIR stays
	// per-job so scratch writes remain inside the private job directory.
	cmd.Env = childEnv(dir,
		"GOTMPDIR="+dir,
		"GONOSUMDB=*",
		"GOSUMDB=off")
	cmd.SysProcAttr = hardenProcAttr(uid)

	killGroup := func() {
		for i := 0; i < 200 && (cmd.Process == nil || cmd.Process.Pid <= 0); i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			killProcessGroup(cmd.Process.Pid)
		}
	}
	go func() {
		<-ctx.Done()
		killGroup()
	}()

	// Stream into a bounded writer instead of CombinedOutput, which would buffer
	// the program's entire output before any limit applied.
	cw := &cappedWriter{max: maxOutputSize}
	cmd.Stdout = cw
	cmd.Stderr = cw
	runErr := cmd.Run()

	out := cw.Bytes()
	if ctx.Err() == context.DeadlineExceeded {
		return out, -1, true
	}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			return out, ee.ExitCode(), false
		}
		return out, 1, false
	}
	return out, 0, false
}

func handleCompile(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	var req CompileRequest
	limitBody(w, r)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", 0, 0)
		return
	}

	// A multi-file request carries no source field; resolveEntry validates the
	// file set instead, so these single-source checks apply only when it is absent.
	if len(req.Files) == 0 {
		if strings.TrimSpace(req.Source) == "" {
			writeError(w, http.StatusBadRequest, "Source code is required", 0, 0)
			return
		}
	}

	if len(req.Source) > maxSourceSize {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Source code exceeds %d bytes", maxSourceSize), 0, 0)
		return
	}

	// Bound concurrency: queue up to the request deadline instead of spawning
	// unlimited compilers that would OOM the container.
	select {
	case jobSem <- struct{}{}:
		defer func() { <-jobSem }()
	case <-r.Context().Done():
		writeError(w, http.StatusServiceUnavailable, "Compiler is busy, try again", 0, 0)
		return
	}

	// deterministic artifact reuse for identical source
	sum := sha256.Sum256([]byte(req.Source))
	hash := hex.EncodeToString(sum[:])
	artifact, cacheHit := cacheGet(hash)

	// Each job gets its own uid and a private random 0700 directory. Sharing one
	// predictable directory let concurrent jobs read each other's source and
	// rewrite it before it was executed.
	uid := uids.acquire()
	defer uids.release(uid)

	jobDir, jerr := newJobDir(uid)
	if jerr != nil {
		writeError(w, http.StatusInternalServerError, "Failed to create workspace", 0, 0)
		return
	}
	defer os.RemoveAll(jobDir)

	compileMs, execMs, output, exitCode, timeout, truncated := compileAndRun(uid, jobDir, req.Source, artifact, cacheHit, hash)

	resp := CompileResponse{
		Success:         exitCode == 0,
		Output:          output,
		ExitCode:        exitCode,
		CompileTime:     compileMs,
		ExecuteTime:     execMs,
		Timeout:         timeout,
		OutputTruncated: truncated,
	}

	if exitCode != 0 && output == "" {
		resp.Error = "Compilation or execution failed"
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)

	log.Printf("Compile: exit=%d compile=%dms exec=%dms total=%dms timeout=%v cached=%v",
		exitCode, compileMs, execMs, time.Since(start).Milliseconds(), timeout, cacheHit)
}

func compileAndRun(uid uint32, jobDir, source, cachedArtifact string, cacheHit bool, hash string) (compileMs, execMs int64, output string, exitCode int, timeout, truncated bool) {
	ep, err := resolveEntry(source, nil, "", srcFilename)
	if err != nil {
		return 0, 0, fmt.Sprintf("Failed to validate source: %v", err), -1, false, false
	}
	srcFile, werr := writeEntry(jobDir, ep, uid)
	if werr != nil {
		return 0, 0, fmt.Sprintf("Failed to stage source: %v", werr), -1, false, false
	}

	// The compiler parses untrusted input, so it runs untrusted and confined to
	// this job's private directory. It must not be able to write the shared cache
	// directly; only the service itself, which no request can reach, publishes
	// there. A job that fails to publish still runs its local binary.
	binFile := filepath.Join(jobDir, "main")
	localBin := binFile
	compileOutput, compileCode, compileTimedOut := []byte{}, 0, false
	compileStart := time.Now()

	if cacheHit && cachedArtifact != "" && verifyArtifact(cachedArtifact, source) {
		binFile = cachedArtifact
	} else {
		// Compile into the persistent cache dir so the artifact survives across
		// requests (jobDir is cleaned up after every run).
		cacheArtifact := filepath.Join(cacheDir, hash+".bin")
		if verifyArtifact(cacheArtifact, source) {
			binFile = cacheArtifact
			cachePut(hash, cacheArtifact)
		} else {
			compileOutput, compileCode, compileTimedOut = runCommand(
				context.Background(), maxCompileTime, jobDir, uid,
				"go", "build", "-o", localBin, srcFile,
			)
			compileMs = time.Since(compileStart).Milliseconds()
			if compileCode == 0 {
				// Publish through the trusted service, sealing the artifact so a later
				// reader can tell it apart from anything a user planted.
				if blob, rerr := os.ReadFile(localBin); rerr == nil {
					if werr := writeArtifact(cacheArtifact, source, blob); werr == nil {
						cachePut(hash, cacheArtifact)
						binFile = cacheArtifact
					}
				}
			}
		}
	}

	if compileTimedOut {
		return compileMs, 0, "Compilation timed out (limit: 60s)", -1, true, false
	}
	if compileCode != 0 {
		return compileMs, 0, sanitizeOutput(string(compileOutput), jobDir), compileCode, false, false
	}

	// Execute on a fresh filesystem tree per request (jobDir), so programs that
	// write files do not interfere with other users' runs.
	execStart := time.Now()
	execOutput, execCode, execTimedOut := runCommand(context.Background(), maxExecTime, jobDir, uid, binFile)
	execMs = time.Since(execStart).Milliseconds()
	if execTimedOut {
		return compileMs, execMs, "Execution timed out (limit: 10s)", -1, true, false
	}

	combinedOutput := sanitizeOutput(string(execOutput), jobDir)
	if len(combinedOutput) >= maxOutputSize {
		combinedOutput = combinedOutput[:maxOutputSize] + "\n... [output truncated]"
		truncated = true
	}

	return compileMs, execMs, combinedOutput, execCode, false, truncated
}

func writeError(w http.ResponseWriter, status int, message string, compileMs, execMs int64) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(CompileResponse{
		Success:     false,
		Error:       message,
		ExitCode:    -1,
		CompileTime: compileMs,
		ExecuteTime: execMs,
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}
