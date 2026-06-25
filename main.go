package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

	stateMu        sync.Mutex
	syncState      SyncState
	syncRunning    int32
	githubClient            = &http.Client{Timeout: 60 * time.Second}
	syncMaxTimeout          = 8 * time.Minute
	githubContentsAPIMaxSize = 1 * 1024 * 1024 // GitHub Contents API: лимит 1 MB
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

func fetchRemoteFiles(ctx context.Context) (map[string]string, error) {
	body, err := githubRequest(ctx, "GET", "/contents/"+encodeRepoPath(targetDir), nil)
	if err != nil {
		if strings.Contains(err.Error(), "Not Found") {
			log.Println("☁️ Папка photos/ на GitHub ещё не создана — считаем пустой")
			return make(map[string]string), nil
		}
		return nil, err
	}

	var contents []GitHubContent
	if err := json.Unmarshal(body, &contents); err != nil {
		var single GitHubContent
		if err2 := json.Unmarshal(body, &single); err2 != nil || single.Type != "file" {
			return nil, fmt.Errorf("неожиданный ответ GitHub")
		}
		return map[string]string{single.Name: single.Sha}, nil
	}

	remote := make(map[string]string)
	for _, c := range contents {
		if c.Type == "file" {
			remote[c.Name] = c.Sha
		}
	}
	return remote, nil
}

func githubRepoURL(apiPath string) string {
	return "https://api.github.com/repos/" + repoOwner + "/" + repoName + apiPath
}

func encodeRepoPath(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "/")
}

func githubRequest(ctx context.Context, method, apiPath string, payload interface{}) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		jsonPayload, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewBuffer(jsonPayload)
	}

	req, err := http.NewRequestWithContext(ctx, method, githubRepoURL(apiPath), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := githubClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errResp GitHubError
		json.Unmarshal(respBody, &errResp)
		if errResp.Message != "" {
			return nil, fmt.Errorf(errResp.Message)
		}
		return nil, fmt.Errorf("github error: %s", resp.Status)
	}
	return respBody, nil
}

func isValidImage(content []byte) bool {
	if len(content) < 12 {
		return false
	}
	if content[0] == 0xFF && content[1] == 0xD8 && content[2] == 0xFF {
		return true
	}
	if content[0] == 0x89 && content[1] == 0x50 && content[2] == 0x4E && content[3] == 0x47 {
		return true
	}
	return string(content[0:4]) == "RIFF" && string(content[8:12]) == "WEBP"
}

func githubPutFile(ctx context.Context, filename string, content []byte, sha string, message string) (string, error) {
	remotePath := targetDir + "/" + filename
	if len(content) > githubContentsAPIMaxSize {
		log.Printf("☁️ %s: %d байт — Git Data API (>1 MB)", filename, len(content))
		return githubPutFileViaGitAPI(ctx, remotePath, content, message)
	}
	return githubPutFileContents(ctx, remotePath, content, sha, message)
}

func githubPutFileContents(ctx context.Context, remotePath string, content []byte, sha string, message string) (string, error) {
	payload := map[string]interface{}{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
	}
	if sha != "" {
		payload["sha"] = sha
	}

	body, err := githubRequest(ctx, "PUT", "/contents/"+encodeRepoPath(remotePath), payload)
	if err != nil {
		return "", err
	}

	var result struct {
		Content struct {
			Sha string `json:"sha"`
		} `json:"content"`
	}
	json.Unmarshal(body, &result)
	return result.Content.Sha, nil
}

func getDefaultBranch(ctx context.Context) (string, error) {
	body, err := githubRequest(ctx, "GET", "", nil)
	if err != nil {
		return "", err
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &repo); err != nil {
		return "", err
	}
	if repo.DefaultBranch == "" {
		return "main", nil
	}
	return repo.DefaultBranch, nil
}

