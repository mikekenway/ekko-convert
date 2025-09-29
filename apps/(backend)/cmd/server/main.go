package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	socketio "github.com/googollee/go-socket.io"
)

const (
	maxUploadSize         int64 = 15 * 1024 * 1024 * 1024 // 15GB
	progressBroadcastStep       = 5                       // emit every 5%
)

type ffprobeStream struct {
	CodecName    string `json:"codec_name"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Duration     string `json:"duration"`
	NbFrames     string `json:"nb_frames"`
	AvgFrameRate string `json:"avg_frame_rate"`
}

type ffprobeData struct {
	Format struct {
		Duration string `json:"duration"`
		Size     string `json:"size"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
	Streams []ffprobeStream `json:"streams"`
}

type conversionProcess struct {
	cmd       *exec.Cmd
	cancelled bool
}

type conversionManager struct {
	mu          sync.Mutex
	conversions map[string]*conversionProcess
}

type serverState struct {
	downloadsDir string
	frontendDir  string
	socket       *socketio.Server
	conversions  *conversionManager
}

type cancelPayload struct {
	FileName string `json:"fileName"`
}

type conversionProgress struct {
	FileName   string `json:"fileName"`
	Progress   int    `json:"progress"`
	OutputName string `json:"outputName"`
	Completed  bool   `json:"completed,omitempty"`
}

type conversionError struct {
	FileName string `json:"fileName"`
	Error    string `json:"error"`
}

func main() {
	port := getEnv("PORT", "3001")
	downloadsDir := getEnv("DOWNLOADS_DIR", filepath.Join("public", "downloads"))
	frontendDir := os.Getenv("FRONTEND_DIST")

	if err := os.MkdirAll(downloadsDir, 0o755); err != nil {
		log.Fatalf("failed to ensure downloads directory: %v", err)
	}

	socketServer := socketio.NewServer(nil)

	conversions := &conversionManager{conversions: make(map[string]*conversionProcess)}
	state := &serverState{
		downloadsDir: downloadsDir,
		frontendDir:  frontendDir,
		socket:       socketServer,
		conversions:  conversions,
	}

	socketServer.OnConnect("/", func(conn socketio.Conn) error {
		log.Printf("client connected: %s", conn.ID())
		return nil
	})

	socketServer.OnEvent("/", "cancel-conversion", func(conn socketio.Conn, payload cancelPayload) {
		if payload.FileName == "" {
			return
		}
		log.Printf("cancellation requested for %s", payload.FileName)
		if _, ok := conversions.cancel(payload.FileName); ok {
			state.emitError(payload.FileName, "Conversion cancelled by user")
		} else {
			log.Printf("no active conversion found for %s", payload.FileName)
		}
	})

	socketServer.OnDisconnect("/", func(conn socketio.Conn, reason string) {
		log.Printf("client disconnected (%s): %s", conn.ID(), reason)
	})

	go func() {
		if err := socketServer.Serve(); err != nil {
			log.Fatalf("socket server error: %v", err)
		}
	}()
	defer socketServer.Close()

	mux := http.NewServeMux()
	mux.Handle("/socket.io/", socketServer)
	mux.HandleFunc("/api/health", state.handleHealth)
	mux.HandleFunc("/api/analyze", state.handleAnalyze)
	mux.HandleFunc("/api/convert", state.handleConvert)
	mux.HandleFunc("/api/files", state.handleFiles)

	// Serve downloads directory
	downloadsFS := http.StripPrefix("/downloads/", http.FileServer(http.Dir(downloadsDir)))
	mux.Handle("/downloads/", downloadsFS)

	// Optionally serve frontend assets if provided
	if frontendDir != "" {
		frontendHandler := spaHandler(frontendDir)
		mux.Handle("/", frontendHandler)
	}

	handler := loggingMiddleware(corsMiddleware(mux))

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      0,
		ReadTimeout:       0,
	}

	log.Printf("Video converter server running on port %s", port)
	log.Printf("Health check: http://localhost:%s/api/health", port)
	log.Printf("Downloads: http://localhost:%s/downloads/", port)

	if frontendDir != "" {
		log.Printf("Serving frontend assets from %s", frontendDir)
	}

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func (s *serverState) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r.Method)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"service": "video-converter",
	})
}

