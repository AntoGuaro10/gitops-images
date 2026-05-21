package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
)

type ServiceRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
	Port      int    `json:"port"`
}

func main() {
	if os.Getenv("GITHUB_TOKEN") == "" || os.Getenv("GITHUB_OWNER") == "" {
		log.Fatal("ERROR: env vars GITHUB_TOKEN and GITHUB_OWNER not set")
	}

	http.HandleFunc("/api/create", enableCORS(createGitOpsStructure))

	fmt.Println("Backend API correctly started on port 8080...")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func enableCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func createGitOpsStructure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Only POST method is accepted")
		return
	}

	var req ServiceRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid JSON data")
		return
	}

	token := os.Getenv("GITHUB_TOKEN")
	owner := os.Getenv("GITHUB_OWNER")
	centralRepo := "argocd-with-flux"

	// -------------------------------------------------------------------------
	// STEP 1: Repository creation
	// -------------------------------------------------------------------------
	fmt.Printf("1. Creating repository for service: %s\n", req.Name)
	createRepoPayload := map[string]interface{}{
		"name":         req.Name,
		"description":  fmt.Sprintf("Created repository for service %s", req.Name),
		"private":      true, // or false
		"auto_init":    true,
	}
	
	payloadBytes, _ := json.Marshal(createRepoPayload)
	createRepoURL := "https://api.github.com/regions/repos" // Base endpoint for user
	// Usiamo l'endpoint utente standard:
	createRepoURL = "https://api.github.com/user/repos"

	resp, err := sendGitHubRequest("POST", createRepoURL, token, payloadBytes)
	if err != nil || (resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK) {
		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("ERROR: Repository creation failed (Code %d). Verify the Token permissions.", statusCode))
		return
	}

	// -------------------------------------------------------------------------
	// STEP 2: Kubernetes manifests (Deployment + Service) creation
	// -------------------------------------------------------------------------
	fmt.Printf("2. Pushing k8s manifests to the new repository: %s\n", req.Name)
	appManifestsYAML := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      containers:
      - name: app
        image: %s
        ports:
        - containerPort: %d
        resources:
          limits:
            cpu: 100m
            memory: 512Mi
---
apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
  annotations:
      prometheus.io/scrape: 'true'
      prometheus.io/port:   '%d'
spec:
  ports:
  - port: %d
    targetPort: %d
  selector:
    app: %s
`, req.Name, req.Namespace, req.Name, req.Name, req.Image, req.Port, req.Name, req.Namespace, req.Port, req.Port, req.Port, req.Name)

	encodedAppManifests := base64.StdEncoding.EncodeToString([]byte(appManifestsYAML))
	pushManifestsPayload := map[string]string{
		"message": "feat: bootstrap kubernetes manifests with GitOps",
		"content": encodedAppManifests,
		"branch":  "main",
	}
	
	payloadBytes, _ = json.Marshal(pushManifestsPayload)
	manifestsURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/k8s/app.yaml", owner, req.Name)
	
	resp, err = sendGitHubRequest("PUT", manifestsURL, token, payloadBytes)
	if err != nil || (resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK) {
		respondWithError(w, http.StatusInternalServerError, "ERROR: Failed to write manifests to the new repository")
		return
	}

	// -------------------------------------------------------------------------
	// STEP 3: Application ArgoCD creation on argocd-with-flux
	// -------------------------------------------------------------------------
	fmt.Println("3. Registration of the application on ArgoCD via the central repository")
	
	targetRepoURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, req.Name)

	argoAppFieldsYAML := fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: %s
  namespace: argocd
spec:
  project: default
  source:
    repoURL: '%s'
    targetRevision: HEAD
    path: k8s
  destination:
    server: https://kubernetes.default.svc
    namespace: %s
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
    - CreateNamespace=true
`, req.Name, targetRepoURL, req.Namespace)

	encodedArgoApp := base64.StdEncoding.EncodeToString([]byte(argoAppFieldsYAML))
	argoPayload := map[string]string{
		"message": fmt.Sprintf("feat: register application %s via GitOps portal", req.Name),
		"content": encodedArgoApp,
		"branch":  "main",
	}

	payloadBytes, _ = json.Marshal(argoPayload)
	centralArgoURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/apps/%s.yaml", owner, centralRepo, req.Name)

	resp, err = sendGitHubRequest("PUT", centralArgoURL, token, payloadBytes)
	if err != nil || (resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK) {
		respondWithError(w, http.StatusInternalServerError, "ERROR: Failed to register the Application in the central ArgoCD repository")
		return
	}

	// All set
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"message": fmt.Sprintf("Microservice successfully created! Repository dedicated: %s, registered in %s/apps/%s.yaml", targetRepoURL, centralRepo, req.Name),
	})
}

// Funzione ausiliaria per evitare di ripetere il codice delle chiamate HTTP a GitHub
func sendGitHubRequest(method, url string, token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := &http.Client{}
	return client.Do(req)
}

func respondWithError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}