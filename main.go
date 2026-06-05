package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/joho/godotenv"
)

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\x00", "")
	name = strings.TrimSpace(name)
	return name
}

func decodeGitHubContent(b64 string) ([]byte, error) {
	b64 = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, b64)
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

type GitHubContent struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Sha  string `json:"sha"`
	Size int    `json:"size"`
	Type string `json:"type"`
}

type GitHubError struct {
	Message string `json:"message"`
}

type RenameRequest struct {
	OldName string `json:"old_name"`
	NewName string `json:"new_name"`
}

type SyncState struct {
	GitSHAs        map[string]string `json:"github_shas"`
	PendingDeletes []string          `json:"pending_deletes"`
}

type SyncResult struct {
	Uploaded int      `json:"uploaded"`
	Updated  int      `json:"updated"`
	Deleted  int      `json:"deleted"`
	Errors   []string `json:"errors"`
}

var (
	githubToken string
	repoOwner   = "goshva"
	repoName    = "moneyTracker"
	targetDir   = "photos"
	localDir    = "./photos"
	stateFile   = "./photos/.sync_state.json"
	apiBaseURL  = "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/contents/" + targetDir

	stateMu   sync.Mutex
	syncState SyncState
)

func init() {
	err := godotenv.Load()
	if err != nil {
		log.Println("⚠️ .env файл не найден, используется переменная окружения GHTOKEN")
	}
	githubToken = os.Getenv("GHTOKEN")
	if githubToken == "" {
		log.Println("⚠️ GHTOKEN не задан — синхронизация с Git будет недоступна")
	}

	if err := os.MkdirAll(localDir, 0755); err != nil {
		log.Fatal("❌ Не удалось создать папку photos:", err)
	}
	loadSyncState()
}

func loadSyncState() {
	stateMu.Lock()
	defer stateMu.Unlock()

	data, err := os.ReadFile(stateFile)
	if err != nil {
		syncState = SyncState{GitSHAs: make(map[string]string)}
		return
	}
	if err := json.Unmarshal(data, &syncState); err != nil {
		syncState = SyncState{GitSHAs: make(map[string]string)}
		return
	}
	if syncState.GitSHAs == nil {
		syncState.GitSHAs = make(map[string]string)
	}
}

func saveSyncStateLocked() {
	data, _ := json.MarshalIndent(syncState, "", "  ")
	_ = os.WriteFile(stateFile, data, 0644)
}

func markFileChanged(name string) {
	stateMu.Lock()
	defer stateMu.Unlock()
	delete(syncState.GitSHAs, name)
	saveSyncStateLocked()
}

func markRenamed(oldName, newName string) {
	stateMu.Lock()
	defer stateMu.Unlock()
	if _, ok := syncState.GitSHAs[oldName]; ok {
		syncState.PendingDeletes = append(syncState.PendingDeletes, oldName)
	}
	delete(syncState.GitSHAs, oldName)
	delete(syncState.GitSHAs, newName)
	saveSyncStateLocked()
}

func localFilePath(filename string) (string, error) {
	filename = sanitizeFilename(filename)
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "\\") || strings.HasPrefix(filename, ".") {
		return "", fmt.Errorf("invalid filename")
	}
	path := filepath.Join(localDir, filename)
	absLocal, err := filepath.Abs(localDir)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if absPath != absLocal && !strings.HasPrefix(absPath, absLocal+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid path")
	}
	return path, nil
}

func listLocalFileEntries() ([]map[string]interface{}, error) {
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return nil, err
	}
	var files []map[string]interface{}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, map[string]interface{}{
			"name": e.Name(),
			"path": targetDir + "/" + e.Name(),
			"size": info.Size(),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i]["name"].(string) < files[j]["name"].(string)
	})
	return files, nil
}

func countPendingSync() int {
	stateMu.Lock()
	defer stateMu.Unlock()

	pending := len(syncState.PendingDeletes)
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return pending
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, synced := syncState.GitSHAs[e.Name()]; !synced {
			pending++
		}
	}
	return pending
}

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func listFiles(w http.ResponseWriter, r *http.Request) {
	files, err := listLocalFileEntries()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

func downloadFile(w http.ResponseWriter, r *http.Request) {
	filename := sanitizeFilename(strings.TrimPrefix(r.URL.Path, "/api/file/"))
	if filename == "" {
		http.Error(w, "filename required", http.StatusBadRequest)
		return
	}
	path, err := localFilePath(filename)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".png"):
		w.Header().Set("Content-Type", "image/png")
	case strings.HasSuffix(lower, ".webp"):
		w.Header().Set("Content-Type", "image/webp")
	case strings.HasSuffix(lower, ".gif"):
		w.Header().Set("Content-Type", "image/gif")
	default:
		w.Header().Set("Content-Type", "image/jpeg")
	}
	w.Header().Set("Content-Disposition", "inline; filename="+filename)
	io.Copy(w, f)
}

func uploadFile(w http.ResponseWriter, r *http.Request) {
	err := r.ParseMultipartForm(25 << 20)
	if err != nil {
		http.Error(w, "can't parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, handler, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "file field missing", http.StatusBadRequest)
		return
	}
	defer file.Close()

	fileBytes, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	filename := sanitizeFilename(handler.Filename)
	if filename == "" {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}
	path, err := localFilePath(filename)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.WriteFile(path, fileBytes, 0644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	markFileChanged(filename)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "filename": filename})
}