func (s *serverState) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r.Method)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		respondUploadError(w, err)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "No file uploaded", http.StatusBadRequest)
		return
	}
	defer file.Close()

	tempPath := filepath.Join(s.downloadsDir, fmt.Sprintf("temp-%s-%s", uuid.NewString(), sanitizeFilename(header.Filename)))
	if err := writeStreamToFile(file, tempPath); err != nil {
		log.Printf("failed to persist temp file: %v", err)
		http.Error(w, "Failed to store upload", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tempPath)

	metadata, err := probeFile(tempPath)
	if err != nil {
		log.Printf("analysis error: %v", err)
		http.Error(w, "Analysis failed", http.StatusInternalServerError)
		return
	}

	stream := firstVideoStream(metadata.Streams)
	duration, _ := strconv.ParseFloat(metadata.Format.Duration, 64)
	size, _ := strconv.ParseInt(metadata.Format.Size, 10, 64)

	response := map[string]interface{}{
		"name":     header.Filename,
		"duration": duration,
		"fileSize": size,
		"resolution": func() string {
			if stream != nil && stream.Width > 0 && stream.Height > 0 {
				return fmt.Sprintf("%dx%d", stream.Width, stream.Height)
			}
			return ""
		}(),
		"codec": func() string {
			if stream != nil {
				return stream.CodecName
			}
			return ""
		}(),
		"bitrate": metadata.Format.BitRate,
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *serverState) handleConvert(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r.Method)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		respondUploadError(w, err)
		return
	}

	format := r.FormValue("format")
	if format == "" {
		http.Error(w, "Missing output format", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "No file uploaded", http.StatusBadRequest)
		return
	}
	defer file.Close()

	originalName := header.Filename
	inputName := fmt.Sprintf("%s-%s", uuid.NewString(), sanitizeFilename(originalName))
	outputName := fmt.Sprintf("%s.%s", uuid.NewString(), sanitizeFilename(format))
	inputPath := filepath.Join(s.downloadsDir, inputName)
	outputPath := filepath.Join(s.downloadsDir, outputName)

	if err := writeStreamToFile(file, inputPath); err != nil {
		log.Printf("failed to store uploaded file: %v", err)
		http.Error(w, "Failed to store upload", http.StatusInternalServerError)
		return
	}
	defer os.Remove(inputPath)

	metadata, err := probeFile(inputPath)
	if err != nil {
		log.Printf("ffprobe error: %v", err)
		os.Remove(outputPath)
		http.Error(w, "Failed to analyze source file", http.StatusInternalServerError)
		return
	}

	durationSeconds := parseDurationSeconds(metadata.Format.Duration)
	primaryStream := firstVideoStream(metadata.Streams)
	if durationSeconds <= 0 && primaryStream != nil {
		durationSeconds = parseDurationSeconds(primaryStream.Duration)
		if durationSeconds <= 0 {
			durationSeconds = durationFromFrames(primaryStream.NbFrames, primaryStream.AvgFrameRate)
		}
	}
	totalFrames := parseFrameCount(primaryStream)

	log.Printf("Starting conversion of %s (%fs) to %s", originalName, durationSeconds, format)

	s.emitProgress(originalName, 0, outputName, false)

	cmd := exec.Command("ffmpeg",
		"-y",
		"-i", inputPath,
		"-progress", "pipe:1",
		"-nostats",
		outputPath,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("failed to read ffmpeg progress: %v", err)
		http.Error(w, "Failed to start conversion", http.StatusInternalServerError)
		return
	}

	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		log.Printf("ffmpeg start error: %v", err)
		if errors.Is(err, exec.ErrNotFound) {
			http.Error(w, "FFmpeg not found. Please ensure FFmpeg is installed.", http.StatusInternalServerError)
		} else {
			http.Error(w, "Failed to start conversion", http.StatusInternalServerError)
		}
		return
	}

	proc := s.conversions.store(originalName, cmd)

	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024), 1024*1024)
		lastReported := -progressBroadcastStep
		for scanner.Scan() {
			line := scanner.Text()
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])

			switch key {
			case "frame":
				currentProgress := progressFromFrame(value, totalFrames)
				if shouldEmitProgress(currentProgress, lastReported) {
					lastReported = currentProgress
					s.emitProgress(originalName, currentProgress, outputName, false)
				}
			case "out_time_ms":
				currentProgress := progressFromTimestamp(value, durationSeconds, 1000)
				if shouldEmitProgress(currentProgress, lastReported) {
					lastReported = currentProgress
					s.emitProgress(originalName, currentProgress, outputName, false)
				}
			case "out_time_us":
				currentProgress := progressFromTimestamp(value, durationSeconds, 1000000)
				if shouldEmitProgress(currentProgress, lastReported) {
					lastReported = currentProgress
					s.emitProgress(originalName, currentProgress, outputName, false)
				}
			case "out_time":
				currentProgress := progressFromTimecode(value, durationSeconds)
				if shouldEmitProgress(currentProgress, lastReported) {
					lastReported = currentProgress
					s.emitProgress(originalName, currentProgress, outputName, false)
				}
			case "progress":
				if value == "end" {
					s.emitProgress(originalName, 100, outputName, true)
				}
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("progress scanner error: %v", err)
		}
	}()

	err = cmd.Wait()
	<-progressDone
	s.conversions.remove(originalName)

	if proc != nil && proc.cancelled {
		os.Remove(outputPath)
		http.Error(w, "Conversion cancelled by user", http.StatusConflict)
		return
	}

	if err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) && exitErr.Exited() {
			log.Printf("conversion exited with error: %s", stderrBuf.String())
		} else {
			log.Printf("conversion failed: %v", err)
		}

		message, status := mapFFmpegError(stderrBuf.String(), format)
		s.emitError(originalName, message)
		http.Error(w, message, status)
		os.Remove(outputPath)
		return
	}

	outputMetadata, err := probeFile(outputPath)
	if err != nil {
		log.Printf("failed to probe output: %v", err)
		http.Error(w, "Conversion completed but metadata lookup failed", http.StatusInternalServerError)
		return
	}

	stream := firstVideoStream(outputMetadata.Streams)
	duration := parseDurationSeconds(outputMetadata.Format.Duration)
	size, _ := strconv.ParseInt(outputMetadata.Format.Size, 10, 64)

	response := map[string]interface{}{
		"name":        originalName,
		"outputName":  outputName,
		"downloadUrl": fmt.Sprintf("/downloads/%s", outputName),
		"duration":    duration,
		"fileSize":    size,
		"resolution": func() string {
			if stream != nil && stream.Width > 0 && stream.Height > 0 {
				return fmt.Sprintf("%dx%d", stream.Width, stream.Height)
			}
			return ""
		}(),
		"codec": func() string {
			if stream != nil {
				return stream.CodecName
			}
			return ""
		}(),
	}

	writeJSON(w, http.StatusOK, response)
}