func getBranchCommitSHA(ctx context.Context, branch string) (string, error) {
	body, err := githubRequest(ctx, "GET", "/git/refs/heads/"+url.PathEscape(branch), nil)
	if err != nil {
		return "", err
	}
	var ref struct {
		Object struct {
			Sha string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &ref); err != nil {
		return "", err
	}
	return ref.Object.Sha, nil
}

func getCommitTreeSHA(ctx context.Context, commitSHA string) (string, error) {
	body, err := githubRequest(ctx, "GET", "/git/commits/"+commitSHA, nil)
	if err != nil {
		return "", err
	}
	var commit struct {
		Tree struct {
			Sha string `json:"sha"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(body, &commit); err != nil {
		return "", err
	}
	return commit.Tree.Sha, nil
}

func createGitBlob(ctx context.Context, content []byte) (string, error) {
	payload := map[string]string{
		"content":  base64.StdEncoding.EncodeToString(content),
		"encoding": "base64",
	}
	body, err := githubRequest(ctx, "POST", "/git/blobs", payload)
	if err != nil {
		return "", err
	}
	var blob struct {
		Sha string `json:"sha"`
	}
	if err := json.Unmarshal(body, &blob); err != nil {
		return "", err
	}
	return blob.Sha, nil
}

func createGitTree(ctx context.Context, baseTreeSHA, remotePath, blobSHA string) (string, error) {
	payload := map[string]interface{}{
		"base_tree": baseTreeSHA,
		"tree": []map[string]string{
			{
				"path": remotePath,
				"mode": "100644",
				"type": "blob",
				"sha":  blobSHA,
			},
		},
	}
	body, err := githubRequest(ctx, "POST", "/git/trees", payload)
	if err != nil {
		return "", err
	}
	var tree struct {
		Sha string `json:"sha"`
	}
	if err := json.Unmarshal(body, &tree); err != nil {
		return "", err
	}
	return tree.Sha, nil
}

func createGitCommit(ctx context.Context, message, treeSHA, parentSHA string) (string, error) {
	payload := map[string]interface{}{
		"message": message,
		"tree":    treeSHA,
		"parents": []string{parentSHA},
	}
	body, err := githubRequest(ctx, "POST", "/git/commits", payload)
	if err != nil {
		return "", err
	}
	var commit struct {
		Sha string `json:"sha"`
	}
	if err := json.Unmarshal(body, &commit); err != nil {
		return "", err
	}
	return commit.Sha, nil
}

func updateBranchRef(ctx context.Context, branch, commitSHA string) error {
	payload := map[string]interface{}{
		"sha":   commitSHA,
		"force": false,
	}
	_, err := githubRequest(ctx, "PATCH", "/git/refs/heads/"+url.PathEscape(branch), payload)
	return err
}

func githubPutFileViaGitAPI(ctx context.Context, remotePath string, content []byte, message string) (string, error) {
	blobSHA, err := createGitBlob(ctx, content)
	if err != nil {
		return "", fmt.Errorf("blob: %w", err)
	}

	branch, err := getDefaultBranch(ctx)
	if err != nil {
		return "", fmt.Errorf("branch: %w", err)
	}

	parentSHA, err := getBranchCommitSHA(ctx, branch)
	if err != nil {
		return "", fmt.Errorf("ref: %w", err)
	}

	baseTreeSHA, err := getCommitTreeSHA(ctx, parentSHA)
	if err != nil {
		return "", fmt.Errorf("tree: %w", err)
	}

	newTreeSHA, err := createGitTree(ctx, baseTreeSHA, remotePath, blobSHA)
	if err != nil {
		return "", fmt.Errorf("new tree: %w", err)
	}

	newCommitSHA, err := createGitCommit(ctx, message, newTreeSHA, parentSHA)
	if err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}

	if err := updateBranchRef(ctx, branch, newCommitSHA); err != nil {
		return "", fmt.Errorf("ref update: %w", err)
	}

	return blobSHA, nil
}

func githubDeleteFile(ctx context.Context, filename, sha, message string) error {
	remotePath := targetDir + "/" + filename
	payload := map[string]interface{}{
		"message": message,
		"sha":     sha,
	}
	_, err := githubRequest(ctx, "DELETE", "/contents/"+encodeRepoPath(remotePath), payload)
	return err
}

func isFileSynced(name string) bool {
	stateMu.Lock()
	defer stateMu.Unlock()
	_, synced := syncState.GitSHAs[name]
	return synced
}

func markFileSynced(name, sha string) {
	stateMu.Lock()
	defer stateMu.Unlock()
	syncState.GitSHAs[name] = sha
}

func markFileDeletedFromRemote(name string) {
	stateMu.Lock()
	defer stateMu.Unlock()
	removeFromPendingDeletesLocked(name)
	delete(syncState.GitSHAs, name)
}

func collectPendingDeletes() []string {
	stateMu.Lock()
	defer stateMu.Unlock()
	return uniqueStrings(append([]string{}, syncState.PendingDeletes...))
}

func syncToGit(w http.ResponseWriter, r *http.Request) {
	if githubToken == "" {
		http.Error(w, "GHTOKEN не задан", http.StatusServiceUnavailable)
		return
	}
	if !atomic.CompareAndSwapInt32(&syncRunning, 0, 1) {
		http.Error(w, "синхронизация уже выполняется", http.StatusConflict)
		return
	}
	defer atomic.StoreInt32(&syncRunning, 0)

	ctx, cancel := context.WithTimeout(r.Context(), syncMaxTimeout)
	defer cancel()

	result := SyncResult{}
	log.Println("☁️ Синхронизация с Git: старт")

	remote, err := fetchRemoteFiles(ctx)
	if err != nil {
		log.Printf("☁️ Ошибка списка GitHub: %v", err)
		http.Error(w, "не удалось получить список с GitHub: "+err.Error(), http.StatusBadGateway)
		return
	}

	localEntries, err := listLocalFileEntries()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	deletes := collectPendingDeletes()
	for _, name := range deletes {
		if err := ctx.Err(); err != nil {
			result.Errors = append(result.Errors, "timeout: "+err.Error())
			break
		}
		sha, ok := remote[name]
		if !ok {
			markFileDeletedFromRemote(name)
			continue
		}
		log.Printf("☁️ Удаление %s", name)
		if err := githubDeleteFile(ctx, name, sha, "Sync delete "+name); err != nil {
			result.Errors = append(result.Errors, "delete "+name+": "+err.Error())
			continue
		}
		result.Deleted++
		delete(remote, name)
		markFileDeletedFromRemote(name)
	}

	for _, f := range localEntries {
		if err := ctx.Err(); err != nil {
			result.Errors = append(result.Errors, "timeout: "+err.Error())
			break
		}
		name := f["name"].(string)
		if isFileSynced(name) {
			continue
		}

		path, _ := localFilePath(name)
		content, err := os.ReadFile(path)
		if err != nil {
			result.Errors = append(result.Errors, "read "+name+": "+err.Error())
			continue
		}
		if !isValidImage(content) {
			result.Errors = append(result.Errors, "upload "+name+": файл повреждён или не является изображением")
			continue
		}

		remoteSha := remote[name]
		msg := "Sync upload " + name
		if remoteSha != "" {
			msg = "Sync update " + name
		}

		log.Printf("☁️ Отправка %s", name)
		newSha, err := githubPutFile(ctx, name, content, remoteSha, msg)
		if err != nil {
			result.Errors = append(result.Errors, "upload "+name+": "+err.Error())
			continue
		}

		if remoteSha == "" {
			result.Uploaded++
		} else {
			result.Updated++
		}
		markFileSynced(name, newSha)
		remote[name] = newSha
	}

	stateMu.Lock()
	saveSyncStateLocked()
	stateMu.Unlock()

	log.Printf("☁️ Синхронизация завершена: +%d ~%d -%d ошибок %d", result.Uploaded, result.Updated, result.Deleted, len(result.Errors))
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