func renameFile(w http.ResponseWriter, r *http.Request) {
	var req RenameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.OldName = sanitizeFilename(req.OldName)
	req.NewName = sanitizeFilename(req.NewName)
	if req.OldName == "" || req.NewName == "" {
		http.Error(w, "old_name and new_name are required", http.StatusBadRequest)
		return
	}
	oldPath, err := localFilePath(req.OldName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	newPath, err := localFilePath(req.NewName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(oldPath); err != nil {
		http.Error(w, "source file not found", http.StatusNotFound)
		return
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	markRenamed(req.OldName, req.NewName)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "renamed", "new_name": req.NewName})
}

func syncStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"pending": countPendingSync(),
		"has_token": githubToken != "",
	})
}

func fetchRemoteFiles() (map[string]string, error) {
	req, _ := http.NewRequest("GET", apiBaseURL, nil)
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp GitHubError
		json.NewDecoder(resp.Body).Decode(&errResp)
		return nil, fmt.Errorf(errResp.Message)
	}

	var contents []GitHubContent
	json.NewDecoder(resp.Body).Decode(&contents)

	remote := make(map[string]string)
	for _, c := range contents {
		if c.Type == "file" {
			remote[c.Name] = c.Sha
		}
	}
	return remote, nil
}

func githubPutFile(filename string, content []byte, sha string, message string) (string, error) {
	remotePath := targetDir + "/" + filename
	url := "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/contents/" + remotePath

	payload := map[string]interface{}{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
	}
	if sha != "" {
		payload["sha"] = sha
	}

	jsonPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PUT", url, bytes.NewBuffer(jsonPayload))
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var errResp GitHubError
		json.Unmarshal(body, &errResp)
		if errResp.Message != "" {
			return "", fmt.Errorf(errResp.Message)
		}
		return "", fmt.Errorf("github error: %s", resp.Status)
	}

	var result struct {
		Content struct {
			Sha string `json:"sha"`
		} `json:"content"`
	}
	json.Unmarshal(body, &result)
	return result.Content.Sha, nil
}

func githubDeleteFile(filename, sha, message string) error {
	remotePath := targetDir + "/" + filename
	url := "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/contents/" + remotePath

	payload := map[string]interface{}{
		"message": message,
		"sha":     sha,
	}
	jsonPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequest("DELETE", url, bytes.NewBuffer(jsonPayload))
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		var errResp GitHubError
		json.Unmarshal(body, &errResp)
		if errResp.Message != "" {
			return fmt.Errorf(errResp.Message)
		}
		return fmt.Errorf("github delete error: %s", resp.Status)
	}
	return nil
}

func syncToGit(w http.ResponseWriter, r *http.Request) {
	if githubToken == "" {
		http.Error(w, "GHTOKEN не задан", http.StatusServiceUnavailable)
		return
	}

	result := SyncResult{}
	remote, err := fetchRemoteFiles()
	if err != nil {
		http.Error(w, "не удалось получить список с GitHub: "+err.Error(), http.StatusBadGateway)
		return
	}

	localEntries, err := listLocalFileEntries()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	localNames := make(map[string]bool)
	for _, f := range localEntries {
		localNames[f["name"].(string)] = true
	}

	stateMu.Lock()
	defer stateMu.Unlock()

	// Удаления из очереди и осиротевшие файлы на GitHub
	deletes := append([]string{}, syncState.PendingDeletes...)
	for name := range remote {
		if !localNames[name] {
			deletes = append(deletes, name)
		}
	}
	deletes = uniqueStrings(deletes)

	for _, name := range deletes {
		sha, ok := remote[name]
		if !ok {
			removeFromPendingDeletesLocked(name)
			delete(syncState.GitSHAs, name)
			continue
		}
		if err := githubDeleteFile(name, sha, "Sync delete "+name); err != nil {
			result.Errors = append(result.Errors, "delete "+name+": "+err.Error())
			continue
		}
		result.Deleted++
		delete(remote, name)
		removeFromPendingDeletesLocked(name)
		delete(syncState.GitSHAs, name)
	}

	// Загрузка новых и изменённых
	for _, f := range localEntries {
		name := f["name"].(string)
		if _, synced := syncState.GitSHAs[name]; synced {
			continue
		}

		path, _ := localFilePath(name)
		content, err := os.ReadFile(path)
		if err != nil {
			result.Errors = append(result.Errors, "read "+name+": "+err.Error())
			continue
		}

		remoteSha := remote[name]
		msg := "Sync upload " + name
		if remoteSha != "" {
			msg = "Sync update " + name
		}

		newSha, err := githubPutFile(name, content, remoteSha, msg)
		if err != nil {
			result.Errors = append(result.Errors, "upload "+name+": "+err.Error())
			continue
		}

		if remoteSha == "" {
			result.Uploaded++
		} else {
			result.Updated++
		}
		syncState.GitSHAs[name] = newSha
		remote[name] = newSha
	}

	saveSyncStateLocked()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func removeFromPendingDeletesLocked(name string) {
	var kept []string
	for _, d := range syncState.PendingDeletes {
		if d != name {
			kept = append(kept, d)
		}
	}
	syncState.PendingDeletes = kept
}

func uniqueStrings(items []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range items {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func main() {
	http.HandleFunc("/api/files", corsMiddleware(listFiles))
	http.HandleFunc("/api/file/", corsMiddleware(downloadFile))
	http.HandleFunc("/api/upload", corsMiddleware(uploadFile))
	http.HandleFunc("/api/rename", corsMiddleware(renameFile))
	http.HandleFunc("/api/sync/status", corsMiddleware(syncStatus))
	http.HandleFunc("/api/sync", corsMiddleware(syncToGit))

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)

	port := "8080"
	log.Printf("🌐 Сервер запущен на http://localhost:%s (локально: %s)", port, localDir)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