func (s *serverState) handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r.Method)
		return
	}

	entries, err := os.ReadDir(s.downloadsDir)
	if err != nil {
		log.Printf("failed to list downloads: %v", err)
		http.Error(w, "Failed to list files", http.StatusInternalServerError)
		return
	}

	files := make([]map[string]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		created := time.Now().UTC()
		if info, err := entry.Info(); err == nil {
			created = info.ModTime().UTC()
		}
		files = append(files, map[string]string{
			"name":        entry.Name(),
			"downloadUrl": fmt.Sprintf("/downloads/%s", entry.Name()),
			"createdAt":   created.Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, files)
}

func (s *serverState) emitProgress(fileName string, progress int, outputName string, completed bool) {
	if s.socket == nil {
		return
	}
	payload := conversionProgress{
		FileName:   fileName,
		Progress:   clamp(progress, 0, 100),
		OutputName: outputName,
		Completed:  completed,
	}
	log.Printf("Progress update for %s: %d%%", fileName, payload.Progress)
	s.socket.BroadcastToNamespace("/", "conversion-progress", payload)
}

func (s *serverState) emitError(fileName, message string) {
	if s.socket == nil {
		return
	}
	payload := conversionError{
		FileName: fileName,
		Error:    message,
	}
	s.socket.BroadcastToNamespace("/", "conversion-error", payload)
}

func (m *conversionManager) store(name string, cmd *exec.Cmd) *conversionProcess {
	m.mu.Lock()
	defer m.mu.Unlock()
	proc := &conversionProcess{cmd: cmd}
	m.conversions[name] = proc
	return proc
}

func (m *conversionManager) remove(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.conversions, name)
}

func (m *conversionManager) cancel(name string) (*conversionProcess, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	proc, ok := m.conversions[name]
	if !ok || proc.cmd.Process == nil {
		return nil, false
	}
	if err := proc.cmd.Process.Kill(); err != nil {
		log.Printf("failed to cancel conversion %s: %v", name, err)
		return proc, false
	}
	proc.cancelled = true
	delete(m.conversions, name)
	return proc, true
}

func writeStreamToFile(src io.Reader, destination string) error {
	dst, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	return nil
}

