package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"fmt"

	"github.com/joho/godotenv"
)

type GitHubContent struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Sha         string `json:"sha"`
	Size        int    `json:"size"`
	DownloadURL string `json:"download_url"`
	Type        string `json:"type"`
}

type GitHubError struct {
	Message string `json:"message"`
}

type RenameRequest struct {
	OldName string `json:"old_name"`
	NewName string `json:"new_name"`
}

var (
	githubToken string
	repoOwner   = "goshva"
	repoName    = "moneyTracker"
    targetDir = "photos"
	apiBaseURL  = "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/contents/" + targetDir
)

func init() {
	err := godotenv.Load()
	if err != nil {
		log.Println("⚠️ .env файл не найден, используется переменная окружения GHTOKEN")
	}
	githubToken = os.Getenv("GHTOKEN")
	if githubToken == "" {
		log.Fatal("❌ GHTOKEN не задан. Укажите его в .env или в окружении")
	}
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

// GET /api/files – список файлов в targetDir
func listFiles(w http.ResponseWriter, r *http.Request) {
	url := apiBaseURL
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp GitHubError
		json.NewDecoder(resp.Body).Decode(&errResp)
		http.Error(w, errResp.Message, resp.StatusCode)
		return
	}

	var contents []GitHubContent
	json.NewDecoder(resp.Body).Decode(&contents)

	// фильтруем только файлы (type = "file")
	var files []map[string]interface{}
	for _, c := range contents {
		if c.Type == "file" {
			files = append(files, map[string]interface{}{
				"name": c.Name,
				"path": c.Path,
				"size": c.Size,
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(files)
}

// GET /api/file/{filename} – скачать файл (возвращает содержимое)
func downloadFile(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimPrefix(r.URL.Path, "/api/file/")
	if filename == "" {
		http.Error(w, "filename required", http.StatusBadRequest)
		return
	}
	filePath := targetDir + "/" + filename
	url := "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/contents/" + filePath

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/vnd.github.v3.raw")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, "file not found", resp.StatusCode)
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	io.Copy(w, resp.Body)
}

// POST /api/upload – загрузить новый файл
// вспомогательная функция для получения SHA файла (если существует)
func getFileSHA(filename string) (string, error) {
    filePath := targetDir + "/" + filename
    url := "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/contents/" + filePath
    req, _ := http.NewRequest("GET", url, nil)
    req.Header.Set("Authorization", "Bearer "+githubToken)
    req.Header.Set("Accept", "application/vnd.github.v3+json")
    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()
    if resp.StatusCode == http.StatusNotFound {
        return "", nil // файл не существует
    }
    if resp.StatusCode != http.StatusOK {
        return "", fmt.Errorf("failed to get file info: %s", resp.Status)
    }
    var fileInfo struct {
        Sha string `json:"sha"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&fileInfo); err != nil {
        return "", err
    }
    return fileInfo.Sha, nil
}

// POST /api/upload – загрузить новый файл или обновить существующий
func uploadFile(w http.ResponseWriter, r *http.Request) {
    err := r.ParseMultipartForm(25 << 20) // 25 MB
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
    contentBase64 := base64.StdEncoding.EncodeToString(fileBytes)

    commitMsg := r.FormValue("message")
    if commitMsg == "" {
        commitMsg = "Upload " + handler.Filename
    }

    remotePath := targetDir + "/" + handler.Filename
    url := "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/contents/" + remotePath

    // Получаем SHA, если файл уже существует
    sha, err := getFileSHA(handler.Filename)
    if err != nil {
        http.Error(w, "failed to check file existence: "+err.Error(), http.StatusInternalServerError)
        return
    }

    payload := map[string]interface{}{
        "message": commitMsg,
        "content": contentBase64,
    }
    if sha != "" {
        payload["sha"] = sha
        // можно изменить сообщение коммита, чтобы было понятно, что это обновление
        payload["message"] = "Update " + handler.Filename
    }

    jsonPayload, _ := json.Marshal(payload)
    req, _ := http.NewRequest("PUT", url, bytes.NewBuffer(jsonPayload))
    req.Header.Set("Authorization", "Bearer "+githubToken)
    req.Header.Set("Content-Type", "application/json")

    client := &http.Client{}
    resp, err := client.Do(req)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
        var errResp GitHubError
        body, _ := io.ReadAll(resp.Body)
        log.Printf("GitHub API error: %s", string(body))
        json.Unmarshal(body, &errResp)
        http.Error(w, errResp.Message, resp.StatusCode)
        return
    }

    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(map[string]string{"status": "ok", "filename": handler.Filename})
}

// PUT /api/rename – переименовать файл (получить содержимое, удалить старый, создать новый)
// PUT /api/rename – переименовать файл
func renameFile(w http.ResponseWriter, r *http.Request) {
    var req RenameRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, "invalid request", http.StatusBadRequest)
        return
    }
    if req.OldName == "" || req.NewName == "" {
        http.Error(w, "old_name and new_name are required", http.StatusBadRequest)
        return
    }

    oldPath := targetDir + "/" + req.OldName
    newPath := targetDir + "/" + req.NewName

    // 1. Получить содержимое старого файла (и его SHA)
    url := "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/contents/" + oldPath
    client := &http.Client{}
    getReq, _ := http.NewRequest("GET", url, nil)
    getReq.Header.Set("Authorization", "Bearer "+githubToken)
    getReq.Header.Set("Accept", "application/vnd.github.v3+json")

    getResp, err := client.Do(getReq)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    defer getResp.Body.Close()
    if getResp.StatusCode != http.StatusOK {
        http.Error(w, "source file not found", http.StatusNotFound)
        return
    }

    var fileInfo struct {
        Sha     string `json:"sha"`
        Content string `json:"content"`
    }
    json.NewDecoder(getResp.Body).Decode(&fileInfo)
    contentBytes, err := base64.StdEncoding.DecodeString(fileInfo.Content)
    if err != nil {
        http.Error(w, "failed to decode content", http.StatusInternalServerError)
        return
    }
    newContentBase64 := base64.StdEncoding.EncodeToString(contentBytes)

    // 2. Проверить, существует ли уже файл с новым именем, и получить его SHA
    shaNew, _ := getFileSHA(req.NewName) // используем уже существующую функцию getFileSHA

    // 3. Создать (или обновить) новый файл
    createPayload := map[string]interface{}{
        "message": "Rename " + req.OldName + " to " + req.NewName,
        "content": newContentBase64,
    }
    if shaNew != "" {
        createPayload["sha"] = shaNew // если файл существует, передаём его SHA
    }
    jsonPayload, _ := json.Marshal(createPayload)
    createReq, _ := http.NewRequest("PUT", "https://api.github.com/repos/"+repoOwner+"/"+repoName+"/contents/"+newPath, bytes.NewBuffer(jsonPayload))
    createReq.Header.Set("Authorization", "Bearer "+githubToken)
    createReq.Header.Set("Content-Type", "application/json")

    createResp, err := client.Do(createReq)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    defer createResp.Body.Close()
    if createResp.StatusCode != http.StatusOK && createResp.StatusCode != http.StatusCreated {
        var errResp GitHubError
        json.NewDecoder(createResp.Body).Decode(&errResp)
        http.Error(w, "failed to create new file: "+errResp.Message, createResp.StatusCode)
        return
    }

    // 4. Удалить старый файл
    deletePayload := map[string]interface{}{
        "message": "Remove old file after rename",
        "sha":     fileInfo.Sha,
    }
    jsonDelPayload, _ := json.Marshal(deletePayload)
    deleteReq, _ := http.NewRequest("DELETE", "https://api.github.com/repos/"+repoOwner+"/"+repoName+"/contents/"+oldPath, bytes.NewBuffer(jsonDelPayload))
    deleteReq.Header.Set("Authorization", "Bearer "+githubToken)
    deleteReq.Header.Set("Content-Type", "application/json")

    deleteResp, err := client.Do(deleteReq)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    defer deleteResp.Body.Close()
    if deleteResp.StatusCode != http.StatusOK {
        var errResp GitHubError
        json.NewDecoder(deleteResp.Body).Decode(&errResp)
        http.Error(w, "file renamed but old file not deleted: "+errResp.Message, deleteResp.StatusCode)
        return
    }

    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(map[string]string{"status": "renamed", "new_name": req.NewName})
}

func main() {
	http.HandleFunc("/api/files", corsMiddleware(listFiles))
	http.HandleFunc("/api/file/", corsMiddleware(downloadFile))
	http.HandleFunc("/api/upload", corsMiddleware(uploadFile))
	http.HandleFunc("/api/rename", corsMiddleware(renameFile))

	// Статические файлы PWA
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)

	port := "8080"
	log.Printf("🌐 Сервер запущен на http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