func probeFile(path string) (*ffprobeData, error) {
	cmd := exec.Command("ffprobe", "-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", path)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var data ffprobeData
	if err := json.Unmarshal(output, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func progressFromTimestamp(value string, durationSeconds float64, unitsPerSecond float64) int {
	if durationSeconds <= 0 || unitsPerSecond <= 0 {
		return 0
	}
	raw, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	seconds := raw / unitsPerSecond
	percent := int(math.Round((seconds / durationSeconds) * 100))
	return clamp(percent, 0, 100)
}

func progressFromTimecode(value string, durationSeconds float64) int {
	if durationSeconds <= 0 {
		return 0
	}
	seconds := parseDurationSeconds(value)
	if seconds <= 0 {
		return 0
	}
	percent := int(math.Round((seconds / durationSeconds) * 100))
	return clamp(percent, 0, 100)
}

func progressFromFrame(frameValue string, totalFrames float64) int {
	if totalFrames <= 0 {
		return 0
	}
	currentFrame, err := strconv.ParseFloat(frameValue, 64)
	if err != nil {
		return 0
	}
	percent := int(math.Round((currentFrame / totalFrames) * 100))
	return clamp(percent, 0, 100)
}

func parseDurationSeconds(raw string) float64 {
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		if strings.Contains(raw, ":") {
			return parseColonDuration(raw)
		}
		return 0
	}
	return value
}

func parseColonDuration(raw string) float64 {
	parts := strings.Split(raw, ":")
	if len(parts) != 3 {
		return 0
	}
	hours, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}
	minutes, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0
	}
	seconds, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return 0
	}
	return hours*3600 + minutes*60 + seconds
}

func parseFrameCount(stream *ffprobeStream) float64 {
	if stream == nil {
		return 0
	}
	if frames, err := strconv.ParseFloat(stream.NbFrames, 64); err == nil && frames > 0 {
		return frames
	}
	rate := parseFraction(stream.AvgFrameRate)
	if rate <= 0 {
		return 0
	}
	duration := parseDurationSeconds(stream.Duration)
	if duration <= 0 {
		return 0
	}
	return duration * rate
}

func durationFromFrames(nbFrames, avgFrameRate string) float64 {
	frames, err := strconv.ParseFloat(nbFrames, 64)
	if err != nil || frames <= 0 {
		return 0
	}
	rate := parseFraction(avgFrameRate)
	if rate <= 0 {
		return 0
	}
	return frames / rate
}

func parseFraction(raw string) float64 {
	if raw == "" {
		return 0
	}
	if !strings.Contains(raw, "/") {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0
		}
		return value
	}
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) != 2 {
		return 0
	}
	numerator, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}
	denominator, err := strconv.ParseFloat(parts[1], 64)
	if err != nil || denominator == 0 {
		return 0
	}
	return numerator / denominator
}

func shouldEmitProgress(current, last int) bool {
	if current <= last {
		return false
	}
	if current >= 100 {
		return true
	}
	return current >= last+progressBroadcastStep
}

func firstVideoStream(streams []ffprobeStream) *ffprobeStream {
	for i := range streams {
		stream := &streams[i]
		if stream.Width > 0 || stream.Height > 0 {
			return stream
		}
	}
	if len(streams) > 0 {
		return &streams[0]
	}
	return nil
}

func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	name = strings.ReplaceAll(name, "..", "")
	return strings.ReplaceAll(name, " ", "_")
}

func methodNotAllowed(w http.ResponseWriter, method string) {
	http.Error(w, fmt.Sprintf("Method %s not allowed", method), http.StatusMethodNotAllowed)
}

func respondUploadError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, http.ErrNotMultipart) {
		http.Error(w, "Invalid multipart payload", http.StatusBadRequest)
		return
	}
	if strings.Contains(err.Error(), "request body too large") {
		http.Error(w, fmt.Sprintf("File too large. Maximum size allowed is %d bytes", maxUploadSize), http.StatusBadRequest)
		return
	}
	http.Error(w, "Failed to parse upload", http.StatusBadRequest)
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("failed to encode JSON response: %v", err)
	}
}

func mapFFmpegError(stderr, format string) (string, int) {
	switch {
	case strings.Contains(stderr, "Unknown encoder"):
		return fmt.Sprintf("Unsupported output format: %s", format), http.StatusBadRequest
	case strings.Contains(stderr, "Invalid data found"):
		return "Invalid or corrupted video file", http.StatusBadRequest
	case strings.Contains(stderr, "No space left"):
		return "Server storage full. Please try again later.", http.StatusInternalServerError
	case strings.Contains(stderr, "ffmpeg version"):
		return "Conversion failed", http.StatusInternalServerError
	default:
		if stderr != "" {
			return stderr, http.StatusInternalServerError
		}
		return "Conversion failed", http.StatusInternalServerError
	}
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
		origin := r.Header.Get("Origin")
		if origin == "" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Del("Access-Control-Allow-Credentials")
		} else {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		}

		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func clamp(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func spaHandler(root string) http.Handler {
	fileServer := http.FileServer(http.Dir(root))
	indexPath := filepath.Join(root, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(root, r.URL.Path)
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}

		http.ServeFile(w, r, indexPath)
	})
}
